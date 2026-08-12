// Package sales owns recording a sale.
//
// One sale is one transaction that writes, atomically: the sales row, its
// sale_lines, one stock_movement per line, and — when there is anything owed —
// one customer_ledger entry. Plus the change_log rows the sync feed ships.
//
// If any of those failed independently, stock or a customer's balance would
// silently disagree with the sale that caused it, and no retry would fix it.
package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bryandrm/store-system/internal/db"
	"github.com/bryandrm/store-system/internal/httpx"
	"github.com/bryandrm/store-system/internal/money"
)

// Payment methods, mirroring the CHECK constraint on sales.payment_method.
const (
	PaymentCash   = "cash"
	PaymentCredit = "credit"
	PaymentMixed  = "mixed"
)

// maxClockSkew is how far into the future a device clock may run before its
// timestamp gets clamped.
//
// A sale is NEVER rejected over a bad clock: losing a real sale because a phone
// is set wrong would be far worse than a misfiled report line.
const maxClockSkew = 5 * time.Minute

// LineInput is one line as the client recorded it.
type LineInput struct {
	ProductID      uuid.UUID `json:"product_id"`
	QtyMilli       int64     `json:"qty_milli"`
	UnitPriceCents int64     `json:"unit_price_cents"`
	LineTotalCents int64     `json:"line_total_cents"`
}

// CreateInput is a whole sale as the client recorded it.
//
// SaleID comes from the client (UUIDv7). That is what makes replaying an
// operation harmless and lets the outbox retry forever.
type CreateInput struct {
	SaleID          uuid.UUID   `json:"sale_id"`
	CustomerID      *uuid.UUID  `json:"customer_id"`
	Lines           []LineInput `json:"lines"`
	TotalCents      int64       `json:"total_cents"`
	PaidCents       int64       `json:"paid_cents"`
	PaymentMethod   string      `json:"payment_method"`
	Note            string      `json:"note"`
	OccurredAt      time.Time   `json:"occurred_at"`
	DeviceID        string      `json:"device_id"`
	CreatedByUserID uuid.UUID   `json:"created_by_user_id"`
}

// Validate checks everything that does not need the database.
func (in *CreateInput) Validate() []httpx.FieldDetail {
	var problems []httpx.FieldDetail

	if in.SaleID == uuid.Nil {
		problems = append(problems, httpx.FieldDetail{Field: "sale_id", Code: "REQUIRED"})
	}
	if len(in.Lines) == 0 {
		problems = append(problems, httpx.FieldDetail{Field: "lines", Code: "EMPTY"})
	}
	if in.OccurredAt.IsZero() {
		problems = append(problems, httpx.FieldDetail{Field: "occurred_at", Code: "REQUIRED"})
	}
	if in.DeviceID == "" {
		problems = append(problems, httpx.FieldDetail{Field: "device_id", Code: "REQUIRED"})
	}

	switch in.PaymentMethod {
	case PaymentCash:
		if in.PaidCents != in.TotalCents {
			problems = append(problems, httpx.FieldDetail{Field: "paid_cents", Code: "CASH_MUST_BE_FULLY_PAID"})
		}
	case PaymentCredit:
		if in.PaidCents != 0 {
			problems = append(problems, httpx.FieldDetail{Field: "paid_cents", Code: "CREDIT_MUST_BE_UNPAID"})
		}
	case PaymentMixed:
		if in.PaidCents <= 0 || in.PaidCents >= in.TotalCents {
			problems = append(problems, httpx.FieldDetail{Field: "paid_cents", Code: "MIXED_MUST_BE_PARTIAL"})
		}
	default:
		problems = append(problems, httpx.FieldDetail{Field: "payment_method", Code: "INVALID"})
	}

	// Anything not settled in cash has to be owed by somebody.
	if in.PaymentMethod != PaymentCash && in.CustomerID == nil {
		problems = append(problems, httpx.FieldDetail{Field: "customer_id", Code: "REQUIRED_FOR_CREDIT"})
	}

	for i, l := range in.Lines {
		field := fmt.Sprintf("lines[%d]", i)
		if l.ProductID == uuid.Nil {
			problems = append(problems, httpx.FieldDetail{Field: field + ".product_id", Code: "REQUIRED"})
		}
		if l.QtyMilli <= 0 {
			problems = append(problems, httpx.FieldDetail{Field: field + ".qty_milli", Code: "MUST_BE_POSITIVE"})
		}
		if l.UnitPriceCents < 0 {
			problems = append(problems, httpx.FieldDetail{Field: field + ".unit_price_cents", Code: "MUST_NOT_BE_NEGATIVE"})
		}
	}

	return problems
}

