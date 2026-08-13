/**
 * Hand-rolled UI primitives.
 *
 * No component library, on purpose. These are five components used one-handed
 * outdoors; a library would bring a design language that fights the brutalist
 * one and ship far more code than this.
 */
import type { ButtonHTMLAttributes, ReactNode } from 'react'

import { formatCents } from '@/domain/money'
import type { SyncState } from '@/sync/engine'

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  children: ReactNode
}

const VARIANTS: Record<ButtonVariant, string> = {
  primary: 'bg-ink text-paper border-ink',
  secondary: 'bg-paper text-ink border-ink',
  ghost: 'bg-transparent text-ink-soft border-transparent',
  danger: 'bg-danger-bg text-danger border-danger',
}

export function Button({ variant = 'secondary', className = '', ...props }: ButtonProps) {
  return (
    <button
      // min-h-tap is 56px: the smallest target reliably hit with a thumb while
      // holding cash in the other hand.
      className={`min-h-tap fast border-2 px-4 text-base font-bold tracking-wide uppercase
        active:translate-y-px disabled:opacity-40 ${VARIANTS[variant]} ${className}`}
      {...props}
    />
  )
}

/**
 * The sync status chip.
 *
 * Five states and no more. A status display with a dozen states is one nobody
 * reads, and the only question this needs to answer at a glance is "are my
 * sales safe?".
 */
export function StatusChip({
  state,
  pendingCount,
  failedCount,
  lastSyncAt,
}: {
  state: SyncState
  pendingCount: number
  failedCount: number
  lastSyncAt: string | null
}) {
  if (failedCount > 0) {
    return (
      <span className="border-danger bg-danger-bg text-danger border-2 px-2 py-1 text-xs font-bold">
        ⚠ {failedCount} ERROR{failedCount > 1 ? 'ES' : ''}
      </span>
    )
  }

  const label = (() => {
    switch (state) {
      case 'session_expired':
        return 'SESIÓN VENCIDA'
      case 'offline':
        return pendingCount > 0 ? `SIN CONEXIÓN (${pendingCount})` : 'SIN CONEXIÓN'
      case 'syncing':
        return 'SINCRONIZANDO…'
      case 'error':
        return 'ERROR'
      default:
        return pendingCount > 0
          ? `PENDIENTE (${pendingCount})`
          : lastSyncAt
            ? `SINCRONIZADO ${formatTime(lastSyncAt)}`
            : 'SINCRONIZADO'
    }
  })()

  const tone =
    state === 'session_expired' || state === 'error'
      ? 'border-warn bg-warn-bg text-warn'
      : pendingCount > 0 || state === 'offline'
        ? 'border-ink-soft text-ink-soft'
        : 'border-line-soft text-ink-soft'

  return <span className={`border-2 px-2 py-1 text-xs font-bold ${tone}`}>{label}</span>
}

function formatTime(iso: string): string {
  return new Intl.DateTimeFormat('es-SV', {
    hour: '2-digit',
    minute: '2-digit',
    timeZone: 'America/El_Salvador',
  }).format(new Date(iso))
}

/**
 * A numeric keypad, rather than <input type="number">.
 *
 * The native control on a phone opens a keyboard that covers half the screen,
 * accepts a decimal point that has no meaning for whole bags, and puts tiny
 * spinner arrows next to each other. This is bigger, faster and unambiguous.
 */
export function NumberPad({
  value,
  onChange,
  onConfirm,
  onCancel,
}: {
  value: string
  onChange: (next: string) => void
  onConfirm: () => void
  onCancel: () => void
}) {
  const press = (key: string) => {
    if (key === '⌫') return onChange(value.slice(0, -1))
    // Leading zeros would let "007" through, which reads as a bug to the user.
    if (value === '0') return onChange(key)
    if (value.length >= 4) return
    onChange(value + key)
  }

  return (
    <div className="grid grid-cols-3 gap-px bg-line">
      {['1', '2', '3', '4', '5', '6', '7', '8', '9', '⌫', '0'].map((key) => (
        <Button
          key={key}
          variant="secondary"
          className="border-0 text-2xl"
          onClick={() => press(key)}
          aria-label={key === '⌫' ? 'Borrar' : key}
        >
          {key}
        </Button>
      ))}
      <Button variant="primary" className="border-0" onClick={onConfirm}>
        OK
      </Button>
      <Button variant="ghost" className="col-span-3 border-0" onClick={onCancel}>
        Cancelar
      </Button>
    </div>
  )
}

/** The running total. Deliberately huge: it is read at arm's length. */
export function Total({ cents }: { cents: number }) {
  return (
    <output className="tnum block text-right text-5xl leading-none font-black">
      {formatCents(cents)}
    </output>
  )
}

/** A full-width banner for things the user must notice but can keep working through. */
export function Banner({
  tone = 'warn',
  children,
  action,
}: {
  tone?: 'warn' | 'danger'
  children: ReactNode
  action?: ReactNode
}) {
  const classes =
    tone === 'danger'
      ? 'border-danger bg-danger-bg text-danger'
      : 'border-warn bg-warn-bg text-warn'

  return (
    <div className={`flex items-center justify-between gap-3 border-b-2 px-3 py-2 ${classes}`}>
      <p className="text-sm font-bold">{children}</p>
      {action}
    </div>
  )
}
