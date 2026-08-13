# Architecture decision records

Each record states the decision, the reason, and — where it matters most — the
alternatives that were rejected and why. The rejected options are the valuable
part: without them a future reader re-proposes them, and the argument gets had
twice.

| # | Decision | Status |
|---|---|---|
| [000](#adr-000) | Append-only ledgers; balances are derived | accepted |
| [001](#adr-001) | The sync cursor rides on `xid8`, not `BIGSERIAL` | accepted |
| [002](#adr-002) | Opaque bearer tokens, not JWT access+refresh | accepted |
| [003](#adr-003) | Every operational mutation goes through `/sync` | accepted |
| [004](#adr-004) | Price is a ledger; cosmetic metadata is last-write-wins | accepted |
| [005](#adr-005) | Hand-written SQL over an ORM | accepted |
| [006](#adr-006) | Money in cents, quantities in thousandths, shared fixture | accepted |
| [007](#adr-007) | Hand-written service worker, `injectManifest` mode | accepted |
| [008](#adr-008) | Bearer tokens in JS, no cookies | accepted |
| [009](#adr-009) | Append-only enforced by `REVOKE`, not by convention | accepted |
| [010](#adr-010) | Nightly invariant job instead of triggers | accepted |
| [011](#adr-011) | Four test layers as a merge gate | accepted |
| [012](#adr-012) | Single store, multiple users | accepted |

---

## ADR-000 — Append-only ledgers; balances are derived {#adr-000}

**Decision.** Stock, customer balances and current prices are never stored. They
are `SUM()` over append-only tables. Corrections are compensating inserts.

**Why.** An offline device can then only ever `INSERT`, which makes sync
conflicts impossible by construction rather than something to detect and
resolve. It is the decision the entire system rests on.

**Accepted trade-off.** Two devices can sell the last bag while offline. Derived
stock goes to `-1`, the UI flags it, and the fix is a physical count that
appends an `adjustment`. Distributed locking to prevent this would be absurd for
a two-person stand, and it would not work offline anyway.

**Consequence to watch.** Every future feature will be tempted to add an
`UPDATE`. Refusing is not pedantry: one mutable column reintroduces the whole
class of problem.

---

## ADR-001 — The sync cursor rides on `xid8` {#adr-001}

**Decision.** The feed cursor is a transaction-id watermark from
`pg_snapshot_xmin(pg_current_snapshot())`. `change_log.seq` survives only as a
deterministic tiebreak within one transaction.

**Why.** Postgres sequences are not transactional, so a `BIGSERIAL` cursor drops
rows permanently:

```
tx A: nextval -> 100, still open
tx B: nextval -> 101, COMMITS
client: reads, sees 101, stores cursor=101
tx A: COMMITS
client: asks for > 101 -> row 100 is never delivered. Silently. Forever.
```

At a few dozen sales a day the window is milliseconds wide and might never
appear in testing. In the field the symptom is *"Tuesday's sale isn't on the
other phone"* — effectively undiagnosable.

`TestFeedNeverLosesARowUnderConcurrentCommits` reproduces the race.
`TestNaiveSeqCursorLosesARow` demonstrates the broken strategy directly, so the
reasoning lives in the test suite and not only here.

**Rejected alternatives.**

| Approach | Why not |
|---|---|
| `pg_advisory_xact_lock` before inserting, forcing seq order to match commit order | Works, and is genuinely simpler. Rejected only because it serializes every writer. **Keep as the fallback** if `xid8` handling ever becomes painful. |
| Safety lag: `WHERE created_at < now() - interval '5s'` | Reintroduces the clock dependence the design deliberately removed, and 5s is a guess. |
| Re-read the last N seq values | Lowers the probability; establishes no correctness. |
| Logical replication | Enormous complexity; replication slots pin WAL and can fill a 30 GB disk. |

**Consequence.** The watermark is cluster-wide, so any long transaction anywhere
freezes the feed — including the nightly `pg_dump`. This makes
`idle_in_transaction_session_timeout` load-bearing, not a tuning knob. See
gotcha #2.

---

## ADR-002 — Opaque bearer tokens, not JWT access+refresh {#adr-002}

**Decision.** A 32-byte random token, stored as its SHA-256, valid 180 days with
sliding renewal and no rotation.

**Why.** The argument comes from the offline requirement, not from taste. A
refresh token assumes the client can reach the server to refresh it, which is
exactly what this app cannot promise: a 15-minute access token expires on day
two of a three-day stretch with no signal. You then have to build "keep working
with an expired token" anyway, so the expiry buys nothing while costing
rotation, reuse detection, and sensitivity to clock skew — and this app has
known-bad clocks.

JWT's real advantage is stateless verification across services. There is one
service and one database. Revocation is a plain write; a JWT would need a
denylist, which is the lookup you were avoiding.

**No rotation**, deliberately: rotation plus a long offline stretch leaves a
device holding a token already superseded on the other phone.

---

## ADR-003 — Every operational mutation goes through `/sync` {#adr-003}

**Decision.** Sales, stock adjustments and payments arrive as operations on
`POST /api/v1/sync`. Administrative actions (users, sessions) use plain REST and
require connectivity.

**Why.** Operational data has to be recordable offline; administrative data does
not — creating a user offline is meaningless, since the username must be
globally unique and the password needs hashing.

**Consequence.** `sync/apply.go` holds the only `switch` over operation type in
the codebase, and each case delegates to the domain function that already
exists. Business logic has one implementation and two entry points, never a
"sync" version and an "online" version that drift.

---

## ADR-004 — Price is a ledger; cosmetic metadata is last-write-wins {#adr-004}

**Decision.** `product_prices` is append-only with `effective_from`; the current
price is a view. Product names, sort order and customer contact details are
last-write-wins.

**Why.** A mutable `price_cents` would be the **only** update-conflict surface
in the system, reintroducing precisely what ADR-000 exists to eliminate. With a
history, two devices changing a price offline just land two rows and the newest
`effective_from` wins, deterministically, with no resolution logic. Free audit
trail as a bonus.

**The line, stated once.** Anything touching money or stock is append-only.
Anything cosmetic is LWW. This sentence is what stops the design eroding.

---

## ADR-005 — Hand-written SQL over an ORM {#adr-005}

**Decision.** `pgx` with hand-written SQL. No ORM.

**Why.** Continues what already works in *brutalist player* (sqlx). The schema
is small and the queries are the interesting part; an ORM would hide exactly
what deserves attention — the ledger sums and the feed's window function.

**Note.** `sqlc` is listed in the plan and not yet adopted. When it is: it
validates at *generate* time, not compile time, so `sqlc diff` must run in CI or
stale generated code ships silently. It also cannot do dynamic SQL.

---

## ADR-006 — Money in cents, quantities in thousandths, shared fixture {#adr-006}

**Decision.** Money is `int64` cents; quantities are `int64` thousandths
(`qty_milli`). Half-up rounding. `testdata/money_cases.json` is read by **both**
test suites.

**Why cents.** Floating point money is a bug waiting for a customer to notice.

**Why thousandths, when everything sells by the unit.** The day "half a pound"
appears, an integer `qty` would force a migration on clients that hold offline
data — devices you do not control. It costs nothing now.

**Why a shared fixture.** It is the only thing preventing the client from
quoting the buyer one total while the server stores another. Verified by
sabotage: changing one expected value must fail both suites.

**Why greedy, not proportional, payment allocation.** A proportional split
always leaves a stray cent, and wherever it goes is arbitrary and unexplainable
to a customer. Greedy explains itself in one sentence: the oldest debt is
cancelled first.

---

## ADR-007 — Hand-written service worker, `injectManifest` mode {#adr-007}

**Decision.** `vite-plugin-pwa` supplies only the precache list; the service
worker is written by hand.

**Why.** All data lives in IndexedDB, not in the SW cache, so none of Workbox's
runtime caching strategies apply. The only thing worth caching is the app shell.

**Two rules the hand-written version exists to enforce.** `/api/` is never
intercepted — a stale cached response would show sales that no longer exist —
and a new version never activates on its own, because swapping the app mid-sale
clears the screen while a customer waits for change.

---

## ADR-008 — Bearer tokens in JS, no cookies {#adr-008}

**Decision.** The token is held in IndexedDB and sent as `Authorization:
Bearer`. No cookies anywhere; `credentials: 'omit'`.

**Why.** The PWA is on Cloudflare Pages and the API on `api.<domain>` — cross-site
by design. `SameSite=None` third-party cookies are being phased out, and a
cookie would bring a CSRF surface. Bearer-in-JS has none.

**Accepted trade-off.** XSS could read the token. The mitigation is that there
is no third-party script on the page at all: the artifact CSP-equivalent posture
is enforced by having zero external dependencies at runtime.

---

## ADR-009 — Append-only enforced by `REVOKE`, not by convention {#adr-009}

**Decision.** `store_app` holds `SELECT, INSERT` and is explicitly denied
`UPDATE, DELETE` on every transactional table. Migrations run as a separate role.

**Why.** ADR-000 is the system's central invariant. If it depended on the Go
code behaving, it would be a convention, and conventions break within three
features. Here the database refuses.

**How it earns its keep.** It already caught something: the blanket
`GRANT ON ALL TABLES` swept up goose's migration table, letting the application
forge migration history. Found while verifying the lockdown by hand; see gotcha #3.

**Consequence.** Any migration adding a transactional table must issue its own
`REVOKE`. `ALTER DEFAULT PRIVILEGES` only covers the base grant.

---

## ADR-010 — Nightly invariant job instead of triggers {#adr-010}

**Decision.** Cross-table invariants — lines summing to the sale total, a credit
sale having exactly one ledger entry — are checked by `cmd/verify`, run in CI,
nightly before the backup, and against every restored backup. Not by triggers.

**Why.** A trigger on every `INSERT` costs latency on a 0.25 vCPU box and makes
the write path harder to reason about. Because nothing is ever overwritten,
drift cannot accumulate silently: a daily check bounds the damage to 24 hours
and the ledger allows reconstructing what happened.

**What is checked at write time anyway.** Sum-of-lines equals total, and a sale
has at least one line — both already computed in the transaction, so they cost
nothing.

---

## ADR-011 — Four test layers as a merge gate {#adr-011}

**Decision.** No feature is done without: unit tests, integration tests against
neighbouring features, a Playwright flow, and a failure-path case.

**Why.** This system handles other people's money and physical stock. An
arithmetic or convergence bug does not render badly, it miscollects a debt.

**Coverage threshold only where it means something**: 85% on the domain
packages, none globally. A global number is met by writing handler tests that
assert nothing, which measures obedience rather than correctness.

**Honest limitation.** Playwright supports service workers in Chromium only, so
WebKit behaviour — including iOS storage eviction — stays a manual checklist
item. The owner's device is an iPhone, so this gap is real and named rather than
papered over.

---

## ADR-012 — Single store, multiple users {#adr-012}

**Decision.** One store. Several users with `owner` / `staff` roles. No
`store_id` column, no tenancy.

**Why.** This was evaluated explicitly, because it is the one remaining decision
with asymmetric cost: adding `store_id` later means migrating data that lives on
devices you do not control. It was rejected because multi-tenancy drags in
public registration, invitations, password recovery and the responsibility of
holding other people's business data — none of which is wanted.

**If it ever changes.** The cost is real but bounded: the append-only design
means every device can simply re-bootstrap, which is exactly what makes a
migration of this shape survivable.

**Authorization is one column and one middleware.** No policy layer, no
permission tables, for a store run by two people.
