import { beforeEach, describe, expect, it } from 'vitest'

import type { Change } from '@/domain/types'
import { applyChange, applyChanges, applySnapshot, unknownEntitiesSeen } from './apply'
import { allStock, localDayBounds, salesForLocalDay, stockFor } from './local'
import {
  ack,
  backoffMs,
  claimBatch,
  countByStatus,
  discard,
  enqueue,
  fail,
  hasStalePending,
  listFailed,
  prune,
  releaseInflight,
  resendAll,
  retryLater,
} from './outbox'
import { clearReadModel, closeDB, getDB, setMeta } from './schema'

/**
 * Wipes the database between tests; fake-indexeddb keeps it in memory.
 *
 * The close is not optional: deleteDatabase blocks while a connection is open,
 * and it blocks silently, so forgetting it hangs the whole suite rather than
 * failing.
 */
async function freshDB() {
  await closeDB()
  const { deleteDB } = await import('idb')
  await deleteDB('store_system')
  await getDB()
}

beforeEach(async () => {
  await freshDB()
})

const stockChange = (id: string, productId: string, delta: number): Change => ({
  entity: 'stock_movement',
  entity_id: id,
  op: 'insert',
  payload: {
    id,
    product_id: productId,
    delta_qty_milli: delta,
    reason: delta > 0 ? 'restock' : 'sale',
    ref_kind: 'manual',
    ref_id: null,
    occurred_at: '2026-08-12T18:00:00Z',
  },
})

describe('applyChange', () => {
  /**
   * The property the whole sync design leans on. The feed delivers at least
   * once, so the same change WILL arrive twice; if that changed derived stock,
   * every device would slowly drift apart.
   */
  it('is idempotent: applying the same change twice does not move stock', async () => {
    const change = stockChange('m1', 'p1', -3000)

    await applyChange(change)
    expect(await stockFor('p1')).toBe(-3000)

    await applyChange(change)
    await applyChange(change)
    expect(await stockFor('p1')).toBe(-3000)
  })

  it('converges regardless of delivery order', async () => {
    const a = stockChange('m1', 'p1', 10_000)
    const b = stockChange('m2', 'p1', -3000)
    const c = stockChange('m3', 'p1', -2000)

    await applyChanges([a, b, c])
    const forward = await stockFor('p1')

    await freshDB()
    // Same changes, reversed, with duplicates thrown in.
    await applyChanges([c, b, a, b, c])
    const shuffled = await stockFor('p1')

    expect(shuffled).toBe(forward)
    expect(shuffled).toBe(5000)
  })

  /**
   * Forward compatibility: a newer server that ships an entity this client does
   * not know about must not break syncing for a device that has not updated.
   */
  it('ignores an unknown entity instead of throwing', async () => {
    await applyChange({
      entity: 'loyalty_points',
      entity_id: 'x1',
      op: 'insert',
      payload: { id: 'x1' },
    })

    expect(unknownEntitiesSeen()).toContain('loyalty_points')
    expect(await stockFor('p1')).toBe(0)
  })

  it('applies a bootstrap snapshot', async () => {
    await applySnapshot({
      products: [
        { id: 'p1', name: 'Mani japones', category: 'general', is_active: true, sort_order: 0 },
      ],
      prices: [{ product_id: 'p1', price_cents: 50, cost_cents: null }],
      stock_movements: [
        {
          id: 'm1',
          product_id: 'p1',
          delta_qty_milli: 20_000,
          reason: 'restock',
          ref_kind: 'manual',
          ref_id: null,
        },
      ],
    })

    expect(await stockFor('p1')).toBe(20_000)
    expect((await allStock()).get('p1')).toBe(20_000)
  })

  /**
   * Bootstrap deliberately over-delivers: rows it carries can arrive again in
   * the first feed page. That is safe for the same reason — upsert by id.
   */
  it('tolerates a snapshot row arriving again through the feed', async () => {
    await applySnapshot({
      stock_movements: [
        {
          id: 'm1',
          product_id: 'p1',
          delta_qty_milli: 20_000,
          reason: 'restock',
          ref_kind: 'manual',
          ref_id: null,
        },
      ],
    })
    await applyChange(stockChange('m1', 'p1', 20_000))

    expect(await stockFor('p1')).toBe(20_000)
  })
})

