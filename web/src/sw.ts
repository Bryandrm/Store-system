/// <reference lib="webworker" />

/**
 * The service worker, written by hand.
 *
 * vite-plugin-pwa runs in injectManifest mode: it only supplies the precache
 * list below. None of Workbox's runtime caching strategies are used, because
 * all data lives in IndexedDB — the only thing worth caching here is the app
 * shell itself.
 */
import { precacheAndRoute } from 'workbox-precaching'

declare const self: ServiceWorkerGlobalScope

// Injected at build time with every hashed asset.
precacheAndRoute(self.__WB_MANIFEST)

self.addEventListener('install', () => {
  // Deliberately NOT calling skipWaiting(). A new version waits until the user
  // accepts it from the banner: swapping the running app mid-sale would clear
  // the screen while a customer waits for their change.
})

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim())
})

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url)

  // API requests are NEVER intercepted. Letting them fail is the correct
  // behaviour: the sync engine owns retries, and a service worker that served a
  // stale cached response here would make the app show sales that no longer
  // exist, or hide ones that do.
  if (url.pathname.startsWith('/api/')) return

  // Navigations fall back to the cached shell when offline, which is what makes
  // opening the app with no signal work at all.
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetch(event.request).catch(async () => {
        const cached = await caches.match('/index.html')
        return cached ?? Response.error()
      }),
    )
  }
})

/** Lets the page trigger activation once the user accepts the update. */
self.addEventListener('message', (event) => {
  if ((event.data as { type?: string })?.type === 'SKIP_WAITING') {
    void self.skipWaiting()
  }
})
