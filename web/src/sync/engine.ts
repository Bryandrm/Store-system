/**
 * The synchronization engine.
 *
 * One cycle: send whatever the outbox holds, then pull whatever changed since
 * the cursor. Both directions in one round trip, because a phone on mobile data
 * pays for each one.
 *
 * The engine never blocks selling. Every failure mode either retries quietly or
 * pauses the loop, and in both cases the user goes on ringing up sales.
 */
import { applyChanges, applySnapshot } from '@/db/apply'
import { clearReadModel, getMeta, setMeta } from '@/db/schema'
import {
  ack,
  claimBatch,
  countByStatus,
  fail,
  hasStalePending,
  prune,
  releaseInflight,
  retryLater,
} from '@/db/outbox'
import { ApiClient, ApiError } from './client'

/** What the UI shows in the status chip. Five states, no more. */
export type SyncState =
  | 'idle'
  | 'syncing'
  | 'offline'
  | 'session_expired'
  | 'error'

export interface SyncStatus {
  state: SyncState
  pendingCount: number
  failedCount: number
  lastSyncAt: string | null
  /** True when something has been waiting more than a day. See outbox.ts. */
  stalePending: boolean
  clockOffsetMs: number
}

export type SyncListener = (status: SyncStatus) => void

/** Poll while the app is in the foreground. */
const FOREGROUND_INTERVAL_MS = 60_000
/** Debounce after a local mutation, so ringing up three items sends once. */
const MUTATION_DEBOUNCE_MS = 1_000
/** Beyond this the user's clock is wrong enough to distort reports. */
const CLOCK_SKEW_WARN_MS = 5 * 60_000

export class SyncEngine {
  private status: SyncStatus = {
    state: 'idle',
    pendingCount: 0,
    failedCount: 0,
    lastSyncAt: null,
    stalePending: false,
    clockOffsetMs: 0,
  }

  private listeners = new Set<SyncListener>()
  private intervalId: ReturnType<typeof setInterval> | null = null
  private debounceId: ReturnType<typeof setTimeout> | null = null
  private running = false
  private stopped = true

  constructor(
    private readonly client: ApiClient,
    private readonly deviceId: string,
  ) {}

  subscribe(listener: SyncListener): () => void {
    this.listeners.add(listener)
    listener(this.status)
    return () => this.listeners.delete(listener)
  }

  getStatus(): SyncStatus {
    return this.status
  }

  /** Starts the loop and wires the triggers. */
  async start(): Promise<void> {
    this.stopped = false

    // Anything left 'inflight' belongs to a tab that died mid-request. On iOS
    // that is routine: the OS reclaims backgrounded web apps aggressively.
    await releaseInflight()
    await this.requestPersistentStorage()
    await this.refreshCounts()

    if (typeof window !== 'undefined') {
      window.addEventListener('online', this.onOnline)
      window.addEventListener('offline', this.onOffline)
      document.addEventListener('visibilitychange', this.onVisibilityChange)
    }

    this.intervalId = setInterval(() => void this.runCycle(), FOREGROUND_INTERVAL_MS)
    void this.runCycle()
  }

  stop(): void {
    this.stopped = true
    if (typeof window !== 'undefined') {
      window.removeEventListener('online', this.onOnline)
      window.removeEventListener('offline', this.onOffline)
      document.removeEventListener('visibilitychange', this.onVisibilityChange)
    }
    if (this.intervalId) clearInterval(this.intervalId)
    if (this.debounceId) clearTimeout(this.debounceId)
    this.intervalId = null
    this.debounceId = null
  }

  /** Called after a local mutation. Debounced so a burst sends once. */
  nudge(): void {
    if (this.debounceId) clearTimeout(this.debounceId)
    this.debounceId = setTimeout(() => void this.runCycle(), MUTATION_DEBOUNCE_MS)
  }

  private onOnline = () => void this.runCycle()
  private onOffline = () => this.update({ state: 'offline' })
  private onVisibilityChange = () => {
    if (document.visibilityState === 'visible') void this.runCycle()
  }

  /**
   * Runs one cycle, guarded so only one runs at a time across the whole origin.
   *
   * Web Locks is the important part: two open tabs would otherwise both claim
   * the same outbox entries and send them twice. Idempotency makes that
   * harmless on the server, but it doubles traffic and produces confusing
   * duplicate results locally.
   */
  async runCycle(): Promise<void> {
    if (this.stopped || this.running) return

    if (typeof navigator !== 'undefined' && 'locks' in navigator) {
      await navigator.locks.request('store-system-sync', { ifAvailable: true }, async (lock) => {
        if (!lock) return // another tab holds it
        await this.cycleBody()
      })
      return
    }

    // No Web Locks (older WebKit). The in-process guard still prevents
    // overlapping cycles within this tab.
    await this.cycleBody()
  }

