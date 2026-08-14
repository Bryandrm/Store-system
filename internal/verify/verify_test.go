package verify_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bryandrm/store-system/internal/db"
	"github.com/bryandrm/store-system/internal/sales"
	"github.com/bryandrm/store-system/internal/testdb"
	"github.com/bryandrm/store-system/internal/verify"
)

// fixture builds a small but complete world: an owner, two products with
// prices, a customer, one cash sale and one credit sale.
type fixture struct {
	tdb        *testdb.DB
	userID     uuid.UUID
	customerID uuid.UUID
	productA   uuid.UUID
	productB   uuid.UUID
	cashSale   uuid.UUID
	creditSale uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{
		tdb:        testdb.New(t),
		userID:     uuid.Must(uuid.NewV7()),
		customerID: uuid.Must(uuid.NewV7()),
		productA:   uuid.Must(uuid.NewV7()),
		productB:   uuid.Must(uuid.NewV7()),
		cashSale:   uuid.Must(uuid.NewV7()),
		creditSale: uuid.Must(uuid.NewV7()),
	}
	ctx := context.Background()

	mustExec(t, f, `INSERT INTO users (id, username, display_name, password_hash, role)
	                VALUES ($1, 'bryan', 'Bryan', 'x', 'owner')`, f.userID)
	mustExec(t, f, `INSERT INTO customers (id, name) VALUES ($1, 'Cliente')`, f.customerID)

	for i, id := range []uuid.UUID{f.productA, f.productB} {
		mustExec(t, f, `INSERT INTO products (id, name, sort_order) VALUES ($1, $2, $3)`,
			id, "producto "+string(rune('A'+i)), i)
		mustExec(t, f, `INSERT INTO product_prices
		                (id, product_id, price_cents, effective_from, created_by_user_id)
		                VALUES ($1, $2, 50, now(), $3)`,
			uuid.Must(uuid.NewV7()), id, f.userID)
	}

	now := time.Now()

	// 2 x 0.50 = 1.00, paid in cash.
	mustCreateSale(t, f, sales.CreateInput{
		SaleID:        f.cashSale,
		TotalCents:    100,
		PaidCents:     100,
		PaymentMethod: sales.PaymentCash,
		OccurredAt:    now,
		DeviceID:      "test",
		Lines: []sales.LineInput{
			{ProductID: f.productA, QtyMilli: 2000, UnitPriceCents: 50, LineTotalCents: 100},
		},
		CreatedByUserID: f.userID,
	})

	// 3 x 0.50 = 1.50, entirely on credit.
	mustCreateSale(t, f, sales.CreateInput{
		SaleID:        f.creditSale,
		CustomerID:    &f.customerID,
		TotalCents:    150,
		PaidCents:     0,
		PaymentMethod: sales.PaymentCredit,
		OccurredAt:    now,
		DeviceID:      "test",
		Lines: []sales.LineInput{
			{ProductID: f.productB, QtyMilli: 3000, UnitPriceCents: 50, LineTotalCents: 150},
		},
		CreatedByUserID: f.userID,
	})

	_ = ctx
	return f
}

func mustExec(t *testing.T, f *fixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.tdb.App.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
}

func mustCreateSale(t *testing.T, f *fixture, in sales.CreateInput) {
	t.Helper()
	err := db.WithTx(context.Background(), f.tdb.App, func(tx pgx.Tx) error {
		return sales.Create(context.Background(), tx, in, f.userID)
	})
	if err != nil {
		t.Fatalf("could not create the sale: %v", err)
	}
}

// corrupt runs a statement as the migrator role, which — unlike the application
// role — is allowed to UPDATE and DELETE transactional tables.
//
// That is the point of these tests: the REVOKEs make this corruption impossible
// through the application, so the only way to prove the checker works is to
// reach around them.
func (f *fixture) corrupt(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.tdb.Admin.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("could not corrupt the data: %v", err)
	}
}

