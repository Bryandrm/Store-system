package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bryandrm/store-system/internal/testdb"
)

// appendOnlyTables must never accept UPDATE or DELETE from the application role.
//
// The column is only there to build a syntactically valid UPDATE: using a
// column that does not exist would make Postgres fail on the name BEFORE
// reaching the permission check, and the test would pass for the wrong reason.
var appendOnlyTables = []struct {
	table  string
	column string
}{
	{"sales", "id"},
	{"sale_lines", "id"},
	{"sale_voids", "sale_id"},
	{"stock_movements", "id"},
	{"customer_ledger", "id"},
	{"payments", "id"},
	{"product_prices", "id"},
	{"restocks", "id"},
	{"restock_lines", "id"},
	{"change_log", "seq"},
	{"change_log_floor", "singleton"},
	{"sync_operations", "op_id"},
}

// TestAppendOnlyEnforcedByPostgres is the test that holds up the system's
// central invariant.
//
// The whole design rests on nothing ever being modified or deleted. If that
// depended on the Go code behaving, it would be a convention, and conventions
// break within three features. Here we verify the database flatly REFUSES.
//
// If this test fails it is not a permissions detail: it means a stray UPDATE
// can silently corrupt the books.
func TestAppendOnlyEnforcedByPostgres(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	for _, c := range appendOnlyTables {
		t.Run(c.table, func(t *testing.T) {
			_, err := tdb.App.Exec(ctx,
				"UPDATE "+c.table+" SET "+c.column+" = "+c.column)
			requirePermissionDenied(t, err, "UPDATE on "+c.table)

			_, err = tdb.App.Exec(ctx, "DELETE FROM "+c.table)
			requirePermissionDenied(t, err, "DELETE on "+c.table)
		})
	}
}

// TestCatalogAcceptsUpdate checks the other side of the line: cosmetic metadata
// IS updated, last-write-wins.
//
// Without this test, "lock everything down" would look correct and would break
// renaming a product.
func TestCatalogAcceptsUpdate(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	for _, table := range []string{"products", "customers", "users", "sessions"} {
		if _, err := tdb.App.Exec(ctx, "UPDATE "+table+" SET created_at = created_at"); err != nil {
			if strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s should accept UPDATE (LWW metadata) but was denied", table)
			}
		}
	}
}

// TestAppCanInsertAndRead makes sure the lockdown did not overshoot: the
// application still has to be able to do its job.
func TestAppCanInsertAndRead(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	var one int
	if err := tdb.App.QueryRow(ctx, "SELECT 1 FROM sales LIMIT 1").Scan(&one); err != nil {
		if strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the application cannot read sales: %v", err)
		}
	}

	// change_log.seq is BIGSERIAL: without USAGE on the sequence every INSERT
	// fails. It is an easy permission to forget and it breaks the whole system.
	_, err := tdb.App.Exec(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ('probe', gen_random_uuid(), 'insert', '{}'::jsonb)`)
	if err != nil {
		t.Fatalf("the application cannot write to change_log: %v", err)
	}
}

// TestMigrationHistoryProtected: the application must not be able to forge the
// migration history.
func TestMigrationHistoryProtected(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	var privileges int
	err := tdb.Admin.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.role_table_grants
		 WHERE grantee = 'store_app' AND table_name = 'goose_db_version'`).Scan(&privileges)
	if err != nil {
		t.Fatalf("could not read privileges: %v", err)
	}
	if privileges != 0 {
		t.Errorf("store_app holds %d privileges on goose_db_version; it should hold none", privileges)
	}
}

// TestDerivedViewsRespond checks that stock and balances are derived and that
// the application can query them.
func TestDerivedViewsRespond(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()

	for _, view := range []string{"stock_levels", "customer_balances", "current_prices"} {
		var n int
		if err := tdb.App.QueryRow(ctx, "SELECT count(*) FROM "+view).Scan(&n); err != nil {
			t.Errorf("could not query view %s: %v", view, err)
		}
	}
}

func requirePermissionDenied(t *testing.T, err error, operation string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s was ALLOWED; append-only enforcement is not active", operation)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("%s failed for another reason (the test may be wrong): %v", operation, err)
	}
}