  private async cycleBody(): Promise<void> {
    this.running = true
    this.update({ state: 'syncing' })

    try {
      const batch = await claimBatch()
      const cursor = (await getMeta<string>('cursor')) ?? '0'

      const response = await this.client.sync(
        this.deviceId,
        cursor,
        batch.map((e) => ({ op_id: e.op_id, type: e.type, payload: e.payload })),
      )

      // Resolve each operation before applying changes, so a crash in between
      // leaves entries pending rather than acknowledged-but-unapplied.
      for (const result of response.results) {
        switch (result.status) {
          case 'applied':
          case 'duplicate':
            // A duplicate is treated exactly as applied: the server already has
            // it, which is the only thing the client cares about.
            await ack(result.op_id)
            break
          case 'rejected':
            await fail(result.op_id, result.error_code ?? 'REJECTED', result.message ?? '')
            break
          case 'retry':
            await retryLater(result.op_id, result.error_code ?? 'TRANSIENT', result.message ?? '')
            break
        }
      }

      // Any entry the server said nothing about (the batch was cut short by a
      // transient failure) goes back to pending rather than staying inflight.
      await releaseInflight()

      await applyChanges(response.changes)
      await setMeta('cursor', response.cursor)

      const now = new Date().toISOString()
      await setMeta('last_sync_at', now)
      this.trackClockOffset(response.server_time)

      await prune()
      await this.refreshCounts()
      this.update({ state: 'idle', lastSyncAt: now })

      // has_more means the server truncated the page at a transaction boundary.
      // Keep going immediately rather than waiting for the next interval.
      if (response.has_more) {
        this.running = false
        await this.cycleBody()
        return
      }
    } catch (error) {
      await this.handleFailure(error)
    } finally {
      this.running = false
    }
  }

  private async handleFailure(error: unknown): Promise<void> {
    if (!(error instanceof ApiError)) {
      await releaseInflight()
      await this.refreshCounts()
      this.update({ state: 'error' })
      return
    }

    switch (error.kind) {
      case 'transient':
        // Nothing to record: the entries return to pending and the next tick
        // tries again. This is the normal case with no signal.
        await releaseInflight()
        await this.refreshCounts()
        this.update({ state: 'offline' })
        break

      case 'auth':
        // The outbox is NOT touched. Operations are business data, not HTTP
        // requests, and selling has to keep working while the user re-logs in.
        await releaseInflight()
        await this.refreshCounts()
        this.update({ state: 'session_expired' })
        break

      case 'stale_cursor':
        // The device fell behind the retention window. Rebuild the read model
        // from scratch, keeping every unsent operation.
        await this.rebootstrap()
        break

      case 'permanent':
        await releaseInflight()
        await this.refreshCounts()
        this.update({ state: 'error' })
        break
    }
  }

  /** Drops the read model and rebuilds it from a fresh snapshot. */
  async rebootstrap(): Promise<void> {
    const { snapshot, cursor } = await this.client.bootstrap()
    await clearReadModel()
    await applySnapshot(snapshot)
    await setMeta('cursor', cursor)
    await this.refreshCounts()
    this.update({ state: 'idle' })
  }

  /**
   * Records how far this device's clock is from the server's.
   *
   * Sync itself is immune to clock skew, because the cursor rides on
   * transaction ids. occurred_at is not, and every report groups by it, so a
   * badly set phone quietly files sales on the wrong day.
   */
  private trackClockOffset(serverTime: string): void {
    const offset = Date.parse(serverTime) - Date.now()
    void setMeta('clock_offset_ms', offset)
    this.update({ clockOffsetMs: offset })
  }

  /** True when the clock is wrong enough to warrant telling the user. */
  hasClockSkew(): boolean {
    return Math.abs(this.status.clockOffsetMs) > CLOCK_SKEW_WARN_MS
  }

  /**
   * Asks the browser to exempt this origin from storage eviction.
   *
   * On iOS this matters more than anywhere else: storage for sites unused for
   * about a week gets cleared, and the primary device here is an iPhone. It is
   * requested on every start rather than once, because Safari does not treat
   * the grant as permanent.
   *
   * The exposure it protects is narrow but real: everything already
   * acknowledged lives on the server, so only unsent operations would be lost.
   */
  private async requestPersistentStorage(): Promise<void> {
    if (typeof navigator === 'undefined' || !navigator.storage?.persist) return
    try {
      const alreadyPersisted = await navigator.storage.persisted?.()
      const granted = alreadyPersisted || (await navigator.storage.persist())
      await setMeta('storage_persisted', granted)
    } catch {
      // Not supported, or denied. The stale-pending warning is the fallback.
    }
  }

  private async refreshCounts(): Promise<void> {
    const [pendingCount, failedCount, stalePending] = await Promise.all([
      countByStatus('pending'),
      countByStatus('failed'),
      hasStalePending(),
    ])
    this.update({ pendingCount, failedCount, stalePending })
  }

  private update(patch: Partial<SyncStatus>): void {
    this.status = { ...this.status, ...patch }
    for (const listener of this.listeners) listener(this.status)
  }
}
