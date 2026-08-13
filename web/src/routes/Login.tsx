import { useState, type FormEvent } from 'react'

import { Button } from '@/components/ui'
import { applySnapshot } from '@/db/apply'
import { setMeta } from '@/db/schema'
import type { Session } from '@/domain/types'
import { ApiError, type ApiClient } from '@/sync/client'
import { useApp } from '@/stores/app'

export function Login({ client, onLoggedIn }: { client: ApiClient; onLoggedIn: () => void }) {
  const setSession = useApp((s) => s.setSession)
  const refreshData = useApp((s) => s.refreshData)

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError(null)

    try {
      const deviceLabel =
        typeof navigator !== 'undefined' ? describeDevice(navigator.userAgent) : 'dispositivo'

      const result = await client.login(username.trim(), password, deviceLabel)
      const session: Session = { ...result }

      client.setToken(session.token)
      await setSession(session)

      // Pull the full replica before showing the sell screen. Without it the
      // product grid would be empty and the first sale impossible.
      const { snapshot, cursor } = await client.bootstrap()
      await applySnapshot(snapshot)
      await setMeta('cursor', cursor)
      await refreshData()

      onLoggedIn()
    } catch (cause) {
      // The server returns the same message for an unknown user, a wrong
      // password and a deactivated account, which is deliberate.
      setError(
        cause instanceof ApiError ? cause.message : 'No se pudo conectar. Revisa la conexión.',
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="mx-auto flex min-h-dvh max-w-sm flex-col justify-center gap-6 p-6">
      <header>
        <h1 className="text-3xl font-black tracking-tight uppercase">Tienda</h1>
        <p className="text-ink-soft mt-1 text-sm">Ventas, inventario y fiado</p>
      </header>

      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <label className="flex flex-col gap-1">
          <span className="text-xs font-bold tracking-wide uppercase">Usuario</span>
          <input
            className="border-ink min-h-tap border-2 bg-transparent px-3 text-lg"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            autoCapitalize="none"
            autoCorrect="off"
            required
          />
        </label>

        <label className="flex flex-col gap-1">
          <span className="text-xs font-bold tracking-wide uppercase">Contraseña</span>
          <input
            type="password"
            className="border-ink min-h-tap border-2 bg-transparent px-3 text-lg"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </label>

        {error && (
          <p role="alert" className="border-danger bg-danger-bg text-danger border-2 p-3 text-sm">
            {error}
          </p>
        )}

        <Button type="submit" variant="primary" disabled={busy}>
          {busy ? 'Entrando…' : 'Entrar'}
        </Button>
      </form>

      <p className="text-ink-soft text-xs">
        Se necesita conexión solo para entrar. Después la app funciona sin señal.
      </p>
    </main>
  )
}

/** A human-readable device label, so the sessions list is legible later. */
function describeDevice(userAgent: string): string {
  if (/iPhone/i.test(userAgent)) return 'iPhone'
  if (/iPad/i.test(userAgent)) return 'iPad'
  if (/Android/i.test(userAgent)) return 'Android'
  if (/Macintosh/i.test(userAgent)) return 'Mac'
  if (/Windows/i.test(userAgent)) return 'Windows'
  return 'Dispositivo'
}