func (f *fixture) run(t *testing.T) []verify.Violation {
	t.Helper()
	violations, err := verify.Run(context.Background(), f.tdb.App)
	if err != nil {
		t.Fatalf("verify.Run: %v", err)
	}
	return violations
}

// requireViolation asserts that a specific invariant fired, and only it.
//
// Checking that ONLY it fired matters: an invariant that trips on unrelated
// damage produces noise, and noisy checks stop being read.
func requireViolation(t *testing.T, violations []verify.Violation, name string) {
	t.Helper()

	var names []string
	found := false
	for _, v := range violations {
		names = append(names, v.Invariant)
		if v.Invariant == name {
			found = true
		}
	}

	if !found {
		t.Fatalf("expected invariant %q to be violated; violations were %v", name, names)
	}
	if len(violations) != 1 {
		t.Errorf("expected only %q to fire, but got %v", name, names)
	}
}

// TestCleanDatabasePasses is the baseline. Without it, every test below could
// pass simply because the checker fires on everything.
func TestCleanDatabasePasses(t *testing.T) {
	f := newFixture(t)

	if violations := f.run(t); len(violations) > 0 {
		for _, v := range violations {
			t.Errorf("unexpected violation %q: %v", v.Invariant, v.Samples)
		}
	}
}

func TestEmptyDatabasePasses(t *testing.T) {
	tdb := testdb.New(t)

	violations, err := verify.Run(context.Background(), tdb.App)
	if err != nil {
		t.Fatalf("verify.Run: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("an empty database should hold every invariant, got %v", violations)
	}
}

// TestDetectsTotalNotMatchingLines: the sale claims a total its lines do not
// add up to. Every individual row still passes its CHECK constraints.
func TestDetectsTotalNotMatchingLines(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `UPDATE sales SET total_cents = 999, paid_cents = 999 WHERE id = $1`, f.cashSale)

	requireViolation(t, f.run(t), "sale_lines_sum_to_total")
}

// TestToleratesOneCentDifference: the server deliberately keeps the client's
// figure when the two differ by a cent, so the checker must not flag it.
func TestToleratesOneCentDifference(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `UPDATE sales SET total_cents = 101, paid_cents = 101 WHERE id = $1`, f.cashSale)

	for _, v := range f.run(t) {
		if v.Invariant == "sale_lines_sum_to_total" {
			t.Error("a one-cent difference should be tolerated, not reported")
		}
	}
}

// TestDetectsSaleWithoutLines: a half-applied transaction, invisible in totals.
func TestDetectsSaleWithoutLines(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `DELETE FROM sale_lines WHERE sale_id = $1`, f.cashSale)

	violations := f.run(t)
	names := map[string]bool{}
	for _, v := range violations {
		names[v.Invariant] = true
	}
	if !names["sale_has_at_least_one_line"] {
		t.Errorf("expected sale_has_at_least_one_line, got %v", violations)
	}
	// Deleting the lines also breaks the total, which is correct and worth
	// asserting: the checks overlap on purpose so one deletion cannot slip
	// through by breaking only the check nobody looks at.
	if !names["sale_lines_sum_to_total"] {
		t.Errorf("expected the total check to fire as well, got %v", violations)
	}
}

// TestDetectsMissingStockMovement: product left the shelf and the system still
// believes it is there. This is the one that silently inflates inventory.
func TestDetectsMissingStockMovement(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `DELETE FROM stock_movements WHERE ref_id = $1`, f.cashSale)

	requireViolation(t, f.run(t), "sale_line_moved_stock")
}

// TestDetectsWrongStockQuantity: the movement exists but moved the wrong
// amount. Catches a sign error, which is the easiest way to invent stock.
func TestDetectsWrongStockQuantity(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t,
		`UPDATE stock_movements SET delta_qty_milli = 2000 WHERE ref_id = $1`, f.cashSale)

	requireViolation(t, f.run(t), "sale_stock_movement_matches_quantity")
}

