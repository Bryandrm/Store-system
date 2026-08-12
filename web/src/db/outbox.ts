/**
 * The operation outbox.
 *
 * Every mutation the user makes is recorded here before it is sent anywhere.
 * The queue is the reason a sale rung up with no signal is not lost: it is real
 * business data sitting in durable storage, not a pending HTTP request.
 */
import type { OperationType, OutboxEntry, OutboxStatus } from '@/domain/types'
import { getDB } from './schema'

/** Exponential backoff with full jitter, capped at 15 minutes. */
const BASE_BACKOFF_MS = 2_000
const MAX_BACKOFF_MS = 15 * 60_000

/**
 * Acknowledged operations are kept for 30 days rather than deleted.
 *
 * The cost is a few hundred kilobytes. The benefit is the cheapest disaster
 * recovery in the system: if the server is ever restored from last night's
 * dump, every device can re-push 30 days of operations, and idempotency makes
 * that safe and duplicate-free. It turns "lose a day" into "lose nothing".
 */
const SYNCED_RETENTION_MS = 30 * 24 * 60 * 60_000

/** Warn the user when the oldest pending operation is older than this. */
export const STALE_PENDING_MS = 24 * 60 * 60_000

export interface EnqueueInput {
  op_id: string
  type: OperationType
  payload: unknown
  occurred_at: string
  entity_ids: string[]
}

/** Adds an operation to the queue. */
export async function enqueue(input: EnqueueInput): Promise<void> {
  const db = await getDB()
  const now = new Date().toISOString()

  await db.put('outbox', {
    op_id: input.op_id,
    type: input.type,
    payload: input.payload,
    occurred_at: input.occurred_at,
    created_at: now,
    status: 'pending',
    attempts: 0,
    next_attempt_at: now,
    last_error_code: null,
    last_error_message: null,
    entity_ids: input.entity_ids,
    synced_at: null,
  })
}

/**
 * Returns the operations that are due to be sent, oldest first.
 *
 * Ordering comes from the op_id being a UUIDv7, which sorts by creation time.
 * That is causal order on this device, and this device's causality is the only
 * one that exists: no ordering is implied between two phones.
 */
export async function claimBatch(limit = 50): Promise<OutboxEntry[]> {
  const db = await getDB()
  const now = Date.now()

  const due: OutboxEntry[] = []
  let cursor = await db
    .transaction('outbox', 'readwrite')
    .store.index('by_status_opid')
    .openCursor(IDBKeyRange.bound(['pending', ''], ['pending', '￿']))

  while (cursor && due.length < limit) {
    const entry = cursor.value
    if (Date.parse(entry.next_attempt_at) <= now) {
      const claimed: OutboxEntry = { ...entry, status: 'inflight' }
      await cursor.update(claimed)
      due.push(claimed)
    }
    cursor = await cursor.continue()
  }

  return due
}

/** Marks an operation as accepted by the server. */
export async function ack(opId: string): Promise<void> {
  const db = await getDB()
  const entry = await db.get('outbox', opId)
  if (!entry) return

  await db.put('outbox', {
    ...entry,
    status: 'synced',
    synced_at: new Date().toISOString(),
    last_error_code: null,
    last_error_message: null,
  })
}

/**
 * Records a transient failure and schedules a retry.
 *
 * There is deliberately NO attempt cap. A device that was offline for five days
 * is still holding real sales; giving up on them would be the worst possible
 * behaviour. Only a permanent rejection stops the retries.
 */
export async function retryLater(opId: string, errorCode: string, message: string): Promise<void> {
  const db = await getDB()
  const entry = await db.get('outbox', opId)
  if (!entry) return

  const attempts = entry.attempts + 1
  await db.put('outbox', {
    ...entry,
    status: 'pending',
    attempts,
    next_attempt_at: new Date(Date.now() + backoffMs(attempts)).toISOString(),
    last_error_code: errorCode,
    last_error_message: message,
  })
}

/**
 * Marks an operation as permanently rejected. It moves to the error tray.
 *
 * A failed entry is never mutated afterwards. Correcting it means enqueuing a
 * NEW operation with a new op_id, because editing this one in place would break
 * the first-write-wins guarantee the server relies on.
 */