// Create records a sale. It must run inside a transaction.
//
// Returns nil when the sale already exists: two devices replaying the same
// operation converge on one sale rather than on an error.
func Create(ctx context.Context, tx pgx.Tx, in CreateInput, syncedByUserID uuid.UUID) error {
	if problems := in.Validate(); len(problems) > 0 {
		return httpx.ErrValidation.WithDetails(problems...)
	}

	products, err := loadProducts(ctx, tx, in.Lines)
	if err != nil {
		return err
	}

	if err := verifyTotals(in); err != nil {
		return err
	}

	occurredAt, skewed := clampOccurredAt(in.OccurredAt)

	// ON CONFLICT DO NOTHING plus RETURNING: if nothing comes back the sale was
	// already recorded, so every dependent write is skipped too. This is what
	// makes replay safe at the row level, on top of the sync_operations ledger.
	var insertedID uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO sales (id, customer_id, total_cents, paid_cents, payment_method,
		                    note, occurred_at, clock_skew_flagged, device_id,
		                    created_by_user_id, synced_by_user_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (id) DO NOTHING
		 RETURNING id`,
		in.SaleID, in.CustomerID, in.TotalCents, in.PaidCents, in.PaymentMethod,
		nullIfEmpty(in.Note), occurredAt, skewed, in.DeviceID,
		in.CreatedByUserID, syncedByUserID,
	).Scan(&insertedID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already recorded
	}
	if err != nil {
		return fmt.Errorf("sales: could not insert sale: %w", err)
	}

	for i, l := range in.Lines {
		lineNo := i + 1
		lineID := uuid.Must(uuid.NewV7())
		product := products[l.ProductID]

		if _, err := tx.Exec(ctx,
			`INSERT INTO sale_lines (id, sale_id, product_id, qty_milli, unit_price_cents,
			                         line_total_cents, product_name_snapshot, line_no)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			lineID, in.SaleID, l.ProductID, l.QtyMilli, l.UnitPriceCents,
			l.LineTotalCents, product.name, lineNo); err != nil {
			return fmt.Errorf("sales: could not insert line %d: %w", lineNo, err)
		}

		// Stock is derived, so selling is just a negative entry in the ledger.
		movementID := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx,
			`INSERT INTO stock_movements (id, product_id, delta_qty_milli, reason,
			                              ref_kind, ref_id, occurred_at, created_by_user_id)
			 VALUES ($1,$2,$3,'sale','sale',$4,$5,$6)`,
			movementID, l.ProductID, -l.QtyMilli, in.SaleID, occurredAt,
			in.CreatedByUserID); err != nil {
			return fmt.Errorf("sales: could not insert stock movement for line %d: %w", lineNo, err)
		}
	}

	// Whatever is left unpaid becomes debt: a negative entry on the customer's
	// ledger. The balance itself is never stored.
	if owed := in.TotalCents - in.PaidCents; owed > 0 {
		ledgerID := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx,
			`INSERT INTO customer_ledger (id, customer_id, delta_cents, kind,
			                              ref_kind, ref_id, occurred_at, created_by_user_id)
			 VALUES ($1,$2,$3,'sale_credit','sale',$4,$5,$6)`,
			ledgerID, *in.CustomerID, -owed, in.SaleID, occurredAt,
			in.CreatedByUserID); err != nil {
			return fmt.Errorf("sales: could not insert ledger entry: %w", err)
		}
	}

	return db.RecordChange(ctx, tx, db.EntitySale, in.SaleID, db.OpInsert, in)
}

type productInfo struct {
	name     string
	isActive bool
}

// loadProducts fetches every product referenced by the sale in one query.
func loadProducts(ctx context.Context, tx pgx.Tx, lines []LineInput) (map[uuid.UUID]productInfo, error) {
	ids := make([]uuid.UUID, 0, len(lines))
	seen := make(map[uuid.UUID]bool, len(lines))
	for _, l := range lines {
		if !seen[l.ProductID] {
			seen[l.ProductID] = true
			ids = append(ids, l.ProductID)
		}
	}

	rows, err := tx.Query(ctx,
		`SELECT id, name, is_active FROM products WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("sales: could not read products: %w", err)
	}
	defer rows.Close()

	found := make(map[uuid.UUID]productInfo, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var p productInfo
		if err := rows.Scan(&id, &p.name, &p.isActive); err != nil {
			return nil, fmt.Errorf("sales: could not scan product: %w", err)
		}
		found[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sales: could not read products: %w", err)
	}

	for _, id := range ids {
		p, ok := found[id]
		if !ok {
			return nil, httpx.ErrUnknownProduct.
				WithDetails(httpx.FieldDetail{Field: id.String(), Code: "UNKNOWN_PRODUCT"})
		}
		// An inactive product may still be sold if it was already in the cart
		// when it got hidden; what matters is that it existed. Rejecting here
		// would throw away a sale that physically happened.
		_ = p.isActive
	}

	return found, nil
}

// verifyTotals recomputes the arithmetic the client sent.
//
// A difference of one cent is tolerated and the CLIENT's number is kept: the
// figure quoted to the buyer is what actually happened. Anything larger is a
// bug worth surfacing rather than papering over.
func verifyTotals(in CreateInput) error {
	var computedTotal int64

	for i, l := range in.Lines {
		want, err := money.LineTotal(l.UnitPriceCents, l.QtyMilli)
		if err != nil {
			return httpx.ErrValidation.
				WithDetails(httpx.FieldDetail{
					Field: fmt.Sprintf("lines[%d]", i), Code: "BAD_ARITHMETIC"}).
				WithCause(err)
		}
		if diff := want - l.LineTotalCents; diff > 1 || diff < -1 {
			return httpx.ErrTotalMismatch.WithDetails(httpx.FieldDetail{
				Field: fmt.Sprintf("lines[%d].line_total_cents", i),
				Code:  "TOTAL_MISMATCH",
			})
		}
		computedTotal += l.LineTotalCents
	}

	if diff := computedTotal - in.TotalCents; diff > 1 || diff < -1 {
		return httpx.ErrTotalMismatch.WithDetails(httpx.FieldDetail{
			Field: "total_cents", Code: "TOTAL_MISMATCH",
		})
	}

	return nil
}

// clampOccurredAt pulls a future timestamp back to now and flags it.
//
// Sync itself is immune to clock skew because the cursor rides on transaction
// ids, but occurred_at is not, and it is what every report groups by.
func clampOccurredAt(t time.Time) (time.Time, bool) {
	if t.After(time.Now().Add(maxClockSkew)) {
		return time.Now(), true
	}
	return t, false
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
