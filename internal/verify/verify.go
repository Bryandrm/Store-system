// Package verify checks the invariants that no CHECK constraint can express.
//
// A CHECK sees one row. It can say "price_cents must not be negative". It
// cannot say "the sum of these lines equals that sale's total", because that
// spans rows in another table.
//
// Those are exactly the invariants that matter here, because recording a sale
// writes four tables in one transaction. If they ever drift apart, the books
// lie while every individual row still looks valid.
//
// The division of labour is worth stating: the REVOKEs in the migration stop a
// single value from being CORRUPTED; this package detects two individually
// valid values that have become INCOHERENT with each other.
//
// Runs in three places, and the third is what justifies the other two:
//  1. CI, after the integration tests — a feature that breaks an invariant
//     never reaches main.
//  2. Production, nightly, before the backup.
//  3. Against every restored backup. A backup that restores with broken
//     invariants is worse than no backup, because it manufactures confidence.
//     Counting rows does not catch that; this does.
//
// See docs/DECISIONS.md ADR-010 for why this is a job rather than triggers.
package verify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Invariant is one property that must hold across the whole database.
//
// Query must return one row per VIOLATION, with a single text column naming the
// offending record. No rows means the invariant holds.
type Invariant struct {
	Name        string
	Description string
	Query       string
}

// Violation is an invariant that failed, with examples.
type Violation struct {
	Invariant   string
	Description string
	Count       int
	Samples     []string
}

// maxSamples caps how many offending rows are reported per invariant. Enough to
// debug with; not so many that a systemic failure buries the summary.
const maxSamples = 5

