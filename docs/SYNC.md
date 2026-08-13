# The synchronization protocol

The correctness argument for the part of the system that is easiest to get
subtly, silently wrong.

## What makes this tractable

Offline synchronization is normally hard because two devices edit the same thing
and someone has to decide who wins. Here that situation cannot arise:

> **Every fact that touches money or stock is an immutable row. Offline devices
> only ever `INSERT`.**

There is nothing to merge. "Syncing" degrades into *ship rows both ways and
dedupe by id*. Every hard-looking property below follows from that one choice.

## The two directions

One `POST /api/v1/sync` carries both. A phone on mobile data pays for each round
trip, and there is no reason to spend two.

```
client → server   operations the device recorded while it was on its own
server → client   everything that changed since the device's cursor
```

## Push: why replay is harmless

The client generates the operation id (`op_id`, a UUIDv7) **and** the sale id.
The server claims the `op_id` first:

```sql
INSERT INTO sync_operations (op_id, …, status)
VALUES ($1, …, 'applied')
ON CONFLICT (op_id) DO NOTHING
RETURNING op_id
```

If nothing comes back, this operation already ran and every dependent write is
skipped. The domain rows, the `change_log` rows and this idempotency row all
commit **in one transaction**, which is what makes a lost response harmless: the
client retries, gets `duplicate`, and nothing is written twice.

`sales` additionally uses `ON CONFLICT (id) DO NOTHING`, so replay is safe at the
row level too, independently of the operation ledger.

**First write wins, without exception.** A replay carrying a different payload
for the same `op_id` is a client bug: it is logged and ignored, never applied.

### Per-operation results

| Status | Meaning | Client reaction |
|---|---|---|
| `applied` | recorded now | acknowledge |
| `duplicate` | already recorded | acknowledge — identical outcome |
| `rejected` | will never succeed | error tray |
| `retry` | infrastructure hiccup | back to the queue |

Operations apply **sequentially, in the order sent**, each in its own
transaction — so an operation can reference something an earlier one in the same
batch created.

**Batch atomicity is deliberately not offered.** One bad operation must never
block the other ninety-nine. A transient failure does stop the batch, because
continuing would just pile up more failures.

## Pull: the cursor, and the bug it exists to avoid

The obvious cursor is `change_log.seq`, a `BIGSERIAL`. It is wrong, and wrong in
the worst way — silently.

Postgres sequences are **not transactional**:

```
tx A: nextval → 100.  Still open.
tx B: nextval → 101.  COMMITS.
client: reads, sees row 101, stores cursor = 101
tx A: COMMITS.
client: asks for everything after 101.
        Row 100 is never delivered. Ever.
```

Aborted transactions also burn sequence values, so `seq` has permanent holes and
any "expect contiguity" heuristic stalls forever instead of recovering.

At a few dozen sales a day this window is milliseconds wide. It may never appear
in testing. In the field it presents as *"Tuesday's sale isn't on the other
phone"*, with nothing in any log to explain it.

### The fix: a transaction-id watermark

`pg_snapshot_xmin(pg_current_snapshot())` returns the lowest transaction id
still running. Every transaction below it has finished, and if it committed it
is permanently visible to every future snapshot.

```sql
SELECT entity, entity_id, op, payload, xact_id::text, seq
FROM change_log
WHERE xact_id >= $cursor::xid8
  AND xact_id <  $watermark::xid8
ORDER BY xact_id, seq
LIMIT $n
```

The cursor stays a single monotonic integer — immune to clock skew — and is now
also immune to the race.

### Why this is correct

1. The watermark is computed in statement one and filtered on in statement two.
   Between them it can only move forward, and moving forward never hides a row
   that already committed. **No explicit transaction or isolation level is
   needed for the feed.**
2. All rows of one transaction share an `xact_id`, so the half-open interval
   `[cursor, watermark)` can never split a transaction down the middle.
3. Delivery is at-least-once and applying is an idempotent upsert by entity id.
   Over-delivery is free; under-delivery is the only real failure.

### Paging

`truncateAtTxBoundary` drops the trailing rows sharing the last `xact_id`, so a
page never ends mid-transaction.

**Guard:** if a single transaction is larger than the page limit, it is
delivered whole and the limit is exceeded. Otherwise the client would loop
forever asking for a page it can never complete.

### Retention

`change_log` is pruned beyond 90 days, and `change_log_floor` records the oldest
retained transaction id. A cursor below the floor gets `CURSOR_TOO_OLD` and the
client re-bootstraps.

**The check runs before the "nothing new" shortcut.** Getting that order wrong
told a stale client it was up to date while it silently missed everything pruned
out from under it. See gotcha #4.

## Bootstrap

`GET /api/v1/bootstrap` returns a complete replica plus a starting cursor, in a
`REPEATABLE READ READ ONLY` transaction.

Bootstrap **does** need that isolation level, unlike the feed, because the
watermark and the data must come from one snapshot. Rows written by transactions
at or above the watermark that happen to be visible get re-delivered by the
first feed call.

**Bootstrap may over-deliver, never under-deliver.** Safe for the same reason as
everything else: applying is idempotent.

## What the client does

### The outbox

Every mutation is recorded in IndexedDB before it is sent anywhere. That is why
a sale rung up with no signal is not lost: it is durable data, not a pending
HTTP request.

Ordering comes from `op_id` being a UUIDv7, which sorts by creation time — the
causal order on that device, which is the only causality that exists. No
ordering is implied between two phones.

### Failure classification

The most behaviour-defining code on the client, and its mistakes are asymmetric:
calling something permanent when it is transient **throws away a real sale**,
while the reverse only wastes battery. So anything unrecognized is transient.

| Kind | Trigger | Reaction |
|---|---|---|
| `transient` | network error, 5xx, 429 | retry forever, **no attempt cap** |
| `auth` | 401, `TOKEN_EXPIRED`, `TOKEN_REVOKED` | pause the loop, **keep the outbox**, keep selling |
| `stale_cursor` | `CURSOR_TOO_OLD` | rebuild the read model, **keep the outbox** |
| `permanent` | other 4xx | error tray |

Two cases share HTTP 409 and demand opposite reactions — `CURSOR_TOO_OLD` means
rebuild everything, `SALE_ALREADY_VOIDED` means discard one operation — so the
error code wins over the status.

### Single-flight

A cycle runs inside `navigator.locks`. Two open tabs would otherwise claim the
same outbox entries and send them twice. The server dedupes, but it doubles
traffic and produces confusing local results.

### Retention as disaster recovery

Acknowledged operations are kept 30 days rather than deleted. A few hundred
kilobytes buys the cheapest recovery in the system: if the server is restored
from last night's dump, every device can replay everything since, and
idempotency makes that duplicate-free.

It turns "lose a day" into "lose nothing", and it falls out of the append-only
design for free.

## Overselling

Two devices sell the last bag while offline. Derived stock becomes `-1` and the
UI shows it in red.

Resolution is a **physical count**: the owner enters the real quantity and the
app appends an `adjustment` movement for the difference. Just another insert.

No locks, no reservations, and **no conflict-resolution UI** — there is nothing
to resolve.

## Clock skew

Sync is clock-immune, because the cursor rides on transaction ids. `occurred_at`
is not, and every report groups by it.

- Every `/sync` response carries `server_time`; the client stores the offset and
  applies it when stamping new sales.
- Beyond five minutes of drift, a persistent banner appears.
- Server-side, an `occurred_at` in the future is clamped and flagged — **never
  rejected**. Losing a real sale because a phone's clock is wrong would be far
  worse than a misfiled report line.
- UUIDv7 timestamps are for ordering only, never business logic.