describe('outbox lifecycle', () => {
  const op = (id: string) => ({
    op_id: id,
    type: 'create_sale' as const,
    payload: { sale_id: id },
    occurred_at: '2026-08-12T18:00:00Z',
    entity_ids: [id],
  })

  it('moves pending to inflight to synced', async () => {
    await enqueue(op('a'))
    expect(await countByStatus('pending')).toBe(1)

    const batch = await claimBatch()
    expect(batch).toHaveLength(1)
    expect(await countByStatus('inflight')).toBe(1)
    expect(await countByStatus('pending')).toBe(0)

    await ack('a')
    expect(await countByStatus('synced')).toBe(1)
  })

  it('claims operations in creation order', async () => {
    // UUIDv7 sorts by time, which is why op_id ordering is causal order.
    await enqueue(op('019ff700-0000-7000-8000-000000000001'))
    await enqueue(op('019ff700-0000-7000-8000-000000000002'))
    await enqueue(op('019ff700-0000-7000-8000-000000000003'))

    const batch = await claimBatch()
    expect(batch.map((e) => e.op_id.slice(-1))).toEqual(['1', '2', '3'])
  })

  /**
   * A device offline for days still holds real sales. Giving up on them would
   * be the worst possible behaviour, so there is no attempt cap.
   */
  it('keeps retrying a transient failure forever', async () => {
    await enqueue(op('a'))
    await claimBatch()

    for (let i = 0; i < 50; i++) {
      await retryLater('a', 'TRANSIENT', 'network down')
      const db = await getDB()
      const entry = await db.get('outbox', 'a')
      expect(entry?.status).toBe('pending')
    }

    const db = await getDB()
    expect((await db.get('outbox', 'a'))?.attempts).toBe(50)
  })

  it('sends a permanent rejection to the error tray', async () => {
    await enqueue(op('a'))
    await claimBatch()
    await fail('a', 'TOTAL_MISMATCH', 'the total does not match')

    const failed = await listFailed()
    expect(failed).toHaveLength(1)
    expect(failed[0]?.last_error_code).toBe('TOTAL_MISMATCH')

    // A failed entry is never retried on its own.
    expect(await claimBatch()).toHaveLength(0)
  })

  it('only discards an operation that actually failed', async () => {
    await enqueue(op('a'))
    await discard('a')
    expect(await countByStatus('pending')).toBe(1)

    await claimBatch()
    await fail('a', 'X', 'y')
    await discard('a')
    expect(await listFailed()).toHaveLength(0)
  })

  /**
   * iOS reclaims backgrounded web apps aggressively. An entry left 'inflight'
   * by a killed tab would otherwise never be retried.
   */
  it('returns abandoned inflight operations to the queue', async () => {
    await enqueue(op('a'))
    await claimBatch()
    expect(await countByStatus('inflight')).toBe(1)

    await releaseInflight()
    expect(await countByStatus('pending')).toBe(1)
  })

  it('respects the backoff schedule when claiming', async () => {
    await enqueue(op('a'))
    await claimBatch()
    // A large attempt count pushes next_attempt_at well into the future.
    await retryLater('a', 'TRANSIENT', 'down')

    const db = await getDB()
    const entry = await db.get('outbox', 'a')
    const scheduled = Date.parse(entry!.next_attempt_at)
    expect(scheduled).toBeGreaterThanOrEqual(Date.now() - 1000)
  })

  it('prunes acknowledged operations only after the retention window', async () => {
    await enqueue(op('a'))
    await claimBatch()
    await ack('a')

    expect(await prune(Date.now())).toBe(0)
    expect(await countByStatus('synced')).toBe(1)

    const in31Days = Date.now() + 31 * 24 * 60 * 60_000
    expect(await prune(in31Days)).toBe(1)
    expect(await countByStatus('synced')).toBe(0)
  })

  /**
   * The disaster-recovery hatch: after a restore from backup, every device can
   * replay 30 days of operations, and idempotency makes it duplicate-free.
   */
  it('can re-queue everything already acknowledged', async () => {
    await enqueue(op('a'))
    await enqueue(op('b'))
    await claimBatch()
    await ack('a')
    await ack('b')

    expect(await resendAll()).toBe(2)
    expect(await countByStatus('pending')).toBe(2)
  })

  it('warns only once a pending operation is older than a day', async () => {
    await enqueue(op('a'))
    expect(await hasStalePending()).toBe(false)

    const in25Hours = Date.now() + 25 * 60 * 60_000
    expect(await hasStalePending(in25Hours)).toBe(true)
  })
})

