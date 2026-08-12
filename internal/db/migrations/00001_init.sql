-- Esquema inicial de store-system.
--
-- Principio rector: todo dato que toca plata o stock es una fila inmutable, y
-- todo numero que la UI muestra es un SUM(). Ver docs/DECISIONS.md ADR-000.
--
-- Los permisos del final NO son endurecimiento opcional: son lo que convierte
-- "append-only" de convencion en garantia. Ver ADR-009.

-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- roles
-- ============================================================================
-- store_app es el rol que usa la API en runtime. Se crea sin LOGIN y sin
-- password: la infraestructura le asigna credenciales con ALTER ROLE, de modo
-- que ningun secreto vive en el repositorio.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'store_app') THEN
    CREATE ROLE store_app NOLOGIN;
  END IF;
END
$$;

-- ============================================================================
-- usuarios y sesiones
-- ============================================================================
CREATE TABLE users (
  id            UUID PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  password_hash TEXT NOT NULL,                       -- cadena codificada argon2id
  role          TEXT NOT NULL CHECK (role IN ('owner', 'staff')),
  is_active     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
  token_sha256 BYTEA PRIMARY KEY,                    -- nunca se guarda el token en claro
  user_id      UUID NOT NULL REFERENCES users(id),
  device_label TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  revoked_at   TIMESTAMPTZ
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- ============================================================================
-- catalogo
-- ============================================================================
CREATE TABLE products (
  id         UUID PRIMARY KEY,
  name       TEXT NOT NULL,
  category   TEXT NOT NULL DEFAULT 'general',
  is_active  BOOLEAN NOT NULL DEFAULT TRUE,          -- visibilidad, NO borrado logico
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- indice funcional en lugar de colacion case-insensitive: el cluster usa
-- --locale=C a proposito (ver docs/DEPLOY.md).
CREATE UNIQUE INDEX products_name_lower_idx ON products (lower(name));

-- El precio es un ledger, no una columna. Un price_cents mutable seria la
-- UNICA superficie de conflicto de update del sistema. Ver ADR-004.
CREATE TABLE product_prices (
  id                 UUID PRIMARY KEY,
  product_id         UUID NOT NULL REFERENCES products(id),
  price_cents        BIGINT NOT NULL CHECK (price_cents >= 0),
  cost_cents         BIGINT CHECK (cost_cents >= 0),  -- opcional; solo lo ve owner
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
-- ventas (append-only)
-- ============================================================================
CREATE TABLE sales (
  id                 UUID PRIMARY KEY,               -- UUIDv7 generado en el cliente
  customer_id        UUID REFERENCES customers(id),
  total_cents        BIGINT NOT NULL CHECK (total_cents >= 0),
  paid_cents         BIGINT NOT NULL CHECK (paid_cents >= 0),
  payment_method     TEXT NOT NULL
                       CHECK (payment_method IN ('cash', 'credit', 'mixed')),
  note               TEXT,
  occurred_at        TIMESTAMPTZ NOT NULL,           -- reloj del dispositivo
  recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),  -- reloj del servidor
  clock_skew_flagged BOOLEAN NOT NULL DEFAULT FALSE,
  device_id          TEXT NOT NULL,
  -- created_by: quien dice la operacion que vendio.
  -- synced_by:  el dueño del token que la subio.
  -- Guardar ambos hace auditable el caso de re-login como la otra persona.
  created_by_user_id UUID NOT NULL REFERENCES users(id),
  synced_by_user_id  UUID NOT NULL REFERENCES users(id),
  -- una venta a credito no puede estar totalmente pagada, y viceversa
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
  qty_milli             BIGINT NOT NULL CHECK (qty_milli > 0),   -- milesimas de unidad
  unit_price_cents      BIGINT NOT NULL CHECK (unit_price_cents >= 0),
  line_total_cents      BIGINT NOT NULL CHECK (line_total_cents >= 0),
  product_name_snapshot TEXT NOT NULL,               -- inmune a renombres posteriores
  line_no               INTEGER NOT NULL CHECK (line_no > 0)
);
CREATE UNIQUE INDEX sale_lines_sale_line_no_idx ON sale_lines (sale_id, line_no);
CREATE INDEX sale_lines_sale_idx    ON sale_lines (sale_id);
CREATE INDEX sale_lines_product_idx ON sale_lines (product_id);

-- Anular es un append compensatorio. Es la UNICA forma de "borrar" una
-- transaccion: la fila de sales nunca se toca.
CREATE TABLE sale_voids (
  sale_id           UUID PRIMARY KEY REFERENCES sales(id),
  reason            TEXT NOT NULL,
  occurred_at       TIMESTAMPTZ NOT NULL,
  recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  voided_by_user_id UUID NOT NULL REFERENCES users(id)
);

-- ============================================================================
-- reabastecimiento
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
-- LEDGER 1: existencias
-- ============================================================================
-- El stock NO se guarda. Es SUM(delta_qty_milli). Un delta de cero significa
-- que una operacion se aplico a medias, por eso esta prohibido.
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
  -- si el movimiento dice venir de una venta o un restock, tiene que decir cual
  CONSTRAINT stock_movements_ref_coherent CHECK (
    (ref_kind = 'manual' AND ref_id IS NULL) OR
    (ref_kind <> 'manual' AND ref_id IS NOT NULL)
  )
);
CREATE INDEX stock_movements_product_idx  ON stock_movements (product_id);
CREATE INDEX stock_movements_ref_idx      ON stock_movements (ref_kind, ref_id);
CREATE INDEX stock_movements_occurred_idx ON stock_movements (occurred_at DESC);

-- ============================================================================
-- LEDGER 2: cuenta corriente de clientes
-- ============================================================================
-- Signo:  (+) saldo a favor del cliente (dejo dinero de mas)
--         (-) fiado: el cliente debe
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
-- sincronizacion
-- ============================================================================
-- xact_id es el cursor real. seq sobrevive solo como desempate determinista
-- DENTRO de una transaccion: las secuencias no son transaccionales y usarlas
-- como cursor pierde filas en silencio. Ver ADR-001.
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

-- Piso de retencion. Si el cursor de un cliente quedo por debajo, ese cliente
-- perdio historia y debe re-bootstrapear (CURSOR_TOO_OLD). Sin esta tabla, la
-- poda produce huecos silenciosos.
CREATE TABLE change_log_floor (
  singleton            BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  min_retained_xact_id XID8 NOT NULL DEFAULT '0'::xid8
);
INSERT INTO change_log_floor DEFAULT VALUES;

-- Registro de idempotencia. Se inserta en la MISMA transaccion que las filas
-- de dominio, por eso reenviar una operacion nunca duplica nada.
CREATE TABLE sync_operations (
  op_id         UUID PRIMARY KEY,                    -- UUIDv7 generado en el cliente
  device_id     TEXT NOT NULL,
  user_id       UUID NOT NULL REFERENCES users(id),
  op_type       TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('applied', 'rejected')),
  error_code    TEXT,
  error_message TEXT,
  request_hash  BYTEA NOT NULL,                      -- sha256 del payload
  applied_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sync_operations_device_idx ON sync_operations (device_id, applied_at DESC);

-- ============================================================================
-- vistas derivadas
-- ============================================================================
-- Vistas y no tablas materializadas: un SUM sobre 50k movimientos con indice
-- tarda <10ms. Umbral para reconsiderar: ~1M de filas (unos 20 años).
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
-- permisos: aca el append-only deja de ser disciplina
-- ============================================================================
GRANT USAGE ON SCHEMA public TO store_app;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO store_app;

-- change_log.seq es BIGSERIAL: sin USAGE sobre la secuencia, todo INSERT falla.
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO store_app;

-- Las tablas transaccionales quedan blindadas. Un UPDATE introducido por error
-- dentro de seis meses falla con permission denied en la primera prueba, en vez
-- de corromper la contabilidad en silencio.
REVOKE UPDATE, DELETE ON
  sales, sale_lines, sale_voids,
  stock_movements, customer_ledger, payments,
  product_prices, restocks, restock_lines,
  change_log, sync_operations
FROM store_app;

-- Solo el catalogo y las sesiones admiten UPDATE: metadata cosmetica por
-- last-write-wins, y last_seen_at de la sesion.
GRANT UPDATE ON products, customers, users, sessions TO store_app;

-- change_log_floor lo actualiza el podador, que corre con el rol migrador.
REVOKE UPDATE, DELETE ON change_log_floor FROM store_app;

-- El historial de migraciones es del rol migrador. GRANT ON ALL TABLES barre
-- tambien goose_db_version, porque goose la crea antes de aplicar la primera
-- migracion; sin este REVOKE, la aplicacion podria falsificar el historial.
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

-- Tablas creadas por migraciones futuras heredan el mismo trato por defecto.
-- OJO: toda migracion que agregue una tabla transaccional debe hacer su propio
-- REVOKE explicito. Esto solo cubre el GRANT base.
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

-- El rol es a nivel de cluster y puede estar en uso por otra base; no se borra.

-- +goose StatementEnd
