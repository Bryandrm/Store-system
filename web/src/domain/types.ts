/**
 * The shapes that travel between the server and the local replica.
 *
 * These mirror the database columns rather than being a prettier client-side
 * model, on purpose: the sync feed ships rows, and every translation layer
 * between the wire and storage is a place where the two can silently disagree.
 */

/** Roles. Authorization is one column and one middleware, not a policy layer. */
export type Role = 'owner' | 'staff'

export type PaymentMethod = 'cash' | 'credit' | 'mixed'

/** Why stock moved. Mirrors the CHECK constraint on stock_movements.reason. */
export type StockReason = 'sale' | 'sale_void' | 'restock' | 'adjustment' | 'loss' | 'initial'

/** Why a customer's balance moved. */
export type LedgerKind = 'sale_credit' | 'payment' | 'adjustment' | 'sale_void'

export interface Product {
  id: string
  name: string
  category: string
  is_active: boolean
  sort_order: number
  created_at: string
  updated_at: string
}

/** The current price of a product, as resolved by the current_prices view. */
export interface ProductPrice {
  product_id: string
  price_cents: number
  /** Only the owner receives this; staff get null. */
  cost_cents: number | null
  effective_from: string
}

export interface Customer {
  id: string
  name: string
  phone: string | null
  notes: string | null
  is_active: boolean
}

export interface Sale {
  id: string
  customer_id: string | null
  total_cents: number
  paid_cents: number
  payment_method: PaymentMethod
  note: string | null
  /** Device clock: when the sale actually happened. Reports group by this. */
  occurred_at: string
  /** Server clock: when it landed. Never mix the two. */
  recorded_at?: string
  clock_skew_flagged?: boolean
  device_id: string
  created_by_user_id: string
  synced_by_user_id?: string
}

export interface SaleLine {
  id: string
  sale_id: string
  product_id: string
  qty_milli: number
  unit_price_cents: number
  line_total_cents: number
  /** Frozen at sale time, so renaming a product never rewrites history. */
  product_name_snapshot: string
  line_no: number
}

export interface StockMovement {
  id: string
  product_id: string
  delta_qty_milli: number
  reason: StockReason
  ref_kind: 'sale' | 'restock' | 'manual'
  ref_id: string | null
  occurred_at: string
}

export interface CustomerLedgerEntry {
  id: string
  customer_id: string
  /** Positive means the customer has credit; negative means they owe. */
  delta_cents: number
  kind: LedgerKind
  ref_kind: 'sale' | 'payment' | 'manual'
  ref_id: string | null
  occurred_at: string
}

export interface Payment {
  id: string
  customer_id: string
  amount_cents: number
  method: string
  occurred_at: string
}

export interface Session {
  token: string
  user_id: string
  username: string
  display_name: string
  role: Role
}

/** A line in the cart being rung up, before it becomes a sale. */
export interface CartLine {
  product_id: string
  product_name: string
  qty_milli: number
  unit_price_cents: number
}

/** Operation types the client may send. Mirrors internal/sync/apply.go. */
export type OperationType = 'create_sale'

/** The status the server reports back for each operation. */
export type OperationResultStatus = 'applied' | 'duplicate' | 'rejected' | 'retry'

/** The local lifecycle of an outbox entry. */
export type OutboxStatus = 'pending' | 'inflight' | 'synced' | 'failed'

export interface OutboxEntry {
  /** Client-generated UUIDv7. Sorting by it gives causal order on this device. */
  op_id: string
  type: OperationType
  payload: unknown
  occurred_at: string
  created_at: string
  status: OutboxStatus
  attempts: number
  next_attempt_at: string
  last_error_code: string | null
  last_error_message: string | null
  /** Local rows this operation created, so they can be marked as synced. */
  entity_ids: string[]
  /** When it was acknowledged, used by the 30-day prune. */
  synced_at: string | null
}

/** One row of the change feed. */
export interface Change {
  entity: string
  entity_id: string
  op: 'insert' | 'update'
  payload: unknown
}
