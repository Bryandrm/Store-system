# Backend integrity

How the books are kept honest, and what to do when `cmd/verify` says they are
not.

## Four levels, strongest first

### 1. Postgres grants — append-only stops being discipline

`store_app`, the role the API runs as, holds `SELECT, INSERT` and is explicitly
denied `UPDATE, DELETE` on every transactional table.

This is the strongest guarantee in the system. An `UPDATE sales SET total_cents
= …` introduced by mistake six months from now fails with `permission denied`
on its first run, instead of silently corrupting the books.

It has already earned its keep: the blanket `GRANT ON ALL TABLES` swept up
goose's migration table, which let the application forge migration history. See
gotcha #3.

`DELETE` exists in exactly two places, both running as the migrator role: the
`change_log` pruner and session revocation.

### 2. Declarative constraints

`CHECK` on every enum, on every sign, and on non-zero deltas. `FOREIGN KEY` on
every reference. `UNIQUE` on `(sale_id, line_no)` and on `lower(name)`.

A zero delta is the quiet trap: it looks harmless, but it means an operation was
half-applied. Better that it fails loudly.

### 3. Cross-table invariants — `cmd/verify`

A `CHECK` sees one row. It cannot say *"the sum of these lines equals that
sale's total"*, because that spans rows in another table. Those are exactly the
properties that matter here, since recording a sale writes four tables in one
transaction.

**The division of labour:** level 1 stops a value from being **corrupted**;
level 3 detects two individually valid values that have become **incoherent**
with each other.

Twelve invariants, listed in `internal/verify/verify.go`. The ones that protect
money most directly:

| Invariant | What its failure means |
|---|---|
| `sale_lines_sum_to_total` | The recorded total is not what was charged |
| `credit_sale_has_matching_ledger_entry` | A debt was forgiven, doubled, or recorded wrong |
| `cash_sale_has_no_debt` | A customer is being billed for a sale they paid |
| `sale_line_moved_stock` | Product left the shelf and inventory still counts it |
| `sale_stock_movement_matches_quantity` | A sign error inventing or destroying stock |

**Verified by sabotage.** Every invariant has a test that deliberately corrupts
the database — reaching around the grants with the migrator role — and asserts
the check fires. A checker that never fires is not verified.

Two things those tests also assert: a clean database produces **zero**
violations, and each corruption trips **only** the relevant check. A noisy check
stops being read.

### 4. Code discipline

- One operation, one transaction, through `db.WithTx`, which guarantees rollback
  on error or panic. No loose `pool.Exec` in domain code.
- Never network I/O inside a transaction. Beyond the obvious: an open
  transaction pins `xmin` and **freezes the sync feed** for every device.
- `go test -race` always. In a system with a sync engine, the race detector is
  not optional.
- Validation lives in one place per operation, never split between handler and
  service.

## Data at rest

- **`--data-checksums`** at cluster initialization. Detects silent disk
  corruption rather than serving broken bytes as data. It matters here because
  the free-tier disk is spinning rust, and it **cannot be enabled later without
  recreating the cluster** — a one-shot decision that had to be right on day
  one.
- `synchronous_commit = on`. Not a tuning knob: it is money.
- The monthly restore drill runs `cmd/verify` **against the restored copy**, not
  just a row count.

## When `cmd/verify` fails

```
DATABASE_URL=… go run ./cmd/verify
```

Exit codes: `0` all hold · `1` violations found · `2` could not run.

### First: do not panic, and do not "fix" the data

Nothing in this system overwrites anything, so **the history that produced the
inconsistency is still there**. That is the whole point of the append-only
design: you can reconstruct what happened. Rewriting rows to make the check pass
destroys the evidence and is almost never the right move.

### Triage

**1. Is this production, or a restored backup?**

If it is a restored backup, the restore is not trustworthy. Try an older one
before doing anything else, and do not overwrite the failing copy — it is
evidence.

**2. How many rows, and are they clustered in time?**

```sql
SELECT date_trunc('day', recorded_at), count(*)
FROM sales GROUP BY 1 ORDER BY 1 DESC LIMIT 14;
```

A handful of rows from one day points at a specific deploy or a specific device.
A steady trickle across months points at a logic bug that has been running all
along, which is worse but also easier to find.

**3. Which invariant, and what does it imply?**

| Invariant | Likely cause | Effect on the business |
|---|---|---|
| `sale_lines_sum_to_total` | Client and server arithmetic drifted | The recorded total is wrong |
| `line_total_matches_arithmetic` | The shared money fixture stopped being shared | Systematic, affects everything after the drift |
| `sale_line_moved_stock` | A partially applied sale transaction | Inventory reads high |
| `credit_sale_has_matching_ledger_entry` | A partially applied credit sale | Somebody owes money the system forgot, or is billed twice |
| `stock_movement_references_real_sale` | A sale was deleted, which should be impossible | Suspect a manual intervention |
| `every_product_has_a_price` | A product created without one | It sells for $0.00 |

**4. Correct forward, never in place.**

Corrections are compensating inserts, exactly like every other change:

```sql
-- Stock missing for a sale that really happened: append the movement.
INSERT INTO stock_movements
  (id, product_id, delta_qty_milli, reason, ref_kind, ref_id, occurred_at, created_by_user_id)
SELECT gen_random_uuid(), l.product_id, -l.qty_milli, 'sale', 'sale',
       l.sale_id, s.occurred_at, s.created_by_user_id
FROM sale_lines l JOIN sales s ON s.id = l.sale_id
WHERE l.sale_id = '<sale-id>';
```

If a sale is genuinely wrong rather than incomplete, **void it** and record the
correct one. That leaves both facts in the history, which is what an audit needs
and what a silent `UPDATE` destroys.

**5. Write the gotcha.**

If the cause was a real defect, it belongs in [GOTCHAS.md](GOTCHAS.md) in the
symptom → cause → fix format. The symptom is what a future reader will have.

### If a check itself turns out to be wrong

That happens, and it is not shameful. But **fix the check, do not delete it**,
and add a test for the case it got wrong — otherwise the next person deletes it
again for the same reason.
