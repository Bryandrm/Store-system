import { useEffect, useMemo, useRef, useState } from 'react'

import { InstallHint } from '@/components/InstallHint'
import { Banner, Button, StatusChip } from '@/components/ui'
import { getMeta } from '@/db/schema'
import type { Session } from '@/domain/types'
import { Login } from '@/routes/Login'
import { Sell } from '@/routes/Sell'
import { Today } from '@/routes/Today'
import { useApp } from '@/stores/app'
import { ApiClient } from '@/sync/client'
import { SyncEngine } from '@/sync/engine'

const API_BASE = import.meta.env['VITE_API_BASE'] ?? '/api/v1'

type Tab = 'sell' | 'today'

export function App({ onUpdateReady }: { onUpdateReady: boolean }) {
  const { hydrated, session, deviceId, syncStatus, hydrate, setSyncStatus, refreshData } = useApp()
  const [tab, setTab] = useState<Tab>('sell')

  const client = useMemo(() => new ApiClient(API_BASE), [])
  const engineRef = useRef<SyncEngine | null>(null)

  useEffect(() => {
    void hydrate()
  }, [hydrate])

  // The engine starts only once there is a session and a device id, and is torn
  // down on logout so a stopped session cannot keep polling in the background.
  useEffect(() => {
    if (!hydrated || !session || !deviceId) return

    client.setToken(session.token)
    const engine = new SyncEngine(client, deviceId)
    engineRef.current = engine

    const unsubscribe = engine.subscribe((status) => {
      setSyncStatus(status)
      // Changes that arrived from the feed have already landed in IndexedDB;
      // this pulls them into the view.
      void refreshData()
    })

    void engine.start()

    return () => {
      unsubscribe()
      engine.stop()
      engineRef.current = null
    }
  }, [hydrated, session, deviceId, client, setSyncStatus, refreshData])

  if (!hydrated) {
    return <p className="p-6 text-sm">Cargando…</p>
  }

  if (!session) {
    return <Login client={client} onLoggedIn={() => setTab('sell')} />
  }

  const status = syncStatus
  const clockSkewed = engineRef.current?.hasClockSkew() ?? false

  return (
    <div className="flex min-h-dvh flex-col">
      <InstallHint />

      {onUpdateReady && (
        <Banner
          action={
            <Button
              variant="secondary"
              className="min-h-0 px-2 py-1 text-xs"
              onClick={() => window.location.reload()}
            >
              Recargar
            </Button>
          }
        >
          Nueva versión disponible
        </Banner>
      )}

      {status?.state === 'session_expired' && (
        <Banner
          action={
            <Button
              variant="secondary"
              className="min-h-0 px-2 py-1 text-xs"
              onClick={() => void handleRelogin()}
            >
              Entrar
            </Button>
          }
        >
          La sesión venció. Podés seguir vendiendo; se sincroniza al volver a entrar.
        </Banner>
      )}

      {/* The one genuinely unrecoverable scenario on iOS is storage eviction
          while operations are still unsent. Everything acknowledged is already
          on the server, so this warning is the whole defence. */}
      {status?.stalePending && (
        <Banner tone="danger">
          Hay ventas sin sincronizar desde hace más de un día. Conectate a internet.
        </Banner>
      )}

      {clockSkewed && <Banner>La hora del teléfono está desfasada. Revisá la configuración.</Banner>}

      <header className="border-ink flex items-center justify-between gap-2 border-b-2 px-3 py-2">
        <h1 className="text-lg font-black tracking-tight uppercase">Tienda</h1>
        {status && (
          <StatusChip
            state={status.state}
            pendingCount={status.pendingCount}
            failedCount={status.failedCount}
            lastSyncAt={status.lastSyncAt}
          />
        )}
      </header>

      <main className="flex-1 overflow-hidden">
        {tab === 'sell' ? <Sell onSold={() => setTab('today')} /> : <Today />}
      </main>

      <nav className="border-ink grid grid-cols-2 gap-px border-t-2 bg-line">
        <Button
          variant={tab === 'sell' ? 'primary' : 'secondary'}
          className="border-0"
          onClick={() => setTab('sell')}
        >
          Vender
        </Button>
        <Button
          variant={tab === 'today' ? 'primary' : 'secondary'}
          className="border-0"
          onClick={() => setTab('today')}
        >
          Hoy
        </Button>
      </nav>
    </div>
  )

  async function handleRelogin(): Promise<void> {
    // Clearing the session sends the user back to the login screen. The outbox
    // is deliberately untouched: those operations are business data, and the
    // sales they represent really happened.
    const stored = await getMeta<Session>('session')
    if (stored) client.setToken(null)
    await useApp.getState().setSession(null)
  }
}
