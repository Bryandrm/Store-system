import { useEffect, useState } from 'react'

import { linesForSale } from '@/db/local'
import { formatCents, formatQty } from '@/domain/money'
import type { SaleLine } from '@/domain/types'
import { useApp } from '@/stores/app'

export function Today() {
  const { todaysSales, pendingSaleIds } = useApp()
  const [expanded, setExpanded] = useState<string | null>(null)

  const total = todaysSales.reduce((sum, sale) => sum + sale.total_cents, 0)

  return (
    <div className="flex min-h-dvh flex-col">
      <header className="border-ink flex items-end justify-between border-b-2 p-3">
        <div>
          <span className="text-ink-soft text-xs font-bold tracking-wide uppercase">
            Ventas de hoy
          </span>
          <p className="tnum text-4xl leading-none font-black">{formatCents(total)}</p>
        </div>
        <span className="tnum text-ink-soft text-sm">
          {todaysSales.length} {todaysSales.length === 1 ? 'venta' : 'ventas'}
        </span>
      </header>

      {todaysSales.length === 0 ? (
        <p className="text-ink-soft p-6 text-center text-sm">Todavía no hay ventas hoy.</p>
      ) : (
        <ul className="flex-1 overflow-y-auto">
          {todaysSales.map((sale) => (
            <SaleRow
              key={sale.id}
              saleId={sale.id}
              totalCents={sale.total_cents}
              occurredAt={sale.occurred_at}
              isCredit={sale.payment_method !== 'cash'}
              // The marker comes from the outbox, not from a flag on the sale,
              // so it cannot drift out of step with what is actually queued.
              pending={pendingSaleIds.has(sale.id)}
              expanded={expanded === sale.id}
              onToggle={() => setExpanded(expanded === sale.id ? null : sale.id)}
            />
          ))}
        </ul>
      )}
    </div>
  )
}

function SaleRow({
  saleId,
  totalCents,
  occurredAt,
  isCredit,
  pending,
  expanded,
  onToggle,
}: {
  saleId: string
  totalCents: number
  occurredAt: string
  isCredit: boolean
  pending: boolean
  expanded: boolean
  onToggle: () => void
}) {
  const [lines, setLines] = useState<SaleLine[] | null>(null)

  useEffect(() => {
    if (expanded && !lines) void linesForSale(saleId).then(setLines)
  }, [expanded, lines, saleId])

  return (
    <li className="border-line-soft border-b">
      <button onClick={onToggle} className="flex w-full items-center gap-3 px-3 py-3 text-left">
        <span className="tnum text-ink-soft text-sm">{formatTime(occurredAt)}</span>

        {/* A bracketed letter rather than a spinner: brutalist, and legible in
            direct sunlight where a subtle animation is invisible. */}
        {pending && (
          <span
            className="border-ink-soft text-ink-soft border px-1 text-xs font-bold"
            title="Todavía no se sincronizó"
          >
            P
          </span>
        )}
        {isCredit && (
          <span className="border-warn text-warn border px-1 text-xs font-bold">FIADO</span>
        )}

        <span className="tnum ml-auto text-lg font-bold">{formatCents(totalCents)}</span>
      </button>

      {expanded && (
        <ul className="bg-line-soft/20 px-3 pb-3">
          {lines?.map((line) => (
            <li key={line.id} className="text-ink-soft flex justify-between py-1 text-sm">
              <span>
                {line.product_name_snapshot}
                <span className="tnum ml-2">
                  {formatQty(line.qty_milli)} × {formatCents(line.unit_price_cents)}
                </span>
              </span>
              <span className="tnum">{formatCents(line.line_total_cents)}</span>
            </li>
          ))}
        </ul>
      )}
    </li>
  )
}

function formatTime(iso: string): string {
  return new Intl.DateTimeFormat('es-SV', {
    hour: '2-digit',
    minute: '2-digit',
    timeZone: 'America/El_Salvador',
  }).format(new Date(iso))
}
