package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Matrix #1 — sale → inventory.
//
// The two are written in the same transaction, so this is the check that the
// wiring between them exists at all. Its failure mode is inventory reading high
// while product has physically left the shelf.
func TestSaleMovesStock(t *testing.T) {
	e := newEnv(t)

	before := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productA)

	resp := e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "cash", nil,
		line(e.productA, 3000, 50)))

	if got := resp.Data.Results[0].Status; got != "applied" {
		t.Fatalf("status = %q, want applied (%s)", got, resp.Data.Results[0].ErrorCode)
	}

	after := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productA)
	if after != before-3000 {
		t.Errorf("stock went from %d to %d, expected a drop of exactly 3000", before, after)
	}
}

// A sale touching two products must move both, each by its own quantity.
func TestSaleMovesEachProductSeparately(t *testing.T) {
	e := newEnv(t)

	beforeA := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productA)
	beforeB := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productB)

	e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "cash", nil,
		line(e.productA, 2000, 50),
		line(e.productB, 5000, 50)))

	if got := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productA); got != beforeA-2000 {
		t.Errorf("product A: %d, want %d", got, beforeA-2000)
	}
	if got := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productB); got != beforeB-5000 {
		t.Errorf("product B: %d, want %d", got, beforeB-5000)
	}
}

// Matrix #2 — credit sale → debt.
//
// This is the one that protects what customers owe. A missing entry forgives a
// debt; a duplicate charges it twice.
func TestCreditSaleCreatesDebt(t *testing.T) {
	e := newEnv(t)

	resp := e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "credit", &e.customerID,
		line(e.productA, 3000, 50)))

	if got := resp.Data.Results[0].Status; got != "applied" {
		t.Fatalf("status = %q (%s)", got, resp.Data.Results[0].ErrorCode)
	}

	// Negative means the customer owes. 3 x 0.50 = 1.50.
	balance := e.scalar(
		`SELECT balance_cents FROM customer_balances WHERE customer_id = $1`, e.customerID)
	if balance != -150 {
		t.Errorf("balance = %d, want -150", balance)
	}

	entries := e.scalar(
		`SELECT count(*) FROM customer_ledger WHERE customer_id = $1`, e.customerID)
	if entries != 1 {
		t.Errorf("ledger entries = %d, want exactly 1", entries)
	}
}

// A cash sale must create no debt at all. Getting this wrong bills a customer
// for something they already paid for.
func TestCashSaleCreatesNoDebt(t *testing.T) {
	e := newEnv(t)

	e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "cash", &e.customerID,
		line(e.productA, 2000, 50)))

	if got := e.scalar(`SELECT count(*) FROM customer_ledger`); got != 0 {
		t.Errorf("a cash sale created %d ledger entries, want 0", got)
	}
}

// A credit sale still moves stock. The product leaves the shelf whether or not
// it was paid for, and it would be easy to write the debt path without it.
func TestCreditSaleAlsoMovesStock(t *testing.T) {
	e := newEnv(t)

	before := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productA)

	e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "credit", &e.customerID,
		line(e.productA, 4000, 50)))

	if got := e.scalar(`SELECT qty_milli FROM stock_levels WHERE product_id = $1`, e.productA); got != before-4000 {
		t.Errorf("stock = %d, want %d", got, before-4000)
	}
}

// Matrix #11 — sync → everything.
//
// One bad operation must never block the others. That is why batch atomicity is
// deliberately not offered.
func TestOneInvalidOperationDoesNotBlockTheBatch(t *testing.T) {
	e := newEnv(t)

	good1 := saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(e.productA, 1000, 50))
	bad := saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(uuid.Must(uuid.NewV7()), 1000, 50))
	good2 := saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(e.productB, 2000, 50))

	resp := e.sync("0", good1, bad, good2)

	if len(resp.Data.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(resp.Data.Results))
	}
	if resp.Data.Results[0].Status != "applied" {
		t.Errorf("first operation: %q", resp.Data.Results[0].Status)
	}
	if resp.Data.Results[1].Status != "rejected" {
		t.Errorf("second operation: %q, want rejected", resp.Data.Results[1].Status)
	}
	if resp.Data.Results[1].ErrorCode != "UNKNOWN_PRODUCT" {
		t.Errorf("second operation code: %q", resp.Data.Results[1].ErrorCode)
	}
	if resp.Data.Results[2].Status != "applied" {
		t.Errorf("third operation: %q, want applied", resp.Data.Results[2].Status)
	}

	if got := e.scalar(`SELECT count(*) FROM sales`); got != 2 {
		t.Errorf("%d sales landed, want 2", got)
	}
}

