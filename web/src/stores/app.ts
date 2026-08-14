/**
 * Application state.
 *
 * The rule that keeps this honest: these stores are a CACHE over IndexedDB,
 * never the source of truth. They hydrate from the database on boot and are
 * refreshed after writes. Nothing is stored only here, because state living in
 * two places is how two views of the same sale start disagreeing.
 */
import { create } from 'zustand'

import { sellableProducts, salesForLocalDay, allStock } from '@/db/local'
import { getDB, getMeta, setMeta } from '@/db/schema'
import type { CartLine, Product, Sale, Session } from '@/domain/types'
import { uuidv7 } from '@/domain/uuid'
import type { SyncStatus } from '@/sync/engine'

export type SellableProduct = Product & { price_cents: number }

interface AppState {
  hydrated: boolean
  session: Session | null
  deviceId: string

  products: SellableProduct[]
  stock: Map<string, number>
  todaysSales: Sale[]
  /** Sale ids still waiting to reach the server, for the pending marker. */
  pendingSaleIds: Set<string>

  cart: CartLine[]
  syncStatus: SyncStatus | null

  /**
   * Asks the sync engine to run now.
   *
   * Without this, a sale sits in the outbox until the 60 second poll, so the
   * user sees PENDING for a minute after every sale even on perfect signal.
   * Set by App when the engine starts; a no-op before then.
   */
  nudgeSync: () => void

  hydrate: () => Promise<void>
  refreshData: () => Promise<void>
  setSession: (session: Session | null) => Promise<void>
  setSyncStatus: (status: SyncStatus) => void
  setNudgeSync: (fn: () => void) => void

  addToCart: (product: SellableProduct, qtyMilli?: number) => Promise<void>
  setLineQty: (productId: string, qtyMilli: number) => Promise<void>
  removeFromCart: (productId: string) => Promise<void>
  clearCart: () => Promise<void>
}

export const useApp = create<AppState>((set, get) => ({
  hydrated: false,
  session: null,
  deviceId: '',
  products: [],
  stock: new Map(),
  todaysSales: [],
  pendingSaleIds: new Set(),
  cart: [],
  syncStatus: null,
  nudgeSync: () => {},

  async hydrate() {
    const [session, storedDeviceId, db] = await Promise.all([
      getMeta<Session>('session'),
      getMeta<string>('device_id'),
      getDB(),
    ])

    // The device id is generated once and kept forever. The server uses it to
    // attribute operations, so regenerating it would make the same phone look
    // like a new one after every reload.
    let deviceId = storedDeviceId
    if (!deviceId) {
      deviceId = uuidv7()
      await setMeta('device_id', deviceId)
    }

    // The cart survives a reload. A phone in a pocket reloads the tab, and
    // losing a half-rung sale in front of a customer is the worst bug a
    // point-of-sale app can have.
    const cart = (await db.get('cart', 'current')) ?? []

    set({ session: session ?? null, deviceId, cart, hydrated: true })
    await get().refreshData()
  },

  async refreshData() {
    const [products, stock, todaysSales, db] = await Promise.all([
      sellableProducts(),
      allStock(),
      salesForLocalDay(),
      getDB(),
    ])

    // A sale is "pending" when an outbox entry still references it. That is
    // read from the queue rather than flagged on the sale itself, so the marker
    // cannot drift out of step with reality.
    const outbox = await db.getAll('outbox')
    const pendingSaleIds = new Set<string>()
    for (const entry of outbox) {
      if (entry.status === 'pending' || entry.status === 'inflight') {
        for (const id of entry.entity_ids) pendingSaleIds.add(id)
      }
    }

    set({ products, stock, todaysSales, pendingSaleIds })
  },

  async setSession(session) {
    await setMeta('session', session)
    set({ session })
  },

  setSyncStatus(status) {
    set({ syncStatus: status })
  },

  setNudgeSync(fn) {
    set({ nudgeSync: fn })
  },

  async addToCart(product, qtyMilli = 1000) {
    const cart = [...get().cart]
    const existing = cart.findIndex((l) => l.product_id === product.id)

    if (existing >= 0) {
      cart[existing] = { ...cart[existing]!, qty_milli: cart[existing]!.qty_milli + qtyMilli }
    } else {
      cart.push({
        product_id: product.id,
        product_name: product.name,
        qty_milli: qtyMilli,
        // The price is captured when the item enters the cart, so a price
        // change mid-sale never rewrites what the customer was quoted.
        unit_price_cents: product.price_cents,
      })
    }

    await persistCart(cart)
    set({ cart })
  },

  async setLineQty(productId, qtyMilli) {
    if (qtyMilli <= 0) return get().removeFromCart(productId)

    const cart = get().cart.map((l) =>
      l.product_id === productId ? { ...l, qty_milli: qtyMilli } : l,
    )
    await persistCart(cart)
    set({ cart })
  },

  async removeFromCart(productId) {
    const cart = get().cart.filter((l) => l.product_id !== productId)
    await persistCart(cart)
    set({ cart })
  },

  async clearCart() {
    await persistCart([])
    set({ cart: [] })
  },
}))

async function persistCart(cart: CartLine[]): Promise<void> {
  const db = await getDB()
  await db.put('cart', cart, 'current')
}
