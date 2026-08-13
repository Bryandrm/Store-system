import { useState } from 'react'

import { Button, NumberPad, Total } from '@/components/ui'
import { formatCents, formatQty, lineTotal, saleTotal } from '@/domain/money'
import { recordSale } from '@/domain/recordSale'
import { QTY_SCALE } from '@/domain/money'
import { useApp, type SellableProduct } from '@/stores/app'

export function Sell({ onSold }: { onSold: () => void }) {
  const { products, stock, cart, session, deviceId, syncStatus } = useApp()
  const { addToCart, setLineQty, removeFromCart, clearCart, refreshData } = useApp()

  const [editing, setEditing] = useState<{ productId: string; draft: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const lineTotals = cart.map((l) => lineTotal(l.unit_price_cents, l.qty_milli))
  const total = saleTotal(lineTotals)

  async function handleCharge() {
    if (!session || cart.length === 0) return
    setBusy(true)
    setError(null)

    try {
      await recordSale({
        lines: cart,
        paymentMethod: 'cash',
        session,
        deviceId,
        clockOffsetMs: syncStatus?.clockOffsetMs ?? 0,
      })
      await clearCart()
      await refreshData()
      onSold()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'No se pudo registrar la venta')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-dvh flex-col">
      <section className="grid flex-1 auto-rows-min grid-cols-2 gap-px overflow-y-auto bg-line p-px">
        {products.map((product) => (
          <ProductTile
            key={product.id}
            product={product}
            stockMilli={stock.get(product.id) ?? 0}
            onTap={() => void addToCart(product)}
          />
        ))}
        {products.length === 0 && (
          <p className="text-ink-soft col-span-2 bg-paper p-6 text-center text-sm">
            No hay productos todavía.
          </p>
        )}
      </section>

      <section className="border-ink border-t-2">
        {cart.length > 0 && (
          <ul className="max-h-48 overflow-y-auto">
            {cart.map((line, index) => (
              <li
                key={line.product_id}
                className="border-line-soft flex items-center gap-2 border-b px-3 py-2"
              >
                <button
                  className="flex-1 text-left"
                  onClick={() => setEditing({ productId: line.product_id, draft: '' })}
                >
                  <span className="font-bold">{line.product_name}</span>
                  <span className="text-ink-soft tnum ml-2 text-sm">
                    {formatQty(line.qty_milli)} × {formatCents(line.unit_price_cents)}
                  </span>
                </button>
                <span className="tnum font-bold">{formatCents(lineTotals[index]!)}</span>
                <Button
                  variant="ghost"
                  className="min-h-0 px-2 py-1 text-lg"
                  aria-label={`Quitar ${line.product_name}`}
                  onClick={() => void removeFromCart(line.product_id)}
                >
                  ×
                </Button>
              </li>
            ))}
          </ul>
        )}

        <div className="flex items-end justify-between gap-4 p-3">
          <div>
            <span className="text-ink-soft text-xs font-bold tracking-wide uppercase">Total</span>
            <Total cents={total} />
          </div>
        </div>

        {error && (
          <p role="alert" className="border-danger bg-danger-bg text-danger border-t-2 p-3 text-sm">
            {error}
          </p>
        )}

        <div className="grid grid-cols-3 gap-px bg-line">
          <Button
            variant="secondary"
            className="border-0"
            disabled={cart.length === 0 || busy}
            onClick={() => void clearCart()}
          >
            Vaciar
          </Button>
          <Button
            variant="primary"
            className="col-span-2 border-0 text-xl"
            disabled={cart.length === 0 || busy}
            onClick={() => void handleCharge()}
          >
            {busy ? '…' : 'Cobrar'}
          </Button>
        </div>
      </section>

      {editing && (
        <QuantitySheet
          draft={editing.draft}
          onChange={(draft) => setEditing({ ...editing, draft })}
          onConfirm={() => {
            const units = Number.parseInt(editing.draft || '0', 10)
            void setLineQty(editing.productId, units * QTY_SCALE)
            setEditing(null)
          }}
          onCancel={() => setEditing(null)}
        />
      )}
    </div>
  )
}

function ProductTile({
  product,
  stockMilli,
  onTap,
}: {
  product: SellableProduct
  stockMilli: number
  onTap: () => void
}) {
  // Negative stock is shown, not hidden. It means the ledger says more was sold
  // than was ever stocked, which is real information the owner has to act on —
  // usually by doing a physical count.
  const negative = stockMilli < 0

  return (
    <button
      onClick={onTap}
      className="bg-paper fast flex min-h-24 flex-col justify-between p-3 text-left active:bg-ink active:text-paper"
    >
      <span className="text-base leading-tight font-bold">{product.name}</span>
      <span className="flex items-baseline justify-between gap-2">
        <span className="tnum text-lg font-black">{formatCents(product.price_cents)}</span>
        <span className={`tnum text-xs ${negative ? 'text-danger font-bold' : 'text-ink-soft'}`}>
          {negative ? `${formatQty(stockMilli)} ⚠` : formatQty(stockMilli)}
        </span>
      </span>
    </button>
  )
}

function QuantitySheet({
  draft,
  onChange,
  onConfirm,
  onCancel,
}: {
  draft: string
  onChange: (next: string) => void
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <div className="fixed inset-0 z-10 flex flex-col justify-end bg-ink/40" onClick={onCancel}>
      <div className="bg-paper border-ink border-t-2" onClick={(e) => e.stopPropagation()}>
        <p className="border-line-soft border-b p-3 text-center">
          <span className="text-ink-soft text-xs font-bold tracking-wide uppercase">Cantidad</span>
          <output className="tnum block text-4xl font-black">{draft || '0'}</output>
        </p>
        <NumberPad value={draft} onChange={onChange} onConfirm={onConfirm} onCancel={onCancel} />
      </div>
    </div>
  )
}
