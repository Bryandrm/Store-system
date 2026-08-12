/**
 * Derived reads over the local replica.
 *
 * Nothing here is cached. Stock and balances are recomputed by summing the
 * ledger every time they are asked for.
 *
 * That is deliberate. An incremental counter would be faster and would be the
 * exact premature abstraction that produces double-counting bugs, because it
 * introduces a second source of truth that has to be kept in step with the
 * first. At a few thousand rows these sums are sub-millisecond; revisit only
 * when a profile says otherwise.
 */
import type { Product, Sale, SaleLine } from '@/domain/types'
import { getDB } from './schema'

/** Current stock of one product, in thousandths of a unit. */
export async function stockFor(productId: string): Promise<number> {
  const db = await getDB()
  const movements = await db.getAllFromIndex('stock_movements', 'by_product', productId)
  return movements.reduce((sum, m) => sum + m.delta_qty_milli, 0)
}

/** Current stock of every product. */
export async function allStock(): Promise<Map<string, number>> {
  const db = await getDB()
  const movements = await db.getAll('stock_movements')

  const totals = new Map<string, number>()
  for (const m of movements) {
    totals.set(m.product_id, (totals.get(m.product_id) ?? 0) + m.delta_qty_milli)
  }
  return totals
}

/**
 * Current balance of one customer, in cents.
 * Positive means they have credit; negative means they owe.
 */
export async function balanceFor(customerId: string): Promise<number> {
  const db = await getDB()
  const entries = await db.getAllFromIndex('customer_ledger', 'by_customer', customerId)
  return entries.reduce((sum, e) => sum + e.delta_cents, 0)
}

/** Active products, in display order, each with its current price. */
export async function sellableProducts(): Promise<(Product & { price_cents: number })[]> {
  const db = await getDB()
  const [products, prices] = await Promise.all([
    db.getAll('products'),
    db.getAll('product_prices'),
  ])

  const priceByProduct = new Map(prices.map((p) => [p.product_id, p.price_cents]))

  return products
    .filter((p) => p.is_active)
    .sort((a, b) => a.sort_order - b.sort_order || a.name.localeCompare(b.name, 'es'))
    .map((p) => ({ ...p, price_cents: priceByProduct.get(p.id) ?? 0 }))
}

/**
 * The day's sales, in the store's timezone.
 *
 * El Salvador is UTC-6 with no daylight saving, but the offset is never
 * hardcoded: the boundary is computed through Intl so the code stays correct if
 * it is ever used somewhere else.
 *
 * Grouping is by occurred_at — when the sale actually happened — never by when
 * it reached the server. A sale rung up offline on Monday belongs to Monday
 * even if it syncs on Wednesday.
 */
export const STORE_TIMEZONE = 'America/El_Salvador'

export async function salesForLocalDay(reference = new Date()): Promise<Sale[]> {
  const db = await getDB()
  const { start, end } = localDayBounds(reference)

  const sales = await db.getAllFromIndex(
    'sales',
    'by_occurred_at',
    IDBKeyRange.bound(start.toISOString(), end.toISOString(), false, true),
  )
  return sales.sort((a, b) => b.occurred_at.localeCompare(a.occurred_at))
}

export async function linesForSale(saleId: string): Promise<SaleLine[]> {
  const db = await getDB()
  const lines = await db.getAllFromIndex('sale_lines', 'by_sale', saleId)
  return lines.sort((a, b) => a.line_no - b.line_no)
}

/** Total taken in during the local day, in cents. */
export async function totalForLocalDay(reference = new Date()): Promise<number> {
  const sales = await salesForLocalDay(reference)
  return sales.reduce((sum, s) => sum + s.total_cents, 0)
}

/**
 * Start and end of the local day containing `reference`, as UTC instants.
 *
 * Exported because getting a day boundary wrong is the classic off-by-six-hours
 * bug, and it deserves its own test at 23:59:59 and 00:00:00 local time.
 */
export function localDayBounds(reference: Date): { start: Date; end: Date } {
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: STORE_TIMEZONE,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).formatToParts(reference)

  const get = (type: string) => parts.find((p) => p.type === type)?.value ?? '00'
  const localDate = `${get('year')}-${get('month')}-${get('day')}`

  const start = zonedMidnightToUTC(localDate)
  const end = new Date(start.getTime() + 24 * 60 * 60_000)
  return { start, end }
}

/** Converts local midnight on a YYYY-MM-DD to the matching UTC instant. */
function zonedMidnightToUTC(localDate: string): Date {
  // Start from the naive UTC reading, then correct by the zone's offset at that
  // instant. Two passes because the offset itself depends on the instant.
  const naive = new Date(`${localDate}T00:00:00Z`)
  const offsetMs = zoneOffsetMs(naive)
  return new Date(naive.getTime() + offsetMs)
}

function zoneOffsetMs(at: Date): number {
  const formatted = new Intl.DateTimeFormat('en-US', {
    timeZone: STORE_TIMEZONE,
    hour12: false,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).formatToParts(at)

  const get = (type: string) => formatted.find((p) => p.type === type)?.value ?? '00'
  const asUTC = Date.UTC(
    Number(get('year')),
    Number(get('month')) - 1,
    Number(get('day')),
    Number(get('hour')) % 24,
    Number(get('minute')),
    Number(get('second')),
  )
  return at.getTime() - asUTC
}
