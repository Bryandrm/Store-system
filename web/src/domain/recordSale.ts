/**
 * Recording a sale locally.
 *
 * This is the optimistic write: the sale must appear instantly, long before any
 * server has heard about it, because the customer is standing there waiting for
 * their change.
 *
 * It mirrors what internal/sales/sales.go does on the server — the sale, its
 * lines, and one stock movement per line — so the local view and the eventual
 * server view agree without a reconciliation step.
 */
import { applyChanges } from '@/db/apply'
import { enqueue } from '@/db/outbox'
import type { CartLine, Change, PaymentMethod, Session } from './types'
import { lineTotal, saleTotal } from './money'
import { uuidv7 } from './uuid'

export interface RecordSaleInput {
  lines: CartLine[]
  paymentMethod: PaymentMethod
  customerId?: string | null
  note?: string
  session: Session
  deviceId: string
  /** Difference between this device's clock and the server's, in ms. */
  clockOffsetMs?: number
}

export interface RecordedSale {
  saleId: string
  opId: string
  totalCents: number
}

export async function recordSale(input: RecordSaleInput): Promise<RecordedSale> {
  if (input.lines.length === 0) {
    throw new Error('No se puede cobrar una venta sin productos')
  }

  const saleId = uuidv7()
  const opId = uuidv7()

  // Correct for known clock drift before stamping. Sync itself is immune to a
  // bad clock, but occurred_at is not, and every report groups by it.
  const occurredAt = new Date(Date.now() + (input.clockOffsetMs ?? 0)).toISOString()

  const lines = input.lines.map((line, index) => ({
    id: uuidv7(),
    sale_id: saleId,
    product_id: line.product_id,
    qty_milli: line.qty_milli,
    unit_price_cents: line.unit_price_cents,
    line_total_cents: lineTotal(line.unit_price_cents, line.qty_milli),
    product_name_snapshot: line.product_name,
    line_no: index + 1,
  }))

  const totalCents = saleTotal(lines.map((l) => l.line_total_cents))

  // Cash is paid in full; credit is entirely owed. Mixed is not offered from
  // this screen yet, so it is rejected rather than guessed at.
  const paidCents = input.paymentMethod === 'cash' ? totalCents : 0
  if (input.paymentMethod === 'mixed') {
    throw new Error('El pago mixto todavia no esta disponible')
  }
  if (input.paymentMethod === 'credit' && !input.customerId) {
    throw new Error('Un fiado necesita un cliente')
  }

  const sale = {
    id: saleId,
    customer_id: input.customerId ?? null,
    total_cents: totalCents,
    paid_cents: paidCents,
    payment_method: input.paymentMethod,
    note: input.note ?? null,
    occurred_at: occurredAt,
    device_id: input.deviceId,
    created_by_user_id: input.session.user_id,
  }

  // Stock is derived, so selling is a negative entry in the ledger. Writing it
  // here as well as on the server is what makes the local stock figure correct
  // the instant the sale is rung up.
  const movements = lines.map((line) => ({
    id: uuidv7(),
    product_id: line.product_id,
    delta_qty_milli: -line.qty_milli,
    reason: 'sale' as const,
    ref_kind: 'sale' as const,
    ref_id: saleId,
    occurred_at: occurredAt,
  }))

  const changes: Change[] = [
    { entity: 'sale', entity_id: saleId, op: 'insert', payload: sale },
    ...lines.map((l) => ({
      entity: 'sale_line',
      entity_id: l.id,
      op: 'insert' as const,
      payload: l,
    })),
    ...movements.map((m) => ({
      entity: 'stock_movement',
      entity_id: m.id,
      op: 'insert' as const,
      payload: m,
    })),
  ]

  // Order matters: the rows land first, then the operation is queued. If the
  // process died in between, the sale would be visible locally and unsent,
  // which the pending marker makes obvious. The reverse order could queue an
  // operation for rows that were never written.
  await applyChanges(changes)

  await enqueue({
    op_id: opId,
    type: 'create_sale',
    payload: {
      sale_id: saleId,
      customer_id: sale.customer_id,
      total_cents: totalCents,
      paid_cents: paidCents,
      payment_method: sale.payment_method,
      note: sale.note,
      occurred_at: occurredAt,
      device_id: input.deviceId,
      created_by_user_id: input.session.user_id,
      lines: lines.map((l) => ({
        product_id: l.product_id,
        qty_milli: l.qty_milli,
        unit_price_cents: l.unit_price_cents,
        line_total_cents: l.line_total_cents,
      })),
    },
    occurred_at: occurredAt,
    entity_ids: [saleId, ...lines.map((l) => l.id), ...movements.map((m) => m.id)],
  })

  return { saleId, opId, totalCents }
}