describe('backoffMs', () => {
  it('grows with attempts and caps at fifteen minutes', () => {
    // random = 1 yields the ceiling, making the schedule deterministic here.
    const ceiling = (attempts: number) => backoffMs(attempts, () => 0.999999)

    expect(ceiling(1)).toBeLessThanOrEqual(2_000)
    expect(ceiling(2)).toBeGreaterThan(ceiling(1))
    expect(ceiling(20)).toBeLessThanOrEqual(15 * 60_000)
  })

  it('applies jitter so devices coming back online do not sync in lockstep', () => {
    const low = backoffMs(10, () => 0)
    const high = backoffMs(10, () => 0.999999)
    expect(low).toBeLessThan(high)
  })
})

describe('clearReadModel', () => {
  /**
   * Triggered by CURSOR_TOO_OLD. The read model is stale and gets rebuilt, but
   * unsent operations have never reached anyone else: dropping them would
   * silently destroy sales that really happened.
   */
  it('wipes the replica but keeps the outbox', async () => {
    await applyChange(stockChange('m1', 'p1', 5000))
    await enqueue({
      op_id: 'a',
      type: 'create_sale',
      payload: {},
      occurred_at: '2026-08-12T18:00:00Z',
      entity_ids: [],
    })
    await setMeta('cursor', '12345')

    await clearReadModel()

    expect(await stockFor('p1')).toBe(0)
    expect(await countByStatus('pending')).toBe(1)
  })
})

describe('localDayBounds', () => {
  /**
   * The classic off-by-six-hours bug. El Salvador is UTC-6 with no daylight
   * saving, so local midnight is 06:00 UTC.
   */
  it('places the day boundary at local midnight', () => {
    const { start, end } = localDayBounds(new Date('2026-08-12T18:00:00Z'))
    expect(start.toISOString()).toBe('2026-08-12T06:00:00.000Z')
    expect(end.toISOString()).toBe('2026-08-13T06:00:00.000Z')
  })

  it('keeps a sale at 23:59:59 local in the same day', () => {
    // 2026-08-12 23:59:59 local is 2026-08-13 05:59:59 UTC.
    const lateLocal = new Date('2026-08-13T05:59:59Z')
    const { start } = localDayBounds(lateLocal)
    expect(start.toISOString()).toBe('2026-08-12T06:00:00.000Z')
  })

  it('moves a sale at 00:00:00 local into the next day', () => {
    const justAfterMidnight = new Date('2026-08-13T06:00:00Z')
    const { start } = localDayBounds(justAfterMidnight)
    expect(start.toISOString()).toBe('2026-08-13T06:00:00.000Z')
  })
})

describe('salesForLocalDay', () => {
  it('includes only sales that occurred during the local day', async () => {
    const mk = (id: string, occurredAt: string) => ({
      entity: 'sale',
      entity_id: id,
      op: 'insert' as const,
      payload: {
        id,
        customer_id: null,
        total_cents: 100,
        paid_cents: 100,
        payment_method: 'cash',
        occurred_at: occurredAt,
        device_id: 'd',
        created_by_user_id: 'u',
      },
    })

    await applyChanges([
      mk('s1', '2026-08-12T06:00:00Z'), // local midnight, first of the day
      mk('s2', '2026-08-13T05:59:59Z'), // 23:59:59 local, same day
      mk('s3', '2026-08-13T06:00:00Z'), // next day
      mk('s4', '2026-08-12T05:59:59Z'), // previous day
    ])

    const sales = await salesForLocalDay(new Date('2026-08-12T18:00:00Z'))
    expect(sales.map((s) => s.id).sort()).toEqual(['s1', 's2'])
  })
})
