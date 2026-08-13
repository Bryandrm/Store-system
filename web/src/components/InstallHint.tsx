import { useEffect, useState } from 'react'

import { Button } from './ui'

const DISMISSED_KEY = 'install_hint_dismissed'

/**
 * Installation instructions for iOS.
 *
 * Android browsers offer an install prompt on their own. iOS has none: the user
 * has to go through Share → Add to Home Screen manually, and nothing tells them
 * so. Without this hint the app gets used as a Safari tab, which is the most
 * fragile mode there is — storage in a plain tab is evicted far more eagerly
 * than in an installed app.
 *
 * That matters here more than usual, because the owner's phone is an iPhone.
 */
export function InstallHint() {
  const [visible, setVisible] = useState(false)

  useEffect(() => {
    if (!isIOS() || isStandalone()) return
    if (localStorage.getItem(DISMISSED_KEY)) return
    setVisible(true)
  }, [])

  if (!visible) return null

  function dismiss() {
    localStorage.setItem(DISMISSED_KEY, '1')
    setVisible(false)
  }

  return (
    <aside className="border-ink bg-warn-bg border-b-2 p-3">
      <p className="text-sm font-bold">Instalá la app para que funcione sin señal</p>
      <ol className="text-ink-soft mt-1 list-inside list-decimal text-sm">
        <li>
          Tocá <strong>Compartir</strong> abajo en Safari
        </li>
        <li>
          Elegí <strong>Añadir a pantalla de inicio</strong>
        </li>
        <li>Abrí la app desde el ícono, no desde Safari</li>
      </ol>
      <p className="text-ink-soft mt-2 text-xs">
        Usada como pestaña de Safari, iOS puede borrar los datos guardados.
      </p>
      <Button variant="ghost" className="mt-1 min-h-0 px-0 py-1 text-xs" onClick={dismiss}>
        Entendido
      </Button>
    </aside>
  )
}

function isIOS(): boolean {
  if (typeof navigator === 'undefined') return false
  // iPadOS reports itself as a Mac, so the touch-point check is what catches it.
  return (
    /iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
  )
}

/** True when running from the home screen rather than inside the browser. */
function isStandalone(): boolean {
  if (typeof window === 'undefined') return false
  return (
    window.matchMedia('(display-mode: standalone)').matches ||
    // Non-standard, iOS only, and still the reliable signal there.
    (navigator as { standalone?: boolean }).standalone === true
  )
}