// Invariants is the complete list, in the order they are reported.
//
// Each one exists because a specific mistake would otherwise be invisible. The
// comments say which mistake, so a future reader can judge whether a change to
// the schema makes the check obsolete or more necessary.
var Invariants = []Invariant{
	{
		Name:        "sale_lines_sum_to_total",
		Description: "Each sale's total equals the sum of its lines",
		// Catches a sale written with a total that no longer matches what was
		// actually charged. Tolerates one cent, because the server deliberately
		// keeps the client's figure when they differ by that much: the number
		// quoted to the buyer is what happened.
		Query: `
			SELECT s.id::text || ' total=' || s.total_cents ||
			       ' lines=' || COALESCE(SUM(l.line_total_cents), 0)
			FROM sales s
			LEFT JOIN sale_lines l ON l.sale_id = s.id
			GROUP BY s.id, s.total_cents
			HAVING ABS(s.total_cents - COALESCE(SUM(l.line_total_cents), 0)) > 1`,
	},
	{
		Name:        "sale_has_at_least_one_line",
		Description: "No sale exists without lines",
		// A sale with no lines means the transaction was half-applied, which
		// should be impossible but would be invisible in any total.
		Query: `
			SELECT s.id::text
			FROM sales s
			WHERE NOT EXISTS (SELECT 1 FROM sale_lines l WHERE l.sale_id = s.id)`,
	},
	{
		Name:        "line_total_matches_arithmetic",
		Description: "Each line total equals unit price times quantity, rounded half-up",
		// Catches a client whose money arithmetic drifted from the server's.
		// The shared fixture is supposed to prevent that; this is the check
		// that would notice if it ever stopped being shared.
		Query: `
			SELECT l.id::text || ' stored=' || l.line_total_cents ||
			       ' computed=' || ((l.unit_price_cents * l.qty_milli + 500) / 1000)
			FROM sale_lines l
			WHERE ABS(l.line_total_cents - ((l.unit_price_cents * l.qty_milli + 500) / 1000)) > 1`,
	},
	{
		Name:        "sale_line_moved_stock",
		Description: "Every sale line has a matching stock movement",
		// Stock is derived from movements, so a line without one means product
		// left the shelf and the system still thinks it is there.
		Query: `
			SELECT l.id::text || ' product=' || l.product_id::text
			FROM sale_lines l
			WHERE NOT EXISTS (
				SELECT 1 FROM stock_movements m
				WHERE m.ref_kind = 'sale' AND m.ref_id = l.sale_id
				  AND m.product_id = l.product_id
			)`,
	},
	{
		Name:        "sale_stock_movement_matches_quantity",
		Description: "The stock movement for a line matches the quantity sold",
		// Catches a movement written with the wrong sign or magnitude, which
		// would silently inflate or deflate stock.
		Query: `
			SELECT l.id::text || ' qty=' || l.qty_milli || ' moved=' || m.delta_qty_milli
			FROM sale_lines l
			JOIN stock_movements m
			  ON m.ref_kind = 'sale' AND m.ref_id = l.sale_id
			 AND m.product_id = l.product_id AND m.reason = 'sale'
			WHERE m.delta_qty_milli <> -l.qty_milli`,
	},
	{
		Name:        "credit_sale_has_matching_ledger_entry",
		Description: "Every unpaid amount has exactly one ledger entry for it",
		// This is the one that protects what customers owe. A missing entry
		// forgives a debt; a duplicate charges it twice.
		Query: `
			SELECT s.id::text || ' owed=' || (s.total_cents - s.paid_cents) ||
			       ' ledger=' || COALESCE(SUM(cl.delta_cents), 0) ||
			       ' entries=' || COUNT(cl.id)
			FROM sales s
			LEFT JOIN customer_ledger cl
			  ON cl.ref_kind = 'sale' AND cl.ref_id = s.id AND cl.kind = 'sale_credit'
			WHERE s.paid_cents < s.total_cents
			GROUP BY s.id, s.total_cents, s.paid_cents
			HAVING COUNT(cl.id) <> 1
			    OR COALESCE(SUM(cl.delta_cents), 0) <> -(s.total_cents - s.paid_cents)`,
	},
	{
		Name:        "cash_sale_has_no_debt",
		Description: "A fully paid sale creates no debt entry",
		// The mirror of the above: charging a customer for a sale they already
		// paid in cash.
		Query: `
			SELECT s.id::text
			FROM sales s
			JOIN customer_ledger cl
			  ON cl.ref_kind = 'sale' AND cl.ref_id = s.id AND cl.kind = 'sale_credit'
			WHERE s.paid_cents >= s.total_cents`,
	},
	{
		Name:        "payment_has_matching_ledger_entry",
		Description: "Every payment has exactly one ledger entry for its amount",
		// A missing entry means money was taken and never credited.
		Query: `
			SELECT p.id::text || ' amount=' || p.amount_cents ||
			       ' ledger=' || COALESCE(SUM(cl.delta_cents), 0)
			FROM payments p
			LEFT JOIN customer_ledger cl
			  ON cl.ref_kind = 'payment' AND cl.ref_id = p.id AND cl.kind = 'payment'
			GROUP BY p.id, p.amount_cents
			HAVING COUNT(cl.id) <> 1
			    OR COALESCE(SUM(cl.delta_cents), 0) <> p.amount_cents`,
	},
	{
		Name:        "voided_sale_fully_compensated",
		Description: "A voided sale reverses its stock exactly once",
		// Voiding twice from two devices must converge on one set of
		// compensations. Over-compensating would invent stock that never existed.
		Query: `
			SELECT v.sale_id::text || ' lines=' || (
			         SELECT COUNT(*) FROM sale_lines l WHERE l.sale_id = v.sale_id
			       ) || ' compensations=' || (
			         SELECT COUNT(*) FROM stock_movements m
			         WHERE m.ref_kind = 'sale' AND m.ref_id = v.sale_id
			           AND m.reason = 'sale_void'
			       )
			FROM sale_voids v
			WHERE (SELECT COUNT(*) FROM stock_movements m
			       WHERE m.ref_kind = 'sale' AND m.ref_id = v.sale_id
			         AND m.reason = 'sale_void')
			   <> (SELECT COUNT(*) FROM sale_lines l WHERE l.sale_id = v.sale_id)`,
	},
	{
		Name:        "stock_movement_references_real_sale",
		Description: "No stock movement points at a sale that does not exist",
		// A dangling reference means stock moved for a reason nobody can trace.
		Query: `
			SELECT m.id::text || ' ref=' || m.ref_id::text
			FROM stock_movements m
			WHERE m.ref_kind = 'sale'
			  AND NOT EXISTS (SELECT 1 FROM sales s WHERE s.id = m.ref_id)`,
	},
	{
		Name:        "ledger_entry_references_real_record",
		Description: "No ledger entry points at a sale or payment that does not exist",
		Query: `
			SELECT cl.id::text || ' ' || cl.ref_kind || '=' || cl.ref_id::text
			FROM customer_ledger cl
			WHERE (cl.ref_kind = 'sale'
			       AND NOT EXISTS (SELECT 1 FROM sales s WHERE s.id = cl.ref_id))
			   OR (cl.ref_kind = 'payment'
			       AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.id = cl.ref_id))`,
	},
	{
		Name:        "every_product_has_a_price",
		Description: "No sellable product is missing a price",
		// A product with no price shows as $0.00 on the sell screen, which is a
		// giveaway rather than an error message.
		Query: `
			SELECT p.id::text || ' ' || p.name
			FROM products p
			WHERE p.is_active
			  AND NOT EXISTS (
			    SELECT 1 FROM product_prices pp
			    WHERE pp.product_id = p.id AND pp.effective_from <= now()
			  )`,
	},
}

// Run checks every invariant and returns the ones that failed.
//
// It keeps going after a violation rather than stopping at the first: when
// something has gone wrong, knowing the full extent matters more than knowing
// the first symptom.
func Run(ctx context.Context, pool *pgxpool.Pool) ([]Violation, error) {
	var violations []Violation

	for _, inv := range Invariants {
		rows, err := pool.Query(ctx, inv.Query)
		if err != nil {
			return nil, fmt.Errorf("verify: invariant %q failed to run: %w", inv.Name, err)
		}

		var samples []string
		count := 0
		for rows.Next() {
			var detail string
			if err := rows.Scan(&detail); err != nil {
				rows.Close()
				return nil, fmt.Errorf("verify: invariant %q: %w", inv.Name, err)
			}
			count++
			if len(samples) < maxSamples {
				samples = append(samples, detail)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("verify: invariant %q: %w", inv.Name, err)
		}
		rows.Close()

		if count > 0 {
			violations = append(violations, Violation{
				Invariant:   inv.Name,
				Description: inv.Description,
				Count:       count,
				Samples:     samples,
			})
		}
	}

	return violations, nil
}
