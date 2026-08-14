import { execFileSync } from 'node:child_process'
import { expect, type Page } from '@playwright/test'

/**
 * Test helpers.
 *
 * The database is seeded through psql, never through the UI. Seeding through
 * the UI means one login bug breaks every spec at once, and the failure points
 * at the wrong place.
 */

const MIGRATION_URL =
  'postgres://store_migrator:dev_only_no_usar_en_produccion@localhost:5433/store_system?sslmode=disable'

export const OWNER = { username: 'e2e', password: 'clave-de-prueba-e2e' }

/** Runs SQL as the migrator role and returns stdout. */
export function sql(statement: string): string {
  return execFileSync('psql', [MIGRATION_URL, '-tAc', statement], {
    encoding: 'utf8',
  }).trim()
}

/**
 * Empties the transactional tables and reinstates a known catalog.
 *
 * TRUNCATE rather than DELETE because it also resets the sequence behind
 * change_log, which keeps cursor values small and readable when a spec fails.
 *
 * The catalog is fixed here, not read from cmd/seed, so a change to the real
 * catalog cannot silently change what the specs assert.
 */
export function resetDatabase(): void {
  sql(`
    TRUNCATE sale_voids, sale_lines, sales, stock_movements, customer_ledger,
             payments, restock_lines, restocks, customers,
             product_prices, products, sync_operations, change_log
    RESTART IDENTITY CASCADE;
    DELETE FROM sessions;
    DELETE FROM users;
  `)

  // argon2id hash of OWNER.password, generated once by cmd/seed. Hashing here
  // would make every spec pay 19 MiB and ~100ms for no benefit.
  const hash = ownerHash()

  sql(`
    INSERT INTO users (id, username, display_name, password_hash, role)
    VALUES ('00000000-0000-7000-8000-000000000001', '${OWNER.username}', 'E2E', '${hash}', 'owner');

    INSERT INTO products (id, name, sort_order) VALUES
      ('00000000-0000-7000-8000-0000000000a1', 'Mani japones', 0),
      ('00000000-0000-7000-8000-0000000000a2', 'Semillas', 1),
      ('00000000-0000-7000-8000-0000000000a3', 'Churros', 2);

    INSERT INTO product_prices (id, product_id, price_cents, effective_from, created_by_user_id)
    VALUES
      (gen_random_uuid(), '00000000-0000-7000-8000-0000000000a1', 50, now() - interval '1 day', '00000000-0000-7000-8000-000000000001'),
      (gen_random_uuid(), '00000000-0000-7000-8000-0000000000a2', 25, now() - interval '1 day', '00000000-0000-7000-8000-000000000001'),
      (gen_random_uuid(), '00000000-0000-7000-8000-0000000000a3', 100, now() - interval '1 day', '00000000-0000-7000-8000-000000000001');

    -- Opening stock, so the sell screen does not start negative.
    INSERT INTO stock_movements
      (id, product_id, delta_qty_milli, reason, ref_kind, ref_id, occurred_at, created_by_user_id)
    SELECT gen_random_uuid(), p.id, 100000, 'initial', 'manual', NULL, now(),
           '00000000-0000-7000-8000-000000000001'
    FROM products p;
  `)
}

let cachedHash: string | null = null

/** Generates the owner's password hash once per run, via cmd/seed. */
function ownerHash(): string {
  if (cachedHash) return cachedHash

  // cmd/seed is idempotent and never overwrites an existing user, so it is
  // asked to create a throwaway account purely to obtain a real hash. Doing it
  // this way means the specs cannot drift from the production hashing
  // parameters.
  execFileSync('go', ['run', './cmd/seed', '-username', '__hashsrc__',
    '-password', OWNER.password, '-skip-catalog'], {
    cwd: '..',
    env: { ...process.env, DATABASE_URL: MIGRATION_URL },
    encoding: 'utf8',
  })
  cachedHash = sql(`SELECT password_hash FROM users WHERE username = '__hashsrc__'`)
  sql(`DELETE FROM users WHERE username = '__hashsrc__'`)
  return cachedHash
}

export function countSales(): number {
  return Number.parseInt(sql('SELECT count(*) FROM sales'), 10)
}

export function totalSoldCents(): number {
  return Number.parseInt(sql('SELECT COALESCE(SUM(total_cents), 0) FROM sales'), 10)
}

export function stockFor(productName: string): number {
  return Number.parseInt(
    sql(`SELECT COALESCE(SUM(m.delta_qty_milli), 0)
         FROM stock_movements m JOIN products p ON p.id = m.product_id
         WHERE p.name = '${productName}'`),
    10,
  )
}

/** Logs in through the UI and waits for the sell screen. */
export async function login(page: Page): Promise<void> {
  await page.goto('/')
  await page.getByLabel('Usuario').fill(OWNER.username)
  await page.getByLabel('Contraseña').fill(OWNER.password)
  await page.getByRole('button', { name: 'Entrar' }).click()
  await expect(page.getByRole('button', { name: 'Cobrar' })).toBeVisible()
}

/**
 * Waits until the service worker has taken control.
 *
 * Without this, a reload issued too early goes to the network and the offline
 * assertions pass for the wrong reason.
 */
export async function waitForServiceWorker(page: Page): Promise<void> {
  await page.waitForFunction(() => navigator.serviceWorker?.controller !== null, null, {
    timeout: 15_000,
  })
}

/** Taps a product tile in the grid. */
export async function tapProduct(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: new RegExp(name, 'i') }).first().click()
}