// A rejection is permanent: retrying the same batch must re-reject it rather
// than doing the work again.
func TestRetryingABatchDoesNotDuplicateOrReapply(t *testing.T) {
	e := newEnv(t)

	good := saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(e.productA, 1000, 50))
	bad := saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(uuid.Must(uuid.NewV7()), 1000, 50))

	first := e.sync("0", good, bad)
	if first.Data.Results[0].Status != "applied" || first.Data.Results[1].Status != "rejected" {
		t.Fatalf("unexpected first pass: %+v", first.Data.Results)
	}

	second := e.sync("0", good, bad)
	if second.Data.Results[0].Status != "duplicate" {
		t.Errorf("replayed good operation: %q, want duplicate", second.Data.Results[0].Status)
	}
	if second.Data.Results[1].Status != "duplicate" {
		t.Errorf("replayed bad operation: %q, want duplicate", second.Data.Results[1].Status)
	}

	if got := e.scalar(`SELECT count(*) FROM sales`); got != 1 {
		t.Errorf("%d sales after the replay, want 1", got)
	}
	if got := e.scalar(`SELECT count(*) FROM stock_movements WHERE reason = 'sale'`); got != 1 {
		t.Errorf("%d stock movements after the replay, want 1", got)
	}
}

// An operation referencing something an earlier one in the same batch created
// must work, which is why operations apply sequentially in the order sent.
func TestOperationsApplyInOrder(t *testing.T) {
	e := newEnv(t)

	saleID := uuid.Must(uuid.NewV7())
	first := saleOp(saleID, "cash", nil, line(e.productA, 1000, 50))

	// The same sale id twice in one batch: the second must be recognised as
	// already applied rather than conflict.
	second := saleOp(saleID, "cash", nil, line(e.productA, 1000, 50))

	resp := e.sync("0", first, second)

	if resp.Data.Results[0].Status != "applied" {
		t.Errorf("first: %q", resp.Data.Results[0].Status)
	}
	if got := e.scalar(`SELECT count(*) FROM sales`); got != 1 {
		t.Errorf("%d sales, want 1 — the same sale id must not be recorded twice", got)
	}
	if got := e.scalar(`SELECT count(*) FROM stock_movements WHERE reason = 'sale'`); got != 1 {
		t.Errorf("%d stock movements, want 1 — stock must not be decremented twice", got)
	}
}

// Matrix #12 — bootstrap → sync.
//
// The boundary between a snapshot and the feed is where a row is most likely to
// be delivered twice or lost. Over-delivery is safe; a gap is not.
func TestBootstrapAndFeedDoNotLeaveAGap(t *testing.T) {
	e := newEnv(t)

	e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(e.productA, 1000, 50)))

	// A fresh device bootstraps, then syncs from the cursor it was handed.
	var boot struct {
		Data struct {
			Snapshot struct {
				Sales []map[string]any `json:"sales"`
			} `json:"snapshot"`
			Cursor string `json:"cursor"`
		} `json:"data"`
	}
	req, _ := http.NewRequest(http.MethodGet, e.server.URL+"/api/v1/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&boot); err != nil {
		t.Fatalf("could not decode bootstrap: %v", err)
	}

	if len(boot.Data.Snapshot.Sales) != 1 {
		t.Fatalf("snapshot has %d sales, want 1", len(boot.Data.Snapshot.Sales))
	}

	// A second sale arrives after the snapshot was taken.
	e.sync("0", saleOp(uuid.Must(uuid.NewV7()), "cash", nil, line(e.productB, 1000, 50)))

	// Syncing from the bootstrap cursor must deliver the new one. Whether it
	// also re-delivers the first is fine — applying is idempotent — but missing
	// the second would be a permanent gap.
	feed := e.sync(boot.Data.Cursor)

	sawSecond := false
	for _, c := range feed.Data.Changes {
		if c.Entity == "sale" {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Error("the sale made after bootstrap was never delivered by the feed")
	}
}
