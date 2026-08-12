/**
 * The local IndexedDB replica.
 *
 * Every device holds a complete copy of the store's data. That is why the API
 * has so few read endpoints: reports, sales lists and customer statements are
 * all computed here, from these stores.
 *
 * `idb` is used rather than raw IndexedDB because the native API is callback
 * based and its transactions auto-close in ways that break silently under
 * `await`. It is a promise wrapper, roughly 1.4 kB, not a framework — Dexie
 * would be the framework and is deliberately avoided.
 */
import { openDB, type DBSchema, type IDBPDatabase } from 'idb'

import type {
  CartLine,
  Customer,
  CustomerLedgerEntry,
  OutboxEntry,
  Payment,
  Product,
  ProductPrice,
  Sale,
  SaleLine,
  StockMovement,
} from '@/domain/types'

export const DB_NAME = 'store_system'
export const DB_VERSION = 1

/** Keys used in the `meta` store. Typed so a typo cannot invent a new setting. */
export type MetaKey =
  | 'cursor'
  | 'device_id'
  | 'session'
  | 'clock_offset_ms'
  | 'last_sync_at'
  | 'storage_persisted'

export interface StoreSystemDB extends DBSchema {
  outbox: {
    key: string
    value: OutboxEntry
    indexes: {
      /** Drives the send loop: pending entries in causal order. */
      by_status_opid: [string, string]
    }
  }
  meta: { key: MetaKey; value: unknown }
  /** The sale being rung up. Persisted so a reload mid-sale loses nothing. */
  cart: { key: 'current'; value: CartLine[] }

  products: { key: string; value: Product }
  product_prices: { key: string; value: ProductPrice }
  customers: { key: string; value: Customer }
  sales: { key: string; value: Sale; indexes: { by_occurred_at: string } }
  sale_lines: { key: string; value: SaleLine; indexes: { by_sale: string } }
  stock_movements: {
    key: string
    value: StockMovement
    indexes: { by_product: string }
  }
  customer_ledger: {
    key: string
    value: CustomerLedgerEntry
    indexes: { by_customer: string }
  }
  payments: { key: string; value: Payment; indexes: { by_customer: string } }
}

let dbPromise: Promise<IDBPDatabase<StoreSystemDB>> | null = null

/** Opens (and on first call creates) the local database. */
export function getDB(): Promise<IDBPDatabase<StoreSystemDB>> {
  dbPromise ??= openDB<StoreSystemDB>(DB_NAME, DB_VERSION, {
    upgrade(db) {
      const outbox = db.createObjectStore('outbox', { keyPath: 'op_id' })
      outbox.createIndex('by_status_opid', ['status', 'op_id'])

      db.createObjectStore('meta')
      db.createObjectStore('cart')

      db.createObjectStore('products', { keyPath: 'id' })
      // product_prices is keyed by product_id because only the current price is
      // held locally; the full history stays on the server, where it is audited.
      db.createObjectStore('product_prices', { keyPath: 'product_id' })
      db.createObjectStore('customers', { keyPath: 'id' })

      const sales = db.createObjectStore('sales', { keyPath: 'id' })
      sales.createIndex('by_occurred_at', 'occurred_at')

      const saleLines = db.createObjectStore('sale_lines', { keyPath: 'id' })
      saleLines.createIndex('by_sale', 'sale_id')

      const movements = db.createObjectStore('stock_movements', { keyPath: 'id' })
      movements.createIndex('by_product', 'product_id')

      const ledger = db.createObjectStore('customer_ledger', { keyPath: 'id' })
      ledger.createIndex('by_customer', 'customer_id')

      const payments = db.createObjectStore('payments', { keyPath: 'id' })
      payments.createIndex('by_customer', 'customer_id')
    },
  })
  return dbPromise
}

/**
 * Closes the connection and drops the cached handle.
 *
 * Closing matters: IndexedDB's deleteDatabase BLOCKS while any connection is
 * still open, and it blocks silently — no error, the promise simply never
 * settles. Dropping the cached promise without closing the underlying
 * connection leaves it open forever.
 *
 * The same applies in the browser during a version upgrade, which is why this
 * is exported rather than being a test-only helper.
 */
export async function closeDB(): Promise<void> {
  if (!dbPromise) return
  const db = await dbPromise
  db.close()
  dbPromise = null
}

export async function getMeta<T>(key: MetaKey): Promise<T | undefined> {
  const db = await getDB()
  return (await db.get('meta', key)) as T | undefined
}

export async function setMeta(key: MetaKey, value: unknown): Promise<void> {
  const db = await getDB()
  await db.put('meta', value, key)
}

/**
 * Wipes the replica but KEEPS the outbox and the session.
 *
 * Used when the server answers CURSOR_TOO_OLD: the read model is stale and must
 * be rebuilt from a fresh bootstrap, but unsent operations are real business
 * data that has never reached anyone else. Dropping them would silently destroy
 * sales that actually happened.
 */
export async function clearReadModel(): Promise<void> {
  const db = await getDB()
  const stores = [
    'products',
    'product_prices',
    'customers',
    'sales',
    'sale_lines',
    'stock_movements',
    'customer_ledger',
    'payments',
  ] as const

  const tx = db.transaction(stores, 'readwrite')
  await Promise.all(stores.map((s) => tx.objectStore(s).clear()))
  await tx.done

  await setMeta('cursor', '0')
}
