// Package sync implements the synchronization protocol: the change feed the
// clients read, and the operation applier they write through.
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/bryandrm/store-system/internal/db"
	"github.com/bryandrm/store-system/internal/httpx"
)

// Change is one row of the feed, as the client receives it.
type Change struct {
	Entity   string          `json:"entity"`
	EntityID uuid.UUID       `json:"entity_id"`
	Op       string          `json:"op"`
	Payload  json.RawMessage `json:"payload"`
}

// MaxFeedPage caps how many changes one response carries.
const MaxFeedPage = 500

// Watermark returns the lowest transaction id still running.
//
// This is the whole trick behind the cursor. Postgres sequences are NOT
// transactional, so a BIGSERIAL cursor loses rows permanently:
//
//	tx A: nextval -> 100, still open
//	tx B: nextval -> 101, COMMITS
//	client: reads, sees 101, stores cursor=101
//	tx A: COMMITS
//	client: asks for > 101 -> row 100 is never delivered. Silently. Forever.
//
// pg_snapshot_xmin gives the lowest transaction id still in progress. Every
// transaction below it has finished, and if it committed it is permanently
// visible to every future snapshot. Filtering on xact_id < watermark therefore
// never skips a row, while the cursor stays a single monotonic integer that no
// clock can distort.
//
// See docs/DECISIONS.md ADR-001.
func Watermark(ctx context.Context, q db.Querier) (uint64, error) {
	var raw string
	if err := q.QueryRow(ctx,
		`SELECT pg_snapshot_xmin(pg_current_snapshot())::text`).Scan(&raw); err != nil {
		return 0, fmt.Errorf("sync: could not read watermark: %w", err)
	}

	// Cast at the SQL boundary and carry the value as a plain integer in Go,
	// rather than fighting pgx over an xid8 codec.
	wm, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sync: unreadable watermark %q: %w", raw, err)
	}
	return wm, nil
}

// RetentionFloor returns the oldest transaction id still kept in change_log.
//
// A client whose cursor fell below this lost history and must re-bootstrap.
// Skipping this check is how pruning turns into silent gaps a year later.
func RetentionFloor(ctx context.Context, q db.Querier) (uint64, error) {
	var raw string
	if err := q.QueryRow(ctx,
		`SELECT min_retained_xact_id::text FROM change_log_floor`).Scan(&raw); err != nil {
		return 0, fmt.Errorf("sync: could not read retention floor: %w", err)
	}
	floor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sync: unreadable retention floor %q: %w", raw, err)
	}
	return floor, nil
}

// feedRow is a change plus the ordering columns, used only inside this file.
type feedRow struct {
	Change
	xactID uint64
	seq    int64
}

// ReadFeed returns the changes in the half-open interval [cursor, watermark).
//
// The interval is half-open on purpose: every row of one transaction shares an
// xact_id, so this can never split a transaction down the middle.
//
// Delivery is at-least-once. Applying is an idempotent upsert by entity id, so
// over-delivering is free and under-delivering is the only real failure.
func ReadFeed(ctx context.Context, q db.Querier, cursor uint64, limit int) (changes []Change, nextCursor uint64, hasMore bool, err error) {
	if limit <= 0 || limit > MaxFeedPage {
		limit = MaxFeedPage
	}

	// The watermark is computed BEFORE the query on purpose. Between the two
	// statements it can only move forward, and moving forward never hides a row
	// that already committed, so no explicit transaction or isolation level is
	// needed here.
	watermark, err := Watermark(ctx, q)
	if err != nil {
		return nil, 0, false, err
	}

	// The retention check comes FIRST. A cursor below the floor means the
	// client lost history and must re-bootstrap, and that is true no matter
	// where the watermark happens to sit. Checking it after the "nothing new"
	// shortcut below would let a stale client be told "you are up to date"
	// while it is in fact missing rows forever.
	floor, err := RetentionFloor(ctx, q)
	if err != nil {
		return nil, 0, false, err
	}
	if cursor > 0 && cursor < floor {
		return nil, 0, false, httpx.ErrCursorTooOld
	}

	if cursor >= watermark {
		return []Change{}, cursor, false, nil
	}

	// One extra row so we can tell "there is more" from "that was everything".
	rows, err := q.Query(ctx,
		`SELECT entity, entity_id, op, payload, xact_id::text, seq
		 FROM change_log
		 WHERE xact_id >= $1::xid8 AND xact_id < $2::xid8
		 ORDER BY xact_id, seq
		 LIMIT $3`,
		strconv.FormatUint(cursor, 10), strconv.FormatUint(watermark, 10), limit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("sync: could not read the feed: %w", err)
	}
	defer rows.Close()

	var collected []feedRow
	for rows.Next() {
		var fr feedRow
		var xactText string
		if err := rows.Scan(&fr.Entity, &fr.EntityID, &fr.Op, &fr.Payload, &xactText, &fr.seq); err != nil {
			return nil, 0, false, fmt.Errorf("sync: could not scan a change: %w", err)
		}
		if fr.xactID, err = strconv.ParseUint(xactText, 10, 64); err != nil {
			return nil, 0, false, fmt.Errorf("sync: unreadable xact_id %q: %w", xactText, err)
		}
		collected = append(collected, fr)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("sync: could not read the feed: %w", err)
	}

	return truncateAtTxBoundary(collected, watermark, limit)
}

// truncateAtTxBoundary drops the trailing rows that share the last transaction
// id, so a page never cuts a transaction in half.
//
// Guard worth stating: if a single transaction is larger than the page limit,
// it is delivered whole and the limit is exceeded. Otherwise the client would
// loop forever asking for a page it can never finish.
func truncateAtTxBoundary(rows []feedRow, watermark uint64, limit int) ([]Change, uint64, bool, error) {
	if len(rows) == 0 {
		return []Change{}, watermark, false, nil
	}

	hasMore := len(rows) > limit
	if !hasMore {
		// Everything up to the watermark fits: the cursor can jump to it.
		out := make([]Change, len(rows))
		for i, r := range rows {
			out[i] = r.Change
		}
		return out, watermark, false, nil
	}

	rows = rows[:limit]
	lastXact := rows[len(rows)-1].xactID

	cut := len(rows)
	for cut > 0 && rows[cut-1].xactID == lastXact {
		cut--
	}

	if cut == 0 {
		// One transaction alone fills the page. Deliver it whole, exceeding the
		// limit, or the client makes no progress at all.
		out := make([]Change, len(rows))
		for i, r := range rows {
			out[i] = r.Change
		}
		return out, lastXact + 1, true, nil
	}

	rows = rows[:cut]
	out := make([]Change, len(rows))
	for i, r := range rows {
		out[i] = r.Change
	}
	return out, rows[len(rows)-1].xactID + 1, true, nil
}
