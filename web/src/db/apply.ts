/**
 * The single write path into the local replica.
 *
 * Local optimistic writes and changes arriving from the server both go through
 * `applyChange`. One code path means convergence is free, and it removes the
 * entire class of bugs where the optimistic version of a row and the real one
 * drift apart.
 *
 * Every write is an upsert keyed by entity id, so re-applying a change is a
 * no-op. That is what makes the feed's at-least-once delivery safe: over-
 * delivery costs nothing, and under-delivery is the only real failure.
 */
import type { IDBPDatabase, IDBPTransaction } from 'idb'

import type { Change } from '@/domain/types'
import { getDB, type StoreSystemDB } from './schema'

/**
 * Maps the entity names the server sends onto local store names.
 *
 * They match one-to-one today. The map exists so that an unknown entity is
 * ignored explicitly rather than throwing: a newer server that starts shipping
 * an entity this client version does not know about must not break syncing for
 * a device that has not updated yet.
 */
const ENTITY_TO_STORE = {
  product: 'products',
  product_price: 'product_prices',
  customer: 'customers',
  sale: 'sales',
  sale_line: 'sale_lines',
  stock_movement: 'stock_movements',
  customer_ledger: 'customer_ledger',
  payment: 'payments',
} as const satisfies Record<string, keyof StoreSystemDB>

export type WritableStore = (typeof ENTITY_TO_STORE)[keyof typeof ENTITY_TO_STORE]

/** The stores applyChange may write to, for opening a transaction over them. */
export const WRITABLE_STORES = Object.values(ENTITY_TO_STORE) as WritableStore[]

/** Entities this client version does not understand, for diagnostics. */
const unknownEntities = new Set<string>()

export function unknownEntitiesSeen(): string[] {
  return [...unknownEntities]
}

/**
 * Applies one change inside an existing transaction.
 *
 * Takes a transaction rather than opening its own so that a whole batch from
 * the feed lands atomically: a half-applied batch would leave derived stock
 * disagreeing with the sales it came from.
 */
export function applyChangeInTx(
  tx: IDBPTransaction<StoreSystemDB, readonly WritableStore[], 'readwrite'>,
  change: Change,
): void {
  const storeName = ENTITY_TO_STORE[change.entity as keyof typeof ENTITY_TO_STORE]

  if (!storeName) {
    // Forward compatibility, not an error: ignoring an unknown entity keeps an
    // older client syncing everything it does understand.
    unknownEntities.add(change.entity)
    return
  }

  // There is no 'delete': nothing transactional is ever deleted, and catalog
  // rows are hidden with is_active rather than removed.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  void (tx.objectStore(storeName) as any).put(change.payload)
}

/** Applies a batch of changes atomically. */
export async function applyChanges(changes: readonly Change[]): Promise<void> {
  if (changes.length === 0) return

  const db = await getDB()
  const tx = db.transaction(WRITABLE_STORES, 'readwrite')
  for (const change of changes) {
    applyChangeInTx(tx, change)
  }
  await tx.done
}

/**
 * Applies a single change, opening its own transaction.
 *
 * This is what a local optimistic write calls: the user rang up a sale and it
 * has to appear immediately, long before any server has heard about it.
 */
export async function applyChange(change: Change): Promise<void> {
  await applyChanges([change])
}

/**
 * Applies a full bootstrap snapshot.
 *
 * Bootstrap may over-deliver — rows it contains can also arrive again in the
 * first feed page — and that is safe for exactly the same reason: upsert by id.
 */
export async function applySnapshot(
  snapshot: Record<string, unknown[]>,
  db?: IDBPDatabase<StoreSystemDB>,
): Promise<void> {
  const database = db ?? (await getDB())

  const sections: [keyof typeof SNAPSHOT_TO_STORE, WritableStore][] = Object.entries(
    SNAPSHOT_TO_STORE,
  ) as [keyof typeof SNAPSHOT_TO_STORE, WritableStore][]

  const tx = database.transaction(WRITABLE_STORES, 'readwrite')
  for (const [section, storeName] of sections) {
    const rows = snapshot[section]
    if (!Array.isArray(rows)) continue
    for (const row of rows) {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      void (tx.objectStore(storeName) as any).put(row)
    }
  }
  await tx.done
}

/** Snapshot section names as sent by GET /bootstrap. */
const SNAPSHOT_TO_STORE = {
  products: 'products',
  prices: 'product_prices',
  customers: 'customers',
  sales: 'sales',
  sale_lines: 'sale_lines',
  stock_movements: 'stock_movements',
  customer_ledger: 'customer_ledger',
  payments: 'payments',
} as const satisfies Record<string, WritableStore>
