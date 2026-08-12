package sync

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Snapshot is the full local replica a fresh device receives.
//
// Every client holds all of it, which is why the API has so few read endpoints:
// reports, sales lists and customer statements are all computed on the device.
type Snapshot struct {
	Products       []json.RawMessage `json:"products"`
	Prices         []json.RawMessage `json:"prices"`
	Customers      []json.RawMessage `json:"customers"`
	Sales          []json.RawMessage `json:"sales"`
	SaleLines      []json.RawMessage `json:"sale_lines"`
	StockMovements []json.RawMessage `json:"stock_movements"`
	CustomerLedger []json.RawMessage `json:"customer_ledger"`
	Payments       []json.RawMessage `json:"payments"`
}

// BootstrapResult is the snapshot plus the cursor the client starts syncing from.
type BootstrapResult struct {
	Snapshot Snapshot `json:"snapshot"`
	Cursor   string   `json:"cursor"`
}

// Bootstrap returns a complete replica and the cursor to continue from.
//
// This one DOES need REPEATABLE READ, unlike the feed: the watermark and the
// data must come from the same snapshot. Rows written by transactions at or
// above the watermark that happen to be visible here get re-delivered by the
// first feed call.
//
// The safety argument, stated plainly: bootstrap may over-deliver, never
// under-deliver, and applying is idempotent, so over-delivery costs nothing.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) (BootstrapResult, error) {
	var result BootstrapResult

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return result, fmt.Errorf("sync: could not open the bootstrap snapshot: %w", err)
	}
	// Read-only and short-lived, but the rollback still matters: an open
	// transaction pins xmin and freezes the feed for every device.
	defer func() { _ = tx.Rollback(ctx) }()

	watermark, err := Watermark(ctx, tx)
	if err != nil {
		return result, err
	}

	loaders := []struct {
		dest  *[]json.RawMessage
		query string
	}{
		{&result.Snapshot.Products, `
			SELECT to_jsonb(p) FROM products p ORDER BY p.sort_order, p.name`},
		{&result.Snapshot.Prices, `
			SELECT to_jsonb(c) FROM current_prices c`},
		{&result.Snapshot.Customers, `
			SELECT to_jsonb(c) FROM customers c ORDER BY c.name`},
		{&result.Snapshot.Sales, `
			SELECT to_jsonb(s) FROM sales s ORDER BY s.occurred_at DESC`},
		{&result.Snapshot.SaleLines, `
			SELECT to_jsonb(l) FROM sale_lines l`},
		{&result.Snapshot.StockMovements, `
			SELECT to_jsonb(m) FROM stock_movements m`},
		{&result.Snapshot.CustomerLedger, `
			SELECT to_jsonb(e) FROM customer_ledger e`},
		{&result.Snapshot.Payments, `
			SELECT to_jsonb(p) FROM payments p`},
	}

	for _, l := range loaders {
		rows, err := loadJSONRows(ctx, tx, l.query)
		if err != nil {
			return result, err
		}
		*l.dest = rows
	}

	result.Cursor = fmt.Sprintf("%d", watermark)
	return result, nil
}

func loadJSONRows(ctx context.Context, tx pgx.Tx, query string) ([]json.RawMessage, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap query failed: %w", err)
	}
	defer rows.Close()

	// Never nil: the client should receive [] rather than null, so it does not
	// have to special-case an empty table.
	out := make([]json.RawMessage, 0)
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sync: could not scan a bootstrap row: %w", err)
		}
		out = append(out, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: bootstrap query failed: %w", err)
	}
	return out, nil
}
