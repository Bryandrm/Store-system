# Backlog

States: `[ ]` not started · `[~]` in progress · `[x]` done

The **Deliberately not built yet** section at the bottom is the important one.
Each entry there records a decision *not* to build something, plus the concrete
trigger that would change the answer. Without the trigger written down, these
get rebuilt from scratch as arguments every few months.

---

## M1 — "Sell and see the day"

- [x] Full schema, ledgers, derived views, append-only enforced by grants
- [x] Money arithmetic with a fixture shared between Go and TypeScript
- [x] Throwaway-database test harness, running as the production role
- [x] Single HTTP response contract
- [x] Opaque bearer tokens, argon2id, login rate limiting
- [x] Sale recorded as one atomic transaction
- [x] `xid8` watermark feed and idempotent apply
- [x] `/bootstrap` and `/sync`, verified end to end with curl
- [x] Local replica, single write path, operation outbox
- [x] Sync engine with failure classification and Web Locks
- [x] Sell screen, day view, installable PWA shell
- [x] iOS mitigations: `storage.persist`, stale-pending warning, install hint
- [ ] Playwright: `login`, `venta`, `venta-offline`, `recarga-carrito`
- [ ] `cmd/verify` with the sale invariants
- [ ] Integration tests 1, 2 and 11
- [ ] Production compose, Caddy, domain, GCP e2-micro
- [ ] Frontend on Cloudflare Pages
- [ ] GitHub Actions: test → build → deploy

**Done criterion.** One real day of selling, on a phone, including a stretch
with mobile data off, and the sales are on the server afterwards.

---

## M2 — "Inventory and derived stock"

- [ ] `adjust_stock` operation and the physical-count screen
- [ ] Stock screen with the negative-stock warning
- [ ] `change_log` pruner maintaining `change_log_floor`
- [ ] Nightly backup to Cloudflare R2
- [ ] **First manual restore drill** — not optional, not deferred
- [ ] Integration tests 1, 8, 12; Playwright `dos-dispositivos`, `inventario`

## M3 — "Customer debt"

- [ ] Customers: create and edit
- [ ] Credit and mixed sales from the sell screen
- [ ] `record_payment` operation with oldest-first allocation
- [ ] Per-customer statement and a "who owes me" list
- [ ] Void a sale, with compensating entries
- [ ] Integration tests 2, 3, 4, 5, 6; Playwright `fiado`, `saldo-a-favor`

## M4 — "Administration"

*Brought forward from the original plan: the app should not depend on `cmd/seed`
for anything but the very first account.*

- [ ] Products: create, edit, deactivate, reorder
- [ ] Price changes, with history visible
- [ ] Restocking with cost, and a low-stock alert
- [ ] Users: create, edit, deactivate (owner only)
- [ ] Sessions: list and revoke
- [ ] Integration tests 7, 9, 10; Playwright `sesion-vencida`

## M5 — "Reports and hardening"

- [ ] Client-side reports: day, week, month, top products, margin
- [ ] CSV export — also the third independent copy of the data
- [ ] Cloudflare proxy in front of the API, firewall restricted to its ranges
- [ ] Automated restore drill in CI, running `cmd/verify` on the restored copy
- [ ] Egress monitoring

---

## Deliberately not built yet

### Stock snapshots

**Not building.** `stock_levels` sums the movement ledger on every read.

**Trigger:** roughly one million movement rows, about twenty years at fifty
sales a day. If it ever arrives:
`stock_snapshots(product_id, as_of_xact_id, qty_milli)` plus summing the tail.

### Bootstrap with a cutoff

**Not building.** `/bootstrap` sends the entire history.

**Trigger:** payload above ~5 MB, roughly two years. The cutoff must ship
**together with opening balances** — truncating ledger history without them
silently corrupts every derived number, which is a far worse bug than a slow
first load.

### Incremental backups

**Not building.** A nightly full `pg_dump`.

**Trigger:** monthly egress from the VM approaching 500 MB. At five years the
nightly dump is ~15 MB, which is half the free-tier budget. The fix is monthly
fulls plus weekly dumps, not thirty dailies.

### Workload Identity Federation for deploys

**Not building.** A dedicated SSH key in GitHub secrets.

**Trigger:** anyone other than the owner gaining repository access. WIF plus
`gcloud compute ssh --tunnel-through-iap` removes the long-lived key.

### Multi-store tenancy

**Not building.** See [ADR-012](DECISIONS.md#adr-012).

**Trigger:** someone other than the two current users wanting to run their own
store on it. The cost is real but bounded — `store_id` on every table plus a
scope check — and the append-only design means every device can simply
re-bootstrap.

### Background Sync API

**Not building.** The sync engine polls and reacts to `online` and
`visibilitychange`.

**Trigger:** none foreseen. It is Chromium-only, and buys nothing for an app
used in the hand rather than in the background.

### Conflict-resolution UI

**Never building.** There are no conflicts; that is the entire point of the
design. Any request for this is a request to break the core invariant — see
[ADR-000](DECISIONS.md#adr-000).

### Percentage discounts

**Never building.** Absolute cents only. Percentages are the classic
rounding-drift generator and there is no business need for them.
