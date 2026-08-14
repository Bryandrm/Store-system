# Gotchas

Numbered ledger of traps this project has actually paid for. Format is
**symptom → cause → fix**, written so the symptom is searchable, because that is
what you will have when it bites again.

Only real ones go here. A hypothetical worry belongs in `BACKLOG.md`.

---

## #1 — Postgres 18 container restarts in a loop

**Symptom.** `docker compose up` brings Postgres up and it immediately restarts,
over and over. The logs mention `pg_ctlcluster`, `pg_upgrade --link` and
"there appears to be PostgreSQL data in: /var/lib/postgresql/data (unused
mount/volume)". Nothing obviously wrong with the compose file.

**Cause.** From Postgres 18 onward the official images store data in a
subdirectory named after the major version, so that `pg_upgrade --link` works
without crossing a mount boundary. Mounting the volume at
`/var/lib/postgresql/data` — the convention for 17 and earlier, and what every
tutorial still shows — puts the data where the entrypoint refuses to use it.

**Fix.** Mount one level up:

```yaml
volumes:
  - postgres_dev_data:/var/lib/postgresql   # NOT /data
```

---

## #2 — The sync feed stalls, and a transaction in a different database is to blame

**Symptom.** `ReadFeed` returns nothing even though rows were just committed and
are visible to `SELECT`. In tests it looks like flakiness that only appears when
tests run in parallel.

**Cause.** Postgres transaction ids are **cluster-wide, not per-database**. The
feed's watermark comes from `pg_snapshot_xmin(pg_current_snapshot())`, which is
the lowest transaction id still running **anywhere in the instance**. One open
transaction, in any database, holds the watermark down and withholds every newer
row from every client.

This is correct behaviour — it is exactly what stops the feed from skipping a
row that has not committed yet — but the blast radius is wider than it looks.

**Fix, in production.** Treat long transactions as an outage, not an
inefficiency:

- `idle_in_transaction_session_timeout = 30s` and `statement_timeout = 15s` on
  the application role. Not tuning knobs: load-bearing.
- Never do network I/O inside a transaction.
- Schedule `pg_dump` off-peak. It holds a snapshot, so **the feed freezes for
  the duration of the backup** (~2 s at current size, growing with the data).

**Fix, in tests.** The tests in `internal/sync` do not call `t.Parallel()`. One
test holding an open transaction would make another test's committed rows
invisible, and the failure reads like a bug in the feed rather than a shared
cluster resource.

---

## #3 — The application role could forge the migration history

**Symptom.** None. Found by reading `information_schema.role_table_grants` while
verifying the append-only lockdown by hand.

**Cause.** The migration ends with
`GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO store_app`. goose creates
its `goose_db_version` table **before** applying the first migration, so it is
already present when that grant runs and gets swept up with everything else. The
application ended up able to insert migration records.

**Fix.** An explicit revoke at the end of the migration, guarded so it does not
fail if the table is absent:

```sql
REVOKE ALL ON goose_db_version FROM store_app;
```

`TestMigrationHistoryProtected` now watches it.

**The general lesson.** `ON ALL TABLES` grants over whatever happens to exist at
that instant. Any migration that adds a transactional table must issue its own
`REVOKE`; the blanket grant only ever covers the base case.

---

## #4 — A stale client is told it is up to date

**Symptom.** A client whose cursor fell below the retention floor received an
empty, successful feed instead of `CURSOR_TOO_OLD`. It would go on syncing
forever, permanently missing everything pruned out from under it.

**Cause.** Check order in `ReadFeed`. The "nothing new" shortcut
(`cursor >= watermark`) ran **before** the retention-floor check, so a stale
cursor that happened to sit above the current watermark took the early return
and never reached the check.

**Fix.** The retention floor is validated first, unconditionally. Whether the
client lost history does not depend on where the watermark happens to be.

**The general lesson.** An early return that means "you are fine" must come
after every check that can mean "you are not". The failure mode of getting this
backwards is silence, which is the worst kind in a sync protocol.

---

## #5 — `pnpm install` fails with ERR_PNPM_IGNORED_BUILDS

**Symptom.** A clean `pnpm install` exits non-zero with
`ERR_PNPM_IGNORED_BUILDS: Ignored build scripts: esbuild`, and suggests running
the interactive `pnpm approve-builds`. Every `pnpm` script then refuses to run,
because pnpm checks dependency status first. In CI it just fails.

**Cause.** pnpm blocks package install scripts by default as a supply-chain
measure, and treats an unreviewed one as an error rather than a warning.
esbuild — a transitive dependency of Vite — needs its script to link the
platform binary.

**Fix.** Approve it in `web/pnpm-workspace.yaml`, so the decision is in version
control and CI needs no interactive step:

```yaml
allowBuilds:
  esbuild: true
```

**The part that costs time.** The setting has moved twice. It used to live in
`package.json` under `pnpm.onlyBuiltDependencies`, then in `.npmrc`, and pnpm 11
replaced `onlyBuiltDependencies` (a list) with **`allowBuilds` (a map)** in
`pnpm-workspace.yaml`. The older spellings are silently ignored — pnpm warns
about the `package.json` field but not about the rest, so the install keeps
failing with the same message while the config looks correct.

---

## #6 — Running the tests locks the dev server out of its database

**Symptom.** The API starts, applies migrations, and then dies with
`failed SASL auth: FATAL: password authentication failed for user "store_app"`.
It worked minutes earlier and nothing in the config changed. The trigger, once
you spot it, is having run `go test ./...` in between.

**Cause.** Postgres roles are **cluster-wide**, like transaction ids (see #2).
The test harness calls `ALTER ROLE store_app LOGIN PASSWORD …` so tests can
connect as the production role and genuinely exercise the grants — but that
statement reaches every database in the instance, not just the throwaway ones it
creates.

**Fix.** The harness and the development server use the same password constant,
and `internal/testdb/testdb.go` says why. Production is unaffected: the password
there comes from the infrastructure, and neither the tests nor the dev server
run against it.

**The general lesson.** A test harness that touches anything cluster-scoped —
roles, tablespaces, replication slots — is not isolated just because it creates
its own database. Both of this project's nastiest surprises came from assuming
a per-database boundary that Postgres does not draw.

---

## #7 — Playwright tests a stale build for an hour

**Symptom.** A fix is applied, the source is unmistakably correct, and the
end-to-end suite keeps failing on it. The accessibility snapshot shows the old
markup. Rebuilding by hand and inspecting `dist/` shows the new code is there.

**Cause.** Two things compounding.

`reuseExistingServer: true` — the default outside CI — makes Playwright reuse a
running web server and **skip its command entirely**. Since the command is
`pnpm build && pnpm preview`, reusing the server means the build never runs and
the suite exercises whatever `dist/` happened to contain.

And the server survived every attempt to kill it: `pkill -f "vite preview"`
matches nothing, because the real process is `node …/vite.js preview`. The
pattern that works is by port:

```bash
lsof -ti:4173 | xargs kill -9
```

**Fix.** `reuseExistingServer: false` for the web server, so the build always
runs. The extra second per run is worth never debugging phantom failures again.

**The general lesson.** When a fix "does not take", verify what is actually
being executed before re-reading the source. The bug was one layer below where
it was being looked for, and every minute spent re-reading correct code was
wasted.
