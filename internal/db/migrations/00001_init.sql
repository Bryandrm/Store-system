-- Initial schema for store-system.
--
-- Guiding principle: every fact that touches money or stock is an immutable
-- row, and every number the UI shows is a SUM(). See docs/DECISIONS.md ADR-000.
--
-- The grants at the bottom are NOT optional hardening: they are what turns
-- "append-only" from a convention into a guarantee. See ADR-009.

-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- roles
-- ============================================================================
-- store_app is the role the API runs as. It is created NOLOGIN and without a
-- password: the infrastructure assigns credentials with ALTER ROLE, so no
-- secret ever lives in the repository.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'store_app') THEN
    CREATE ROLE store_app NOLOGIN;
  END IF;
END
$$;

-- ============================================================================
-- users and sessions
-- ============================================================================
CREATE TABLE users (
  id            UUID PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  password_hash TEXT NOT NULL,                       -- argon2id encoded string
  role          TEXT NOT NULL CHECK (role IN ('owner', 'staff')),
  is_active     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
  token_sha256 BYTEA PRIMARY KEY,                    -- the raw token is never stored
  user_id      UUID NOT NULL REFERENCES users(id),
  device_label TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  revoked_at   TIMESTAMPTZ
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- ============================================================================
-- catalog
-- ============================================================================
CREATE TABLE products (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  category   TEXT NOT NULL DEFAULT 'general',
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,          -- visibility, NOT a soft delete
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- A functional index instead of a case-insensitive collation: the cluster runs
-- with --locale=C on purpose (see docs/DEPLOY.md).
CREATE UNIQUE INDEX products_name_lower_idx ON products (lower(name));

-- Price is a ledger, not a column. A mutable price_cents would be the system's
-- ONLY update-conflict surface. See ADR-004.
CREATE TABLE product_prices (
  id                 UUID PRIMARY KEY,
  product_id         UUID NOT NULL REFERENCES products(id),
  price_cents        BIGINT NOT NULL CHECK (price_cents >= 0),
  cost_cents         BIGINT CHECK (cost_cents >= 0),  -- optional; owner-only
  effective_from     TIMESTAMPTZ NOT NULL,
  note               TEXT,
  created_by_user_id UUID NOT NULL REFERENCES users(id),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX product_prices_lookup_idx
  ON product_prices (product_id, effective_from DESC, created_at DESC);

CREATE TABLE customers (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  phone      TEXT,
  notes      TEXT,
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX customers_name_lower_idx ON customers (lower(name));

-- ============================================================================
-- sales (append-only)
-- ============================================================================
CREATE TABLE sales (
  id                 UUID PRIMARY KEY,               -- client-generated UUIDv7
  customer_id        UUID REFERENCES customers(id),
  total_cents        BIGINT NOT NULL CHECK (total_cents >= 0),
  paid_cents         BIGINT NOT NULL CHECK (paid_cents >= 0),
  payment_method     TEXT NOT NULL
                       CHECK (payment_method IN ('cash', 'credit', 'mixed')),
  note               TEXT,
  occurred_at        TIMESTAMPTZ NOT NULL,           -- device clock
  recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),  -- server clock
  clock_skew_flagged BOOLEAN NOT NULL DEFAULT FALSE,
  device_id          TEXT NOT NULL,
  -- created_by: who the operation claims made the sale.
  -- synced_by:  the owner of the token that uploaded it.
  -- Storing both keeps "logged back in as the other person" auditable.
  created_by_user_id UUID NOT NULL REFERENCES users(id),
  synced_by_user_id  UUID NOT NULL REFERENCES users(id),
  -- A credit sale cannot be fully paid, and a cash sale cannot be unpaid.
  CONSTRAINT sales_payment_coherent CHECK (
    (payment_method = 'cash'   AND paid_cents = total_cents) OR
    (payment_method = 'credit' AND paid_cents = 0)           OR
    (payment_method = 'mixed'  AND paid_cents > 0 AND paid_cents < total_cents)
  )
);
CREATE INDEX sales_occurred_at_idx ON sales (occurred_at DESC);
CREATE INDEX sales_customer_idx    ON sales (customer_id) WHERE customer_id IS NOT NULL;

CREATE TABLE sale_lines (
  id                    UUID PRIMARY KEY,
  sale_id               UUID NOT NULL REFERENCES sales(id),
  product_id            UUID NOT NULL REFERENCES products(id),
  qty_milli             BIGINT NOT NULL CHECK (qty_milli > 0),   -- thousandths of a unit
  unit_price_cents      BIGINT NOT NULL CHECK (unit_price_cents >= 0),
  line_total_cents      BIGINT NOT NULL CHECK (line_total_cents >= 0),
  product_name_snapshot TEXT NOT NULL,               -- immune to later renames
  line_no               INTEGER NOT NULL CHECK (line_no > 0)
);
CREATE UNIQUE INDEX sale_lines_sale_line_no_idx ON sale_lines (sale_id, line_no);
CREATE INDEX sale_lines_sale_idx    ON sale_lines (sale_id);
CREATE INDEX sale_lines_product_idx ON sale_lines (product_id);

-- Voiding is a compensating append. It is the ONLY way to "delete" a
-- transaction: the sales row itself is never touched.
CREATE TABLE sale_voids (
  sale_id           UUID PRIMARY KEY REFERENCES sales(id),
  reason            TEXT NOT NULL,
  occurred_at       TIMESTAMPTZ NOT NULL,
  recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  voided_by_user_id UUID NOT NULL REFERENCES users(id)
);

-- ============================================================================
-- restocking
-- ============================================================================
CREATE TABLE restocks (
  id                 UUID PRIMARY KEY,
  supplier_name      TEXT,
  total_cost_cents   BIGINT NOT NULL DEFAULT 0 CHECK (total_cost_cents >= 0),
  note               TEXT,
  occurred_at        TIMESTAMPTZ NOT NULL,
  recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by_user_id UUID NOT NULL REFERENCES users(id)
);

CREATE TABLE restock_lines (
  id              UUID PRIMARY KEY,
  restock_id      UUID NOT NULL REFERENCES restocks(id),
  product_id      UUID NOT NULL REFERENCES products(id),
  qty_milli       BIGINT NOT NULL CHECK (qty_milli > 0),
  unit_cost_cents BIGINT NOT NULL DEFAULT 0 CHECK (unit_cost_cents >= 0),
  line_no         INTEGER NOT NULL CHECK (line_no > 0)
);
CREATE UNIQUE INDEX restock_lines_line_no_idx ON restock_lines (restock_id, line_no);
CREATE INDEX restock_lines_restock_idx ON restock_lines (restock_id);

-- ============================================================================
-- LEDGER 1: stock
-- ============================================================================
-- Stock is NOT stored. It is SUM(delta_qty_milli). A zero delta would mean an
-- operation was half-applied, which is why it is forbidden.
CREATE TABLE stock_movements (
  id                 UUID PRIMARY KEY,
  product_id         UUID NOT NULL REFERENCES products(id),
  delta_qty_milli    BIGINT NOT NULL CHECK (delta_qty_milli <> 0),
  reason             TEXT NOT NULL CHECK (reason IN
                       ('sale', 'sale_void', 'restock', 'adjustment', 'loss', 'initial')),
  ref_kind           TEXT NOT NULL CHECK (ref_kind IN ('sale', 'restock', 'manual')),
  ref_id             UUID,
  note               TEXT,
  occurred_at        TIMESTAMPTZ NOT NULL,
  recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by_user_id UUID NOT NULL REFERENCES users(id),
  -- If a movement claims to come from a sale or a restock, it must say which.
  CONSTRAINT stock_movements_ref_coherent CHECK (
    (ref_kind = 'manual' AND ref_id IS NULL) OR
    (ref_kind <> 'manual' AND ref_id IS NOT NULL)
  )
);
CREATE INDEX stock_movements_product_idx  ON stock_movements (product_id);
CREATE INDEX stock_movements_ref_idx      ON stock_movements (ref_kind, ref_id);
CREATE INDEX stock_movements_occurred_idx ON stock_movements (occurred_at DESC);

-- ============================================================================
-- LEDGER 2: customer account
-- ============================================================================
-- Sign:  (+) the customer has credit (left extra money)
--        (-) the customer owes money
CREATE TABLE customer_ledger (
  id                 UUID PRIMARY KEY,
  customer_id        UUID NOT NULL REFERENCES customers(id),
  delta_cents        BIGINT NOT NULL CHECK (delta_cents <> 0),
  kind               TEXT NOT NULL CHECK (kind IN
                       ('sale_credit', 'payment', 'adjustment', 'sale_void')),
  ref_kind           TEXT NOT NULL CHECK (ref_kind IN ('sale', 'payment', 'manual')),
  ref_id             UUID,
  note               TEXT,
  occurred_at        TIMESTAMPTZ NOT NULL,
  recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by_user_id UUID NOT NULL REFERENCES users(id),
  CONSTRAINT customer_ledger_ref_coherent CHECK (
    (ref_kind = 'manual' AND ref_id IS NULL) OR
    (ref_kind <> 'manual' AND ref_id IS NOT NULL)
  )
);
CREATE INDEX customer_ledger_customer_idx ON customer_ledger (customer_id);
CREATE INDEX customer_ledger_ref_idx      ON customer_ledger (ref_kind, ref_id);

CREATE TABLE payments (
  id                 UUID PRIMARY KEY,
  customer_id        UUID NOT NULL REFERENCES customers(id),
  amount_cents       BIGINT NOT NULL CHECK (amount_cents > 0),
  method             TEXT NOT NULL DEFAULT 'cash',
  note               TEXT,
  occurred_at        TIMESTAMPTZ NOT NULL,
  recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by_user_id UUID NOT NULL REFERENCES users(id)
);
CREATE INDEX payments_customer_idx ON payments (customer_id);

-- ============================================================================
-- synchronization
-- ============================================================================
-- xact_id is the real cursor. seq survives only as a deterministic tiebreak
-- WITHIN one transaction: sequences are not transactional, and using one as a
-- cursor silently drops rows. See ADR-001.
CREATE TABLE change_log (
  seq        BIGSERIAL PRIMARY KEY,
  xact_id    XID8  NOT NULL DEFAULT pg_current_xact_id(),
  entity     TEXT  NOT NULL,
  entity_id  UUID  NOT NULL,
  op         TEXT  NOT NULL CHECK (op IN ('insert', 'update')),
  payload    JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX change_log_xact_idx ON change_log (xact_id, seq);

-- Retention floor. A client whose cursor fell below this lost history and must
-- re-bootstrap (CURSOR_TOO_OLD). Without this table, pruning creates silent gaps.
CREATE TABLE change_log_floor (
  singleton            BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  min_retained_xact_id XID8 NOT NULL DEFAULT '0'::xid8
);
INSERT INTO change_log_floor DEFAULT VALUES;

-- Idempotency ledger. Inserted in the SAME transaction as the domain rows,
-- which is why replaying an operation never duplicates anything.
CREATE TABLE sync_operations (
  op_id         UUID PRIMARY KEY,                    -- client-generated UUIDv7
  device_id     TEXT NOT NULL,
  user_id       UUID NOT NULL REFERENCES users(id),
  op_type       TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('applied', 'rejected')),
  error_code    TEXT,
  error_message TEXT,
  request_hash  BYTEA NOT NULL,                      -- sha256 of the payload
  applied_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sync_operations_device_idx ON sync_operations (device_id, applied_at DESC);

-- ============================================================================
-- derived views
-- ============================================================================
-- Views rather than materialized tables: a SUM over 50k movements with the
-- index takes <10 ms. Revisit at roughly 1M rows (about 20 years).
CREATE VIEW stock_levels AS
SELECT p.id AS product_id,
       COALESCE(SUM(m.delta_qty_milli), 0)::BIGINT AS qty_milli
FROM products p
LEFT JOIN stock_movements m ON m.product_id = p.id
GROUP BY p.id;

CREATE VIEW customer_balances AS
SELECT c.id AS customer_id,
       COALESCE(SUM(l.delta_cents), 0)::BIGINT AS balance_cents
FROM customers c
LEFT JOIN customer_ledger l ON l.customer_id = c.id
GROUP BY c.id;

CREATE VIEW current_prices AS
SELECT DISTINCT ON (product_id)
       product_id, price_cents, cost_cents, effective_from
FROM product_prices
WHERE effective_from <= now()
ORDER BY product_id, effective_from DESC, created_at DESC;

-- ============================================================================
-- grants: where append-only stops being discipline
-- ============================================================================
GRANT USAGE ON SCHEMA public TO store_app;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO store_app;

-- change_log.seq is BIGSERIAL: without USAGE on the sequence every INSERT fails.
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO store_app;

-- The transactional tables are locked down. A stray UPDATE introduced six
-- months from now fails with permission denied on its first run, instead of
-- silently corrupting the books.
REVOKE UPDATE, DELETE ON
  sales, sale_lines, sale_voids,
  stock_movements, customer_ledger, payments,
  product_prices, restocks, restock_lines,
  change_log, sync_operations
FROM store_app;

-- Only the catalog and sessions accept UPDATE: cosmetic last-write-wins
-- metadata, plus the session's last_seen_at.
GRANT UPDATE ON products, customers, users, sessions TO store_app;

-- change_log_floor is written by the pruner, which runs as the migrator role.
REVOKE UPDATE, DELETE ON change_log_floor FROM store_app;

-- The migration history belongs to the migrator role. GRANT ON ALL TABLES also
-- sweeps up goose_db_version, because goose creates it before applying the
-- first migration; without this REVOKE the application could forge history.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_tables
    WHERE schemaname = 'public' AND tablename = 'goose_db_version'
  ) THEN
    REVOKE ALL ON goose_db_version FROM store_app;
  END IF;
END
$$;

-- Tables added by future migrations inherit the same base treatment.
-- NOTE: any migration adding a transactional table must issue its own explicit
-- REVOKE. This only covers the base GRANT.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT ON TABLES TO store_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO store_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT, INSERT ON TABLES FROM store_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE USAGE, SELECT ON SEQUENCES FROM store_app;

DROP VIEW IF EXISTS current_prices;
DROP VIEW IF EXISTS customer_balances;
DROP VIEW IF EXISTS stock_levels;

DROP TABLE IF EXISTS sync_operations;
DROP TABLE IF EXISTS change_log_floor;
DROP TABLE IF EXISTS change_log;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS customer_ledger;
DROP TABLE IF EXISTS stock_movements;
DROP TABLE IF EXISTS restock_lines;
DROP TABLE IF EXISTS restocks;
DROP TABLE IF EXISTS sale_voids;
DROP TABLE IF EXISTS sale_lines;
DROP TABLE IF EXISTS sales;
DROP TABLE IF EXISTS customers;
DROP TABLE IF EXISTS product_prices;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;

-- The role is cluster-wide and may be in use by another database; not dropped.

-- +goose StatementEnd
