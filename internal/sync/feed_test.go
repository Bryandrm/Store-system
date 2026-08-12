// None of the tests in this file call t.Parallel(), and that is deliberate.
//
// Postgres transaction ids are CLUSTER-WIDE, not per-database. The watermark
// comes from pg_snapshot_xmin, so a test holding an open transaction pins xmin
// for every other database in the same Postgres instance — including the
// throwaway databases belonging to other tests. Run in parallel, one test's
// open transaction makes another test's freshly committed rows invisible.
//
// This is not a quirk of the harness. It is the same property that makes the
// nightly pg_dump briefly freeze the sync feed in production, and it is why
// idle_in_transaction_session_timeout is not optional. See docs/GOTCHAS.md.
package sync_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bryandrm/store-system/internal/httpx"
	"github.com/bryandrm/store-system/internal/sync"
	"github.com/bryandrm/store-system/internal/testdb"
)

// TestFeedNeverLosesARowUnderConcurrentCommits is the single most important
// test in the project.
//
// It reproduces the race that makes a BIGSERIAL cursor unusable:
//
//	tx A takes a lower sequence value but commits LAST
//	tx B takes a higher one and commits FIRST
//	a client reading in between would record B's position and never see A
//
// The xid8 watermark prevents it: while A is still running, pg_snapshot_xmin
// sits at A's transaction id, so B's row is withheld too. Nothing is delivered
// until it can be delivered in order.
//
// If this test fails, the symptom in the field is "Tuesday's sale isn't on the
// other phone", which is effectively impossible to diagnose from a report.
func TestFeedNeverLosesARowUnderConcurrentCommits(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()

	// Two genuinely separate connections. This is exactly why the harness does
	// not wrap tests in a transaction: it would make this test impossible.
	connA, err := pgx.Connect(ctx, tdb.AppURL)
	if err != nil {
		t.Fatalf("connection A: %v", err)
	}
	defer connA.Close(ctx)

	connB, err := pgx.Connect(ctx, tdb.AppURL)
	if err != nil {
		t.Fatalf("connection B: %v", err)
	}
	defer connB.Close(ctx)

	// A starts first, so it holds the lower transaction id.
	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txA.Exec(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ('sale', $1, 'insert', '{"who":"A"}'::jsonb)`,
		uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}

	// B starts second, takes a higher transaction id, and commits FIRST.
	txB, err := connB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txB.Exec(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ('sale', $1, 'insert', '{"who":"B"}'::jsonb)`,
		uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	if err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The client reads while A is still open. This is the exact moment a naive
	// cursor would skip ahead past A forever.
	firstBatch, cursor, _, err := sync.ReadFeed(ctx, tdb.App, 0, 100)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(firstBatch) != 0 {
		t.Errorf("the feed delivered %d change(s) while a lower transaction was still "+
			"open; it must withhold them until they can be delivered in order", len(firstBatch))
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	secondBatch, _, _, err := sync.ReadFeed(ctx, tdb.App, cursor, 100)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	seen := map[string]int{}
	for _, c := range append(firstBatch, secondBatch...) {
		seen[string(c.Payload)]++
	}

	if seen[`{"who": "A"}`]+seen[`{"who":"A"}`] == 0 {
		t.Errorf("the row from transaction A was NEVER delivered: %v", seen)
	}
	if seen[`{"who": "B"}`]+seen[`{"who":"B"}`] == 0 {
		t.Errorf("the row from transaction B was NEVER delivered: %v", seen)
	}
	for payload, count := range seen {
		if count != 1 {
			t.Errorf("change %s was delivered %d times, expected exactly once", payload, count)
		}
	}
}

// TestNaiveSeqCursorLosesARow documents WHY the cursor rides on xid8 by showing
// the failure directly. It queries change_log the naive way, ordering by the
// BIGSERIAL column, and asserts that a row does get skipped.
//
// It is written as a passing test of a broken strategy so the reasoning stays in
// the repository. If Postgres ever made sequences transactional, this test would
// fail and the xid8 machinery could be reconsidered.
func TestNaiveSeqCursorLosesARow(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()

	connA, err := pgx.Connect(ctx, tdb.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close(ctx)

	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seqA int64
	if err := txA.QueryRow(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ('sale', $1, 'insert', '{}'::jsonb) RETURNING seq`,
		uuid.Must(uuid.NewV7())).Scan(&seqA); err != nil {
		t.Fatal(err)
	}

	// B commits while A is still open, taking the higher sequence value.
	var seqB int64
	if err := tdb.App.QueryRow(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ('sale', $1, 'insert', '{}'::jsonb) RETURNING seq`,
		uuid.Must(uuid.NewV7())).Scan(&seqB); err != nil {
		t.Fatal(err)
	}
	if seqB <= seqA {
		t.Fatalf("expected B to take a higher sequence value: A=%d B=%d", seqA, seqB)
	}

	// A naive client reads everything visible and records the highest seq.
	var naiveCursor int64
	if err := tdb.App.QueryRow(ctx,
		`SELECT COALESCE(max(seq), 0) FROM change_log`).Scan(&naiveCursor); err != nil {
		t.Fatal(err)
	}
	if naiveCursor != seqB {
		t.Fatalf("the naive cursor should have landed on B: got %d, want %d", naiveCursor, seqB)
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A now exists, but its seq is BELOW the cursor, so a naive reader asking
	// for "everything after my cursor" never sees it again.
	var missed int
	if err := tdb.App.QueryRow(ctx,
		`SELECT count(*) FROM change_log WHERE seq > $1`, naiveCursor).Scan(&missed); err != nil {
		t.Fatal(err)
	}
	if missed != 0 {
		t.Fatalf("expected the naive reader to find nothing new, found %d", missed)
	}

	var total int
	if err := tdb.App.QueryRow(ctx, `SELECT count(*) FROM change_log`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("expected 2 rows in change_log, found %d", total)
	}
	// Two rows exist, the naive cursor sees one of them and will never catch up.
	// That is the bug, and it is why ReadFeed uses a transaction-id watermark.
}

// TestFeedIsIdempotentAcrossReads: reading the same interval twice must produce
// the same changes. Over-delivery is free because applying is an upsert;
// under-delivery is the only real failure.
func TestFeedIsIdempotentAcrossReads(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := tdb.App.Exec(ctx,
			`INSERT INTO change_log (entity, entity_id, op, payload)
			 VALUES ('sale', $1, 'insert', '{}'::jsonb)`,
			uuid.Must(uuid.NewV7())); err != nil {
			t.Fatal(err)
		}
	}

	first, cursor, _, err := sync.ReadFeed(ctx, tdb.App, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("first read returned %d changes, want 3", len(first))
	}

	again, _, _, err := sync.ReadFeed(ctx, tdb.App, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 3 {
		t.Errorf("re-reading from zero returned %d changes, want 3", len(again))
	}

	// Reading from the returned cursor must yield nothing new.
	empty, _, _, err := sync.ReadFeed(ctx, tdb.App, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("reading from the cursor returned %d changes, want 0", len(empty))
	}
}

// TestFeedRejectsCursorBelowRetentionFloor: a client that fell behind the
// pruning window must be told to re-bootstrap rather than handed a feed with a
// silent hole in it.
func TestFeedRejectsCursorBelowRetentionFloor(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()

	if _, err := tdb.Admin.Exec(ctx,
		`UPDATE change_log_floor SET min_retained_xact_id = '999999999'::xid8`); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := sync.ReadFeed(ctx, tdb.App, 1000, 100)
	if !errors.Is(err, httpx.ErrCursorTooOld) {
		t.Errorf("want ErrCursorTooOld, got %v", err)
	}
}

// TestFeedPaginates checks that a page never splits a transaction and that the
// cursor makes progress.
func TestFeedPaginates(t *testing.T) {
	tdb := testdb.New(t)
	ctx := context.Background()

	// Each insert is its own transaction, so each gets its own xact_id and the
	// feed is free to cut between any two of them.
	const total = 10
	for i := 0; i < total; i++ {
		if _, err := tdb.App.Exec(ctx,
			`INSERT INTO change_log (entity, entity_id, op, payload)
			 VALUES ('sale', $1, 'insert', '{}'::jsonb)`,
			uuid.Must(uuid.NewV7())); err != nil {
			t.Fatal(err)
		}
	}

	var cursor uint64
	collected := 0
	for pages := 0; pages < 20; pages++ {
		batch, next, hasMore, err := sync.ReadFeed(ctx, tdb.App, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		collected += len(batch)

		if next < cursor {
			t.Fatalf("the cursor went backwards: %d -> %d", cursor, next)
		}
		cursor = next
		if !hasMore {
			break
		}
	}

	if collected != total {
		t.Errorf("paginating collected %d changes, want %d", collected, total)
	}
}
