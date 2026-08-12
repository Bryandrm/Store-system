import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

import {
  allocatePayment,
  lineTotal,
  MoneyError,
  roundHalfUp,
  sumAllocations,
  type Allocation,
  type Debt,
} from './money'

/**
 * The SAME file internal/money/money_test.go reads.
 *
 * This is the highest-value test on the TypeScript side: it is the only thing
 * that keeps client and server agreeing on what a sale costs. If you point this
 * at a copy, the whole guarantee evaporates.
 */
const fixturePath = fileURLToPath(new URL('../../../testdata/money_cases.json', import.meta.url))

interface Fixture {
  round_half_up: { name: string; numer: number; denom: number; want: number }[]
  line_total: { name: string; unit_price_cents: number; qty_milli: number; want: number }[]
  allocate_payment: {
    name: string
    payment_cents: number
    debts: Debt[]
    want_allocations: Allocation[]
    want_credit_cents: number
  }[]
}

const fixture: Fixture = JSON.parse(readFileSync(fixturePath, 'utf8'))

describe('roundHalfUp', () => {
  it('has cases in the shared fixture', () => {
    expect(fixture.round_half_up.length).toBeGreaterThan(0)
  })

  for (const c of fixture.round_half_up) {
    it(c.name, () => {
      expect(roundHalfUp(c.numer, c.denom)).toBe(c.want)
    })
  }

  it('rejects a non-positive denominator', () => {
    expect(() => roundHalfUp(100, 0)).toThrow(MoneyError)
    expect(() => roundHalfUp(100, -5)).toThrow(MoneyError)
  })

  it('rejects a negative numerator', () => {
    expect(() => roundHalfUp(-1, 1000)).toThrow(MoneyError)
  })
})

describe('lineTotal', () => {
  it('has cases in the shared fixture', () => {
    expect(fixture.line_total.length).toBeGreaterThan(0)
  })

  for (const c of fixture.line_total) {
    it(c.name, () => {
      expect(lineTotal(c.unit_price_cents, c.qty_milli)).toBe(c.want)
    })
  }

  it('rejects a negative price', () => {
    expect(() => lineTotal(-1, 1000)).toThrow(MoneyError)
  })

  it('detects overflow rather than losing precision silently', () => {
    expect(() => lineTotal(2 ** 40, 2 ** 40)).toThrow(MoneyError)
  })
})

describe('allocatePayment', () => {
  it('has cases in the shared fixture', () => {
    expect(fixture.allocate_payment.length).toBeGreaterThan(0)
  })

  for (const c of fixture.allocate_payment) {
    it(c.name, () => {
      const { allocations, creditCents } = allocatePayment(c.payment_cents, c.debts)
      expect(allocations).toEqual(c.want_allocations)
      expect(creditCents).toBe(c.want_credit_cents)
    })
  }

  /**
   * The property test, mirroring TestAllocatePaymentInvariant in Go: over
   * randomized splits, allocations plus leftover credit must equal the payment
   * exactly. Not one cent is created or lost.
   */
  it('never creates or loses a cent', () => {
    // Deterministic generator: a failure has to be reproducible.
    let seed = 1
    const nextInt = (max: number) => {
      seed = (seed * 1103515245 + 12345) & 0x7fffffff
      return seed % max
    }

    for (let i = 0; i < 100; i++) {
      const payment = nextInt(100_000)
      const debts: Debt[] = Array.from({ length: nextInt(8) }, (_, j) => ({
        ref_id: String.fromCharCode(97 + j),
        amount_cents: nextInt(30_000),
      }))

      const { allocations, creditCents } = allocatePayment(payment, debts)

      expect(sumAllocations(allocations) + creditCents).toBe(payment)

      const byRef = new Map(debts.map((d) => [d.ref_id, d.amount_cents]))
      for (const a of allocations) {
        expect(a.amount_cents).toBeGreaterThan(0)
        expect(a.amount_cents).toBeLessThanOrEqual(byRef.get(a.ref_id) ?? 0)
      }
    }
  })

  it('rejects negative amounts', () => {
    expect(() => allocatePayment(-1, [])).toThrow(MoneyError)
    expect(() => allocatePayment(100, [{ ref_id: 'a', amount_cents: -5 }])).toThrow(MoneyError)
  })
})
