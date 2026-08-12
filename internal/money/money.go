// Package money holds every bit of arithmetic over amounts and quantities.
//
// Non-negotiable rules:
//   - Money is int64 cents. Never a float, never NUMERIC.
//   - Quantities are int64 thousandths of a unit (qty_milli).
//   - Half-up rounding, one implementation, mirrored in src/domain/money.ts
//     against the same testdata/money_cases.json fixture.
//
// If this implementation and the TypeScript one drift apart, the client quotes
// the buyer one total and the server stores another. That is why the fixture is
// shared: it is the only thing guaranteeing both say the same number.
package money

import "errors"

var (
	// ErrOverflow signals the operation exceeds int64 range. Unreachable with
	// real prices and quantities; it is still detected because the silent
	// failure mode (wrapping the sign) would be an accounting disaster.
	ErrOverflow = errors.New("money: integer arithmetic overflow")

	// ErrNonPositiveDenominator guards against division by zero.
	ErrNonPositiveDenominator = errors.New("money: denominator must be positive")

	// ErrNegative flags a negative value where the domain does not allow one.
	ErrNegative = errors.New("money: negative value not allowed")
)

// QtyScale is the quantity scale: 1000 thousandths = 1 unit.
//
// Everything is sold per unit today, but the schema stores thousandths on
// purpose: the day "half a pound" shows up, an integer qty would force a
// migration on clients holding offline data. It costs nothing now.
const QtyScale int64 = 1000

// RoundHalfUp divides numer by denom, rounding ties up.
//
// It uses quotient and remainder rather than (numer + denom/2) / denom because
// that form is only exact for even denominators and can overflow before
// dividing when the numerator is large.
func RoundHalfUp(numer, denom int64) (int64, error) {
	if denom <= 0 {
		return 0, ErrNonPositiveDenominator
	}
	if numer < 0 {
		return 0, ErrNegative
	}

	quot := numer / denom
	rem := numer % denom

	// An exact tie (rem == denom/2) rounds up. Comparing 2*rem >= denom avoids
	// depending on whether denom is even.
	if rem > 0 && 2*rem >= denom {
		quot++
	}
	return quot, nil
}

// LineTotal computes a sale line total from the unit price in cents and the
// quantity in thousandths.
//
// This is the function that produces the number the buyer sees and pays.
func LineTotal(unitPriceCents, qtyMilli int64) (int64, error) {
	if unitPriceCents < 0 || qtyMilli < 0 {
		return 0, ErrNegative
	}
	if unitPriceCents == 0 || qtyMilli == 0 {
		return 0, nil
	}

	// Overflow check before multiplying.
	if unitPriceCents > (1<<63-1)/qtyMilli {
		return 0, ErrOverflow
	}

	return RoundHalfUp(unitPriceCents*qtyMilli, QtyScale)
}

// Debt is an outstanding customer debt, used when splitting a payment.
type Debt struct {
	// RefID identifies the credit sale that created the debt.
	RefID string `json:"ref_id"`
	// AmountCents is what remains to be paid on it. Always positive.
	AmountCents int64 `json:"amount_cents"`
}

// Allocation is the slice of a payment applied to one specific debt.
type Allocation struct {
	RefID       string `json:"ref_id"`
	AmountCents int64  `json:"amount_cents"`
}

// AllocatePayment splits a payment across a customer's debts, oldest first, in
// whole cents.
//
// The split is greedy and NOT proportional. A proportional split always leaves a
// stray cent that has to go somewhere, and wherever it goes is an arbitrary
// decision impossible to explain to the customer. Greedy explains itself in one
// sentence: the oldest debt gets cancelled first.
//
// Whatever is left after covering every debt comes back as creditCents: the
// customer's credit balance. This always holds, and is checked as a property
// over randomized splits:
//
//	sum(allocations) + creditCents == paymentCents
//
// debts must arrive ordered oldest to newest. This function does not reorder,
// because the notion of "oldest" (occurred_at) belongs to the query.
func AllocatePayment(paymentCents int64, debts []Debt) (allocations []Allocation, creditCents int64, err error) {
	if paymentCents < 0 {
		return nil, 0, ErrNegative
	}

	remaining := paymentCents
	allocations = make([]Allocation, 0, len(debts))

	for _, d := range debts {
		if d.AmountCents < 0 {
			return nil, 0, ErrNegative
		}
		if remaining == 0 || d.AmountCents == 0 {
			continue
		}

		applied := d.AmountCents
		if remaining < applied {
			applied = remaining
		}

		allocations = append(allocations, Allocation{RefID: d.RefID, AmountCents: applied})
		remaining -= applied
	}

	return allocations, remaining, nil
}

// SumAllocations adds up a split. It exists so the invariant is checkable in one
// line, both in tests and in cmd/verify.
func SumAllocations(allocations []Allocation) int64 {
	var total int64
	for _, a := range allocations {
		total += a.AmountCents
	}
	return total
}
