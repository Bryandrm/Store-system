import { beforeEach, describe, expect, it } from 'vitest'

import { stockFor } from '@/db/local'
import { countByStatus } from '@/db/outbox'
import { closeDB, getDB } from '@/db/schema'
import { recordSale } from './recordSale'
import type { CartLine, Session } from './types'
import { uuidv7, uuidv7Timestamp } from './uuid'

async function freshDB() {
  await closeDB()
  const { deleteDB } = await import('idb')
  await deleteDB('store_system')
  await getDB()
}

beforeEach(async () => {
  await freshDB()
})

const session: Session = {
  token: 't',
  user_id: 'user-1',
  username: 'bryan',
  display_name: 'Bryan',
  role: 'owner',
}

const line = (productId: string, qty: number, price: number): CartLine => ({
  product_id: productId,
  product_name: `producto ${productId}`,
  qty_milli: qty,
  unit_price_cents: price,
})

describe('recordSale', () => {
  it('writes the sale, its lines and one stock movement per line', async () => {
    const result = await recordSale({
      lines: [line('p1', 2000, 50), line('p2', 3000, 25)],
      paymentMethod: 'cash',
      session,
      deviceId: 'phone-1',
    })

    // 2 x 0.50 + 3 x 0.25 = 1.00 + 0.75
    expect(result.totalCents).toBe(175)

    const db = await getDB()
    expect(await db.count('sales')).toBe(1)
    expect(await db.count('sale_lines')).toBe(2)
    expect(await db.count('stock_movements')).toBe(2)

    // Selling decrements derived stock immediately, with no server round trip.
    expect(await stockFor('p1')).toBe(-2000)
    expect(await stockFor('p2')).toBe(-3000)
  })

  /** The sale has to be queued for sending, or it exists only on this phone. */
  it('queues exactly one operation', async () => {
    await recordSale({
      lines: [line('p1', 1000, 25)],
      paymentMethod: 'cash',
      session,
      deviceId: 'phone-1',
    })

    expect(await countByStatus('pending')).toBe(1)
  })

  it('marks a cash sale as fully paid', async () => {
    await recordSale({
      lines: [line('p1', 1000, 25)],
      paymentMethod: 'cash',
      session,
      deviceId: 'phone-1',
    })

    const db = await getDB()
    const [sale] = await db.getAll('sales')
    expect(sale?.paid_cents).toBe(sale?.total_cents)
  })

  it('marks a credit sale as entirely owed', async () => {
    await recordSale({
      lines: [line('p1', 1000, 25)],
      paymentMethod: 'credit',
      customerId: 'customer-1',
      session,
      deviceId: 'phone-1',
    })

    const db = await getDB()
    const [sale] = await db.getAll('sales')
    expect(sale?.paid_cents).toBe(0)
    expect(sale?.customer_id).toBe('customer-1')
  })

  /** Anything not settled in cash has to be owed by somebody nameable. */
  it('refuses a credit sale with no customer', async () => {
    await expect(
      recordSale({
        lines: [line('p1', 1000, 25)],
        paymentMethod: 'credit',
        session,
        deviceId: 'phone-1',
      }),
    ).rejects.toThrow(/cliente/)
  })

  it('refuses an empty sale', async () => {
    await expect(
      recordSale({ lines: [], paymentMethod: 'cash', session, deviceId: 'phone-1' }),
    ).rejects.toThrow()
  })

  /**
   * The line snapshots the product name, so renaming a product later never
   * rewrites what a past receipt said.
   */
  it('freezes the product name on the line', async () => {
    await recordSale({
      lines: [line('p1', 1000, 25)],
      paymentMethod: 'cash',
      session,
      deviceId: 'phone-1',
    })

    const db = await getDB()
    const [saleLine] = await db.getAll('sale_lines')
    expect(saleLine?.product_name_snapshot).toBe('producto p1')
  })

  /**
   * Sync is immune to a bad clock because the cursor rides on transaction ids,
   * but occurred_at is not, and reports group by it. A known offset is applied
   * before stamping.
   */
  it('applies the known clock offset to occurred_at', async () => {
    const oneHour = 60 * 60_000
    const before = Date.now()

    await recordSale({
      lines: [line('p1', 1000, 25)],
      paymentMethod: 'cash',
      session,
      deviceId: 'phone-1',
      clockOffsetMs: oneHour,
    })

    const db = await getDB()
    const [sale] = await db.getAll('sales')
    const stamped = Date.parse(sale!.occurred_at)

    expect(stamped).toBeGreaterThanOrEqual(before + oneHour - 5_000)
    expect(stamped).toBeLessThanOrEqual(Date.now() + oneHour + 5_000)
  })

  it('numbers lines from one, in cart order', async () => {
    await recordSale({
      lines: [line('p1', 1000, 25), line('p2', 1000, 25), line('p3', 1000, 25)],
      paymentMethod: 'cash',
      session,
      deviceId: 'phone-1',
    })

    const db = await getDB()
    const lines = await db.getAll('sale_lines')
    expect(lines.map((l) => l.line_no).sort()).toEqual([1, 2, 3])
  })
})

describe('uuidv7', () => {
  /**
   * The outbox iterates op_ids in order and calls that causal order. That only
   * holds if ids sort by creation time, which is the whole reason for v7 over
   * the v4 that crypto.randomUUID() produces.
   */
  it('sorts by creation time', () => {
    const early = uuidv7(1_700_000_000_000)
    const late = uuidv7(1_700_000_001_000)
    expect(early < late).toBe(true)
  })

  it('stays ordered within the same millisecond', () => {
    const ids = Array.from({ length: 50 }, () => uuidv7(1_700_000_000_000))
    expect([...ids].sort()).toEqual(ids)
  })

  /**
   * A clock correction that jumps backwards would otherwise emit ids sorting
   * before ones already queued, silently reordering the outbox.
   */
  it('does not go backwards when the clock does', () => {
    const normal = uuidv7(1_700_000_010_000)
    const afterJumpBack = uuidv7(1_700_000_000_000)
    expect(afterJumpBack > normal).toBe(true)
  })

  it('is a well-formed version 7 uuid', () => {
    const id = uuidv7()
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  })

  /**
   * A future timestamp is used on purpose. The monotonic guard clamps anything
   * older than the last id issued — which is the point of the test above — so
   * asserting an exact round-trip with a past timestamp would contradict the
   * backwards-jump protection rather than test the encoding.
   */
  it('embeds the timestamp it was given', () => {
    const farFuture = Date.now() + 10 * 365 * 24 * 60 * 60_000
    expect(uuidv7Timestamp(uuidv7(farFuture))).toBe(farFuture)
  })
})