export async function fail(opId: string, errorCode: string, message: string): Promise<void> {
  const db = await getDB()
  const entry = await db.get('outbox', opId)
  if (!entry) return

  await db.put('outbox', {
    ...entry,
    status: 'failed',
    last_error_code: errorCode,
    last_error_message: message,
  })
}

/**
 * Returns claimed-but-unsent operations to the queue.
 *
 * Called when a request dies mid-flight, and on startup: an entry left
 * 'inflight' by a tab that was killed would otherwise never be retried, which
 * is exactly the scenario iOS creates when it reclaims a backgrounded web app.
 */
export async function releaseInflight(): Promise<void> {
  const db = await getDB()
  const tx = db.transaction('outbox', 'readwrite')
  let cursor = await tx.store.openCursor()

  while (cursor) {
    if (cursor.value.status === 'inflight') {
      await cursor.update({ ...cursor.value, status: 'pending' })
    }
    cursor = await cursor.continue()
  }
  await tx.done
}

export async function countByStatus(status: OutboxStatus): Promise<number> {
  const db = await getDB()
  return db.countFromIndex(
    'outbox',
    'by_status_opid',
    IDBKeyRange.bound([status, ''], [status, '￿']),
  )
}

/** The oldest unsent operation, used to decide whether to warn the user. */
export async function oldestPending(): Promise<OutboxEntry | undefined> {
  const db = await getDB()
  const cursor = await db
    .transaction('outbox')
    .store.index('by_status_opid')
    .openCursor(IDBKeyRange.bound(['pending', ''], ['pending', '￿']))
  return cursor?.value
}

/**
 * True when something has been waiting long enough to be worth surfacing.
 *
 * This is the warning that catches the one genuinely unrecoverable scenario on
 * iOS: storage evicted while operations were still pending. Everything already
 * acknowledged lives on the server; only these would be lost.
 */
export async function hasStalePending(now = Date.now()): Promise<boolean> {
  const oldest = await oldestPending()
  if (!oldest) return false
  return now - Date.parse(oldest.created_at) > STALE_PENDING_MS
}

export async function listFailed(): Promise<OutboxEntry[]> {
  const db = await getDB()
  return db.getAllFromIndex(
    'outbox',
    'by_status_opid',
    IDBKeyRange.bound(['failed', ''], ['failed', '￿']),
  )
}

/** Discards a permanently failed operation, at the user's explicit request. */
export async function discard(opId: string): Promise<void> {
  const db = await getDB()
  const entry = await db.get('outbox', opId)
  if (entry?.status !== 'failed') return
  await db.delete('outbox', opId)
}

/** Drops acknowledged operations older than the retention window. */
export async function prune(now = Date.now()): Promise<number> {
  const db = await getDB()
  const tx = db.transaction('outbox', 'readwrite')
  let cursor = await tx.store.openCursor()
  let removed = 0

  while (cursor) {
    const entry = cursor.value
    if (entry.status === 'synced' && entry.synced_at) {
      if (now - Date.parse(entry.synced_at) > SYNCED_RETENTION_MS) {
        await cursor.delete()
        removed++
      }
    }
    cursor = await cursor.continue()
  }

  await tx.done
  return removed
}

/**
 * Re-queues every acknowledged operation still within the retention window.
 *
 * The disaster-recovery escape hatch: if the server is restored from a backup,
 * this replays everything since the dump. Idempotency makes it duplicate-free.
 * Deliberately not exposed in the normal UI.
 */
export async function resendAll(): Promise<number> {
  const db = await getDB()
  const tx = db.transaction('outbox', 'readwrite')
  let cursor = await tx.store.openCursor()
  let requeued = 0

  while (cursor) {
    if (cursor.value.status === 'synced') {
      await cursor.update({
        ...cursor.value,
        status: 'pending',
        attempts: 0,
        next_attempt_at: new Date().toISOString(),
        synced_at: null,
      })
      requeued++
    }
    cursor = await cursor.continue()
  }

  await tx.done
  return requeued
}

/** Exponential backoff with full jitter. */
export function backoffMs(attempts: number, random = Math.random): number {
  const ceiling = Math.min(BASE_BACKOFF_MS * 2 ** Math.max(0, attempts - 1), MAX_BACKOFF_MS)
  // Full jitter rather than a fixed delay: without it, several devices that
  // went offline together come back and hit the server in lockstep.
  return Math.floor(random() * ceiling)
}