// TestDetectsForgivenDebt: the ledger entry for an unpaid sale disappears. The
// customer owes money and the system has forgotten.
func TestDetectsForgivenDebt(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `DELETE FROM customer_ledger WHERE ref_id = $1`, f.creditSale)

	requireViolation(t, f.run(t), "credit_sale_has_matching_ledger_entry")
}

// TestDetectsDoubleChargedDebt: a duplicate entry bills the customer twice.
func TestDetectsDoubleChargedDebt(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `
		INSERT INTO customer_ledger
		  (id, customer_id, delta_cents, kind, ref_kind, ref_id, occurred_at, created_by_user_id)
		SELECT gen_random_uuid(), customer_id, delta_cents, kind, ref_kind, ref_id,
		       occurred_at, created_by_user_id
		FROM customer_ledger WHERE ref_id = $1`, f.creditSale)

	requireViolation(t, f.run(t), "credit_sale_has_matching_ledger_entry")
}

// TestDetectsWrongDebtAmount: the entry exists but for the wrong figure.
func TestDetectsWrongDebtAmount(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `UPDATE customer_ledger SET delta_cents = -75 WHERE ref_id = $1`, f.creditSale)

	requireViolation(t, f.run(t), "credit_sale_has_matching_ledger_entry")
}

// TestDetectsDebtOnAPaidSale: charging somebody for a sale settled in cash.
func TestDetectsDebtOnAPaidSale(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `
		INSERT INTO customer_ledger
		  (id, customer_id, delta_cents, kind, ref_kind, ref_id, occurred_at, created_by_user_id)
		VALUES (gen_random_uuid(), $1, -100, 'sale_credit', 'sale', $2, now(), $3)`,
		f.customerID, f.cashSale, f.userID)

	requireViolation(t, f.run(t), "cash_sale_has_no_debt")
}

// TestDetectsDanglingStockMovement: stock moved for a reason nobody can trace.
func TestDetectsDanglingStockMovement(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `
		INSERT INTO stock_movements
		  (id, product_id, delta_qty_milli, reason, ref_kind, ref_id, occurred_at, created_by_user_id)
		VALUES (gen_random_uuid(), $1, -1000, 'sale', 'sale', gen_random_uuid(), now(), $2)`,
		f.productA, f.userID)

	requireViolation(t, f.run(t), "stock_movement_references_real_sale")
}

// TestDetectsProductWithoutPrice: it would show as $0.00 on the sell screen,
// which is a giveaway rather than an error message.
func TestDetectsProductWithoutPrice(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `DELETE FROM product_prices WHERE product_id = $1`, f.productA)

	requireViolation(t, f.run(t), "every_product_has_a_price")
}

// TestDetectsWrongLineArithmetic: a line whose total does not match price times
// quantity. This is what would fire if the shared money fixture ever stopped
// being shared and the two implementations drifted.
func TestDetectsWrongLineArithmetic(t *testing.T) {
	f := newFixture(t)
	f.corrupt(t, `
		UPDATE sale_lines SET line_total_cents = 40, unit_price_cents = 50, qty_milli = 2000
		WHERE sale_id = $1`, f.cashSale)

	violations := f.run(t)
	names := map[string]bool{}
	for _, v := range violations {
		names[v.Invariant] = true
	}
	if !names["line_total_matches_arithmetic"] {
		t.Errorf("expected line_total_matches_arithmetic, got %v", violations)
	}
}

// TestEveryInvariantHasADescription guards the operator experience: a violation
// reported by name alone, at 2am, against a restored backup, is not actionable.
func TestEveryInvariantHasADescription(t *testing.T) {
	seen := map[string]bool{}
	for _, inv := range verify.Invariants {
		if inv.Name == "" || inv.Description == "" || inv.Query == "" {
			t.Errorf("incomplete invariant: %+v", inv)
		}
		if seen[inv.Name] {
			t.Errorf("duplicate invariant name %q", inv.Name)
		}
		seen[inv.Name] = true
	}
}
