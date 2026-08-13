# store-system

Point-of-sale system for a small snack stand: fast sale entry, inventory,
restocking, store credit and customer debt. An installable PWA that **works with
no signal** and syncs across devices.

The products are sunflower seeds, peanuts, sweet peanuts, Japanese peanuts,
cashews, pumpkin seeds, chocolates and churros. The stand is often out of mobile
coverage, so working offline is not a nicety: it is the primary requirement.

## The idea in one sentence

> **Every fact that touches money or stock is an immutable row. Every number the
> UI shows is a `SUM()`. Offline devices only ever `INSERT`.**

From that follows the central property: **no sync conflict can exist by
construction**. And it is not left to the discipline of the code — the role the
API runs as **has no `UPDATE` or `DELETE` privilege** on any transactional table.

## Stack

| Layer | Choice |
|---|---|
| Backend | Go 1.26 · stdlib `net/http` · `pgx` · `sqlc` · `goose` |
| Database | PostgreSQL 18 with `data-checksums` |
| Frontend | React 19 · Vite · strict TypeScript · Tailwind v4 · Zustand |
| Offline | IndexedDB · hand-written service worker · operation outbox |
| Infra | Cloudflare Pages (PWA) + GCP e2-micro (API) + Caddy |

No ORM, no HTTP framework, no component library.

## Getting started

```bash
# 1. the development database (port 5433, to avoid clashing with another Postgres)
docker compose -f compose.dev.yml up -d

# 2. the tests, which create and drop their own throwaway databases
go test ./... -race
```

Requirements: Go 1.26+, Docker or Colima, Node 22+, pnpm.

```bash
cd web
pnpm install
pnpm dev        # :5173, proxies /api to :8080
pnpm test
```

## Documentation

| File | Contents |
|---|---|
| [CLAUDE.md](CLAUDE.md) | Operating manual: principles, layout, things not to do |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | The four diagrams and where each decision comes from |
| [docs/DECISIONS.md](docs/DECISIONS.md) | ADRs, including the rejected alternatives |
| [docs/SYNC.md](docs/SYNC.md) | The protocol and its correctness argument |
| [docs/API.md](docs/API.md) | The response contract and the error catalog |
| [docs/TESTING.md](docs/TESTING.md) | The four layers, and what only a real iPhone can check |
| [docs/GOTCHAS.md](docs/GOTCHAS.md) | Numbered ledger of traps already paid for |
| [docs/BACKLOG.md](docs/BACKLOG.md) | What is next, and what is deliberately not built |
| `docs/DEPLOY.md` | Deployment and restore runbook *(pending)* |

## Status

In development, milestone 1 (**"Sell and see the day"**).

| Component | Status |
|---|---|
| Schema, ledgers and derived views | ✅ migrated and tested |
| Append-only enforcement via grants | ✅ 12 tables verified in CI |
| Money arithmetic + shared fixture | ✅ verified by sabotage |
| Throwaway database harness | ✅ runs as the production role |
| HTTP contract | ✅ |
| Authentication | ✅ opaque tokens, argon2id, rate limited |
| Sales, sync feed and bootstrap | ✅ verified end to end |
| PWA: local replica, outbox, sync engine | ✅ |
| PWA: sell screen and day view | ✅ builds and runs, not yet exercised |
| Playwright | ⬜ |
| `cmd/verify` and integration tests | ⬜ |
| Deployment | ⬜ |

## Conventions

Conventional commits with the module as the scope, so the changelog can be
generated grouped by module:

```
feat(sales):     quick entry with a numeric keypad
fix(sync):       the outbox stopped retrying after a 401
feat(inventory): low stock warning
```

Scopes: `sales` · `inventory` · `credit` · `restock` · `profiles` · `sync` ·
`api` · `infra` · `docs`.

Code, comments, documentation and commit messages are written in English.
User-facing strings — the app's interface and API error messages — are in
Spanish, because that is what the people running the store read.
