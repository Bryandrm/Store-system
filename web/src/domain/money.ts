/**
 * Money and quantity arithmetic.
 *
 * This file is a deliberate mirror of internal/money/money.go. Both are checked
 * against the SAME fixture, testdata/money_cases.json. That shared file is the
 * only thing preventing the client from quoting the buyer one total while the
 * server stores another.
 *
 * If you change a rule here, change it there, and expect the other suite to go
 * red until you do.
 */

/**
 * Money is always integer cents. The brand costs nothing at runtime and catches
 * the one mistake that matters: passing a currency amount where a quantity was
 * expected, or a float where an integer was.
 */
export type Cents = number & { readonly __cents: unique symbol }

/** Quantities are thousandths of a unit, matching qty_milli in the database. */
export type QtyMilli = number & { readonly __qtyMilli: unique symbol }

export const cents = (n: number): Cents => n as Cents
export const qtyMilli = (n: number): QtyMilli => n as QtyMilli

/** 1000 thousandths = 1 unit. */
export const QTY_SCALE = 1000

export class MoneyError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'MoneyError'
  }
}

/**
 * Divides numer by denom, rounding ties up.
 *
 * Uses quotient and remainder rather than (numer + denom/2) / denom, matching
 * the Go implementation: that form is only exact for even denominators.
 */
export function roundHalfUp(numer: number, denom: number): number {
  if (!Number.isInteger(numer) || !Number.isInteger(denom)) {
    throw new MoneyError('roundHalfUp only accepts integers')
  }
  if (denom <= 0) {
    throw new MoneyError('the denominator must be positive')
  }
  if (numer < 0) {
    throw new MoneyError('negative value not allowed')
  }

  const quot = Math.floor(numer / denom)
  const rem = numer % denom

  // An exact tie rounds up. Comparing 2*rem >= denom avoids depending on
  // whether denom is even.
  return rem > 0 && 2 * rem >= denom ? quot + 1 : quot
}

/**
 * Computes a sale line total from the unit price in cents and the quantity in
 * thousandths. This produces the number the buyer sees and pays.
 */
export function lineTotal(unitPriceCents: number, qty: number): Cents {
  if (unitPriceCents < 0 || qty < 0) {
    throw new MoneyError('negative value not allowed')
  }
  if (unitPriceCents === 0 || qty === 0) {
    return cents(0)
  }

  const product = unitPriceCents * qty
  // JavaScript integers are exact only up to 2^53. Real prices never come close,
  // but silent precision loss in a total would be an accounting bug nobody could
  // trace, so it is checked rather than assumed.
  if (!Number.isSafeInteger(product)) {
    throw new MoneyError('integer arithmetic overflow')
  }

  return cents(roundHalfUp(product, QTY_SCALE))
}

/** Sums line totals. Integer addition is exact, so no rounding happens here. */
export function saleTotal(lineTotals: readonly number[]): Cents {
  let total = 0
  for (const t of lineTotals) {
    total += t
  }
  if (!Number.isSafeInteger(total)) {
    throw new MoneyError('integer arithmetic overflow')
  }
  return cents(total)
}

export interface Debt {
  ref_id: string
  amount_cents: number
}

export interface Allocation {
  ref_id: string
  amount_cents: number
}

export interface PaymentSplit {
  allocations: Allocation[]
  creditCents: Cents
}

/**
 * Splits a payment across a customer's debts, oldest first, in whole cents.
 *
 * Greedy, never proportional. A proportional split always leaves a stray cent
 * that has to go somewhere, and wherever it goes is arbitrary and impossible to
 * explain to the customer. Greedy explains itself in one sentence: the oldest
 * debt is cancelled first.
 *
 * Anything left over is the customer's credit balance, and this always holds:
 *
 *     sum(allocations) + creditCents === paymentCents
 *
 * `debts` must arrive ordered oldest to newest; this function does not reorder.
 */
export function allocatePayment(paymentCents: number, debts: readonly Debt[]): PaymentSplit {
  if (paymentCents < 0) {
    throw new MoneyError('negative value not allowed')
  }

  let remaining = paymentCents
  const allocations: Allocation[] = []

  for (const debt of debts) {
    if (debt.amount_cents < 0) {
      throw new MoneyError('negative value not allowed')
    }
    if (remaining === 0 || debt.amount_cents === 0) {
      continue
    }

    const applied = Math.min(debt.amount_cents, remaining)
    allocations.push({ ref_id: debt.ref_id, amount_cents: applied })
    remaining -= applied
  }

  return { allocations, creditCents: cents(remaining) }
}

export function sumAllocations(allocations: readonly Allocation[]): number {
  return allocations.reduce((sum, a) => sum + a.amount_cents, 0)
}

/**
 * Formats cents for display, in the store's locale.
 *
 * El Salvador uses USD. Display only: the returned string never goes back into
 * a stored value.
 */
export function formatCents(value: number): string {
  return new Intl.NumberFormat('es-SV', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
  }).format(value / 100)
}

/** Formats a quantity, dropping the decimals when it is a whole number. */
export function formatQty(value: number): string {
  const units = value / QTY_SCALE
  return Number.isInteger(units) ? String(units) : units.toFixed(3).replace(/0+$/, '')
}
