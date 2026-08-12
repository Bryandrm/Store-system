package money

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath points at the fixture SHARED with src/domain/money.test.ts.
// If you move it, move it there too: the value of the fixture is that it is the
// same file, not a similar one.
const fixturePath = "../../testdata/money_cases.json"

type fixture struct {
	RoundHalfUp []struct {
		Name  string `json:"name"`
		Numer int64  `json:"numer"`
		Denom int64  `json:"denom"`
		Want  int64  `json:"want"`
	} `json:"round_half_up"`

	LineTotal []struct {
		Name           string `json:"name"`
		UnitPriceCents int64  `json:"unit_price_cents"`
		QtyMilli       int64  `json:"qty_milli"`
		Want           int64  `json:"want"`
	} `json:"line_total"`

	AllocatePayment []struct {
		Name            string       `json:"name"`
		PaymentCents    int64        `json:"payment_cents"`
		Debts           []Debt       `json:"debts"`
		WantAllocations []Allocation `json:"want_allocations"`
		WantCreditCents int64        `json:"want_credit_cents"`
	} `json:"allocate_payment"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(fixturePath))
	if err != nil {
		t.Fatalf("could not read shared fixture %s: %v", fixturePath, err)
	}

	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	return f
}

func TestRoundHalfUp(t *testing.T) {
	f := loadFixture(t)
	if len(f.RoundHalfUp) == 0 {
		t.Fatal("fixture has no round_half_up cases")
	}

	for _, c := range f.RoundHalfUp {
		t.Run(c.Name, func(t *testing.T) {
			got, err := RoundHalfUp(c.Numer, c.Denom)
			if err != nil {
				t.Fatalf("RoundHalfUp(%d, %d) returned error: %v", c.Numer, c.Denom, err)
			}
			if got != c.Want {
				t.Errorf("RoundHalfUp(%d, %d) = %d, want %d", c.Numer, c.Denom, got, c.Want)
			}
		})
	}
}

func TestRoundHalfUpErrors(t *testing.T) {
	if _, err := RoundHalfUp(100, 0); err != ErrNonPositiveDenominator {
		t.Errorf("zero denominator: want ErrNonPositiveDenominator, got %v", err)
	}
	if _, err := RoundHalfUp(100, -5); err != ErrNonPositiveDenominator {
		t.Errorf("negative denominator: want ErrNonPositiveDenominator, got %v", err)
	}
	if _, err := RoundHalfUp(-1, 1000); err != ErrNegative {
		t.Errorf("negative numerator: want ErrNegative, got %v", err)
	}
}

func TestLineTotal(t *testing.T) {
	f := loadFixture(t)
	if len(f.LineTotal) == 0 {
		t.Fatal("fixture has no line_total cases")
	}

	for _, c := range f.LineTotal {
		t.Run(c.Name, func(t *testing.T) {
			got, err := LineTotal(c.UnitPriceCents, c.QtyMilli)
			if err != nil {
				t.Fatalf("LineTotal(%d, %d) returned error: %v", c.UnitPriceCents, c.QtyMilli, err)
			}
			if got != c.Want {
				t.Errorf("LineTotal(%d, %d) = %d, want %d",
					c.UnitPriceCents, c.QtyMilli, got, c.Want)
			}
		})
	}
}

func TestLineTotalOverflow(t *testing.T) {
	// Impossible values in this domain, but the silent failure mode (wrapping
	// the sign) would be an accounting disaster, so it gets detected.
	const big = int64(1) << 40
	if _, err := LineTotal(big, big); err != ErrOverflow {
		t.Errorf("want ErrOverflow, got %v", err)
	}
	if _, err := LineTotal(-1, 1000); err != ErrNegative {
		t.Errorf("negative price: want ErrNegative, got %v", err)
	}
}

func TestAllocatePayment(t *testing.T) {
	f := loadFixture(t)
	if len(f.AllocatePayment) == 0 {
		t.Fatal("fixture has no allocate_payment cases")
	}

	for _, c := range f.AllocatePayment {
		t.Run(c.Name, func(t *testing.T) {
			gotAllocs, gotCredit, err := AllocatePayment(c.PaymentCents, c.Debts)
			if err != nil {
				t.Fatalf("AllocatePayment returned error: %v", err)
			}

			if len(gotAllocs) != len(c.WantAllocations) {
				t.Fatalf("allocation count = %d, want %d (%+v)",
					len(gotAllocs), len(c.WantAllocations), gotAllocs)
			}
			for i := range gotAllocs {
				if gotAllocs[i] != c.WantAllocations[i] {
					t.Errorf("allocation %d = %+v, want %+v",
						i, gotAllocs[i], c.WantAllocations[i])
				}
			}
			if gotCredit != c.WantCreditCents {
				t.Errorf("credit = %d, want %d", gotCredit, c.WantCreditCents)
			}
		})
	}
}

// TestAllocatePaymentInvariant is the property test: over randomized splits, the
// sum of what was allocated plus the leftover credit must equal the payment
// exactly. Not a single cent is created or lost.
//
// This is the property that makes the greedy split defensible over a
// proportional one, which always leaves a stray cent with no clear owner.
func TestAllocatePaymentInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) // fixed seed: reproducible failures

	for i := 0; i < 100; i++ {
		payment := rng.Int63n(100_000)

		debts := make([]Debt, rng.Intn(8))
		for j := range debts {
			debts[j] = Debt{
				RefID:       string(rune('a' + j)),
				AmountCents: rng.Int63n(30_000),
			}
		}

		allocs, credit, err := AllocatePayment(payment, debts)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}

		if got := SumAllocations(allocs) + credit; got != payment {
			t.Fatalf("iteration %d: sum(allocations)=%d + credit=%d = %d, but payment was %d",
				i, SumAllocations(allocs), credit, got, payment)
		}

		// No allocation may exceed its own debt.
		byRef := make(map[string]int64, len(debts))
		for _, d := range debts {
			byRef[d.RefID] = d.AmountCents
		}
		for _, a := range allocs {
			if a.AmountCents > byRef[a.RefID] {
				t.Fatalf("iteration %d: allocated %d against a debt of %d",
					i, a.AmountCents, byRef[a.RefID])
			}
			if a.AmountCents <= 0 {
				t.Fatalf("iteration %d: non-positive allocation %d", i, a.AmountCents)
			}
		}
	}
}

func TestAllocatePaymentNegatives(t *testing.T) {
	if _, _, err := AllocatePayment(-1, nil); err != ErrNegative {
		t.Errorf("negative payment: want ErrNegative, got %v", err)
	}
	if _, _, err := AllocatePayment(100, []Debt{{RefID: "a", AmountCents: -5}}); err != ErrNegative {
		t.Errorf("negative debt: want ErrNegative, got %v", err)
	}
}
