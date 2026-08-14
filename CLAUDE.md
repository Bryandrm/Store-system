# CLAUDE.md

Operating manual for this repository. Read this before changing anything.

## What this is

A point-of-sale system for a small snack stand run by two people: fast sale
entry, inventory, restocking, store credit and customer debt. An installable
PWA that **works with no signal** and syncs across devices.

The stand is often out of mobile coverage, so offline is not a nicety — it is
the primary requirement. The owner's phone is an **iPhone**, the partner's is
Android.

## The one rule everything else follows

> **Every fact that touches money or stock is an immutable row. Every number the
> UI shows is a `SUM()`. Offline devices only ever `INSERT`.**

Consequence: **no sync conflict can exist by construction**. Nothing to resolve,
because nothing can collide.

This is not left to discipline. `store_app`, the role the API runs as, **has no
`UPDATE` or `DELETE` privilege** on any transactional table. A stray `UPDATE`
fails with `permission denied` on its first run instead of silently corrupting
the books. `TestAppendOnlyEnforcedByPostgres` guards it.

## Stack

| Layer | Choice |
|---|---|
| Backend | Go 1.26 · stdlib `net/http` · `pgx` · `goose` |
| Database | PostgreSQL 18, `--data-checksums`, `--locale=C` |
| Frontend | React 19 · Vite · strict TS · Tailwind v4 · Zustand · `idb` |
| Offline | IndexedDB · hand-written service worker · operation outbox |
| Infra | Cloudflare Pages (PWA) + GCP e2-micro (API) + Caddy |

No ORM, no HTTP framework, no component library.

## Principles

1. **No per-endpoint ceremony.** A handler plus a domain function. Layered
   architecture on its own does not stop a file from growing without bound: when
   adding an endpoint costs six files, everyone piles onto the one that exists.
2. **A `.go` file past 400 lines is split by use case, not by layer.**
3. **No interfaces until there are two real implementations.** A `service` is a
   struct holding a `*pgxpool.Pool`, not a "port". The project's only interface
   is `db.Querier`, which genuinely has two.
4. **Prefer the native option over the dependency.** stdlib routing over chi,
   `navigator.locks` over a mutex library, hand-written validation over
   struct-tag magic.
5. **Hand-written SQL.** No ORM.
6. **`snake_case` in Postgres.** camelCase identifiers force quoting in every
   raw query.
7. **One response contract, one pagination contract.**
8. **No mocks in tests.** Real, throwaway Postgres; real in-memory IndexedDB.
9. **Every feature ships with its four test layers.** CI gate, not aspiration.
10. **Invariants are enforced in the database**, not in the code, whenever a
    `REVOKE` or a `CHECK` can express them.

## Language

**Code, comments, documentation and commit messages are in English.**

**User-facing strings are in Spanish** — the app's interface and the `Message`
field of every API error. The people running the store read those.

## Repository layout

```
cmd/storeapi/       wiring only: config, pool, migrations, routes, shutdown
cmd/seed/           first owner + initial catalog. Idempotent.
internal/
  config/           env -> Config
  httpx/            envelope · errors · middleware · decode
  db/               pool · WithTx · migrations · change_log helper
  testdb/           throwaway database per test
  money/            arithmetic, mirrored in web/src/domain/money.ts
  auth/             argon2id · opaque tokens · rate limiting
  sales/            one sale, one transaction
  sync/             feed · apply · bootstrap · handlers
web/src/
  domain/           types · money · uuid · recordSale
  db/               schema · apply · outbox · local
  sync/             client · engine
  stores/           Zustand caches over IndexedDB
  components/       hand-rolled primitives
  routes/           Login · Sell · Today
  sw.ts             hand-written service worker
testdata/           money_cases.json — SHARED by both test suites
```

## Commands

```bash
docker compose -f compose.dev.yml up -d      # Postgres on :5433

go test ./... -race                          # backend, real database
go run ./cmd/seed -username <u> -password <p>
go run ./cmd/storeapi                        # needs DATABASE_URL, ALLOWED_ORIGINS

cd web
pnpm dev                                     # :5173, proxies /api to :8080
pnpm test                                    # vitest
pnpm typecheck
pnpm build
```

## Things not to do

- **Do not build conflict-resolution UI.** There are no conflicts. Every request
  for one is a request to break the core invariant. Ask first.
- **Do not add an `UPDATE` to a transactional table.** Corrections are
  compensating inserts. If you think you need one, you are about to break the
  design; ask first.
- **Do not cache derived values.** Stock and balances are recomputed. An
  incremental counter is the premature abstraction that produces
  double-counting bugs.
- **Do not discard outbox entries on a 401.** They are business data, not HTTP
  requests. Auth failures pause the loop and leave the queue alone.
- **Do not let the service worker intercept `/api/`.** A stale cached response
  would show sales that no longer exist.
- **Do not auto-reload on a new version.** Swapping the app mid-sale clears the
  screen while a customer waits for change.
- **Do not build a response body outside `httpx/envelope.go`.**
- **Do not use `crypto.randomUUID()` for operation ids.** It emits v4, which
  sorts randomly; the outbox needs v7's time ordering.
- **Do not add percentage discounts.** Absolute cents only. Percentages are the
  classic rounding-drift generator and there is no business need.

## Testing

Four layers, all required per feature. Details in [docs/TESTING.md](docs/TESTING.md).

The single most important fact: **`testdata/money_cases.json` is read by both
the Go and the TypeScript suites**. It is the only thing preventing the client
from quoting the buyer one total while the server stores another. Verified by
sabotage — changing one expected value must fail *both* suites.

## Two boundaries Postgres does not draw

Both of this project's nastiest surprises came from assuming a per-database
boundary that does not exist. Written here because it will happen a third time:

- **Transaction ids are cluster-wide.** One open transaction anywhere freezes
  the sync feed everywhere. See gotcha #2.
- **Roles are cluster-wide.** The test harness altering `store_app`'s password
  locked the dev server out of its own database. See gotcha #6.

## More

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — diagrams and where decisions came from
- [docs/DECISIONS.md](docs/DECISIONS.md) — ADRs, including rejected alternatives
- [docs/SYNC.md](docs/SYNC.md) — the protocol and its correctness argument
- [docs/API.md](docs/API.md) — the response contract and error catalog
- [docs/INTEGRITY.md](docs/INTEGRITY.md) — the four levels, and what to do when `cmd/verify` fails
- [docs/GOTCHAS.md](docs/GOTCHAS.md) — traps already paid for
- [docs/TESTING.md](docs/TESTING.md) — the four layers, plus what only a real iPhone can check
- [docs/DEPLOY.md](docs/DEPLOY.md) — deployment and the restore runbook
- [docs/BACKLOG.md](docs/BACKLOG.md) — deliberately not built yet
