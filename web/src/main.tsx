import { StrictMode, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { registerSW } from 'virtual:pwa-register'

import { App } from './App'
import './styles/tokens.css'

function Root() {
  const [updateReady, setUpdateReady] = useState(false)

  useEffect(() => {
    // registerType is 'prompt', so a new version waits until the user accepts.
    // Auto-reloading is not an option: doing it mid-sale would wipe the screen
    // while a customer is waiting for change.
    const update = registerSW({
      onNeedRefresh() {
        setUpdateReady(true)
      },
    })
    // The update function is invoked by the banner's reload, via a full page
    // reload, which is simpler to reason about than a controlled skipWaiting.
    void update
  }, [])

  return <App onUpdateReady={updateReady} />
}

const container = document.getElementById('root')
if (!container) throw new Error('missing #root')

createRoot(container).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
