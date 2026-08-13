# Testing

> **No feature is done without its four layers: unit, integration against
> neighbouring features, a Playwright flow, and a failure-path case.**

A gate, not an aspiration. The reason is concrete: this system handles other
people's money and physical stock. An arithmetic or convergence bug does not
render badly, it miscollects a debt.

## Definition of done, per feature

- [ ] **Unit** — arithmetic and state machines, in isolation
- [ ] **Integration** — against every feature it touches, on a real Postgres
- [ ] **End to end** — at least one Playwright spec of the flow as the user sees it
- [ ] **Failure path** — not only the happy one: offline, expired token, replayed operation
- [ ] **Changelog** — a commit scoped to the module

## Layer 1 — Unit

**Go, against a real, throwaway Postgres.** `internal/testdb` creates
`store_test_template` once, then gives each test its own database via
`CREATE DATABASE … TEMPLATE` in about 30 ms.

Two deliberate choices:

**No mocks.** A mocked database cannot reproduce transaction semantics, and the
most valuable tests here are exactly the ones that depend on them.

**Tests connect as `store_app`**, the same role as production, never as a
superuser. Running as superuser would hide a missing `GRANT` until deploy, and
an improper `UPDATE` would never be caught because a superuser is allowed to do
it.

**Not used: wrapping each test in a transaction and rolling back.** It would be
faster and would make the convergence tests impossible, since they need real,
separate, concurrently committing transactions.

**TypeScript, with a real in-memory IndexedDB** (`fake-indexeddb`). Same
reasoning.

## Layer 2 — The shared fixture

`testdata/money_cases.json` is read by **both** suites — `internal/money` and
`web/src/domain/money.test.ts`. It is the only thing preventing the client from
quoting the buyer one total while the server stores another.

**Verified by sabotage, not by inspection.** Changing one expected value must
fail *both* suites; restoring it must return both to green. If only one fails,
the fixture is a copy and the guarantee is imaginary.

```
GO:          RoundHalfUp(1500, 1000) = 2, want 99   FAIL
TYPESCRIPT:  expected 2 to be 99                    FAIL
```

Re-run that check whenever the fixture moves.

## Layer 3 — Integration, feature against feature

The layer that catches the most real bugs, because one operation touches four
modules at once: a credit sale writes `sales`, `sale_lines`, `stock_movements`
and `customer_ledger` in the same transaction.

**Always enter through `POST /api/v1/sync`**, exactly like the client. A test
that calls `sales.Create()` directly is not testing what runs in production —
the point of this layer is the wiring, and the wiring lives in `apply.go`.

| # | From → To | What it verifies |
|---|---|---|
| 1 | sale → inventory | Selling 3 units drops derived stock by exactly 3 |
| 2 | credit sale → debt | A negative ledger entry for the exact amount |
| 3 | mixed sale → debt | The entry is the *difference*, not the total |
| 4 | payment → debt | Cancels oldest first; overpayment becomes credit |
| 5 | credit balance → sale | A customer with credit takes product with no cash moving |
| 6 | void → inventory + debt | Both restored exactly; voiding twice compensates once |
| 7 | restock → inventory → margin | Cost feeds the margin calculation |
| 8 | adjustment → inventory | A physical count fixes negative stock in one entry |
| 9 | price → sale | Historical sales unchanged; new ones use the new price |
| 10 | profiles → everything | Attribution per user; `staff` gets 403 on owner routes |
| 11 | sync → everything | A 5-operation batch where #3 is invalid: the rest still apply |
| 12 | bootstrap → sync | No duplicates and no gaps at the cursor boundary |

**Two that are worth the whole layer:**

- **`convergence_test.go`** — two devices sell the same product offline, then
  sync interleaved. Both must end with identical stock and balances, matching
  the server. It is the system's central claim.
- **`feed_race_test.go`** — the `BIGSERIAL` race with real held transactions.

## Layer 4 — Playwright

`serviceWorkers: 'allow'` and a Pixel 7 profile, because the app is used on a
phone.

| Spec | What it proves |
|---|---|
| `login` | Session survives a reload; logout clears the token |
| `venta` | Tap products, running total, charge, appears in the day view |
| **`venta-offline`** | **The central one.** Offline → 3 sales → kill the tab → reopen → still there, marked pending → reconnect → synced, and present in the database |
| `recarga-carrito` | A cart survives `page.reload()` |
| `dos-dispositivos` | Two browser contexts, two IndexedDBs: both converge to the same stock |
| `sesion-vencida` | Revoke the token server-side: selling keeps working, queued sales upload after re-login without duplicating |
| `actualizacion` | A new version shows the banner and does **not** reload on its own |

Seed the database per spec via `psql`, never through the UI: seeding through the
UI means one login bug breaks twenty specs.

## What Playwright cannot cover

**Playwright supports service workers in Chromium only.** WebKit is out of
reach, and the owner's phone is an iPhone. These stay manual, on a real device:

- [ ] Install from Share → Add to Home Screen, and confirm it opens without Safari chrome
- [ ] Airplane mode, 3 sales, **kill the app completely**, reopen — sales still there
- [ ] Reconnect and confirm they reach the server
- [ ] **Leave the app unopened for 8 days**, then check the data survived
      (iOS evicts storage for sites unused for about a week)
- [ ] Confirm the install hint appears in Safari and disappears once installed
- [ ] Cart survives a reload mid-sale
- [ ] Two devices, one iPhone and one Android, converge on the same stock

The eviction check is the one that matters most and the only one that takes a
week of calendar time. Everything already synced lives on the server, so the
real exposure is narrow: only unsent operations would be lost.

## Not tested, as a decision

React component rendering and Tailwind classes. Visible behaviour is covered by
Playwright, which is where it matters; testing components in isolation as well
would duplicate effort in the lowest-value layer.

## CI gates

```bash
go vet ./... && go test ./... -race     # layers 1 and 3
go run ./cmd/verify                     # cross-table invariants
tsc --noEmit && vitest run              # TypeScript
playwright test                         # layer 4
```

**Coverage threshold only where it means something**: 85% on
`internal/{sales,inventory,credit,sync}` and `web/src/{domain,db,sync}`. **No
global threshold** — a global number is met by writing handler tests that assert
nothing, which measures obedience rather than correctness.

## Running locally

```bash
docker compose -f compose.dev.yml up -d
go test ./... -race
cd web && pnpm test
```

**Note:** the Go harness alters `store_app`'s password, and Postgres roles are
cluster-wide. It uses the same password as the dev server for exactly that
reason — see gotcha #6.
