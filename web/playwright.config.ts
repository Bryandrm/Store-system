import { defineConfig, devices } from '@playwright/test'

/**
 * End-to-end configuration.
 *
 * These specs run against the built app served by `vite preview`, not the dev
 * server, because the service worker only exists in a build — and the service
 * worker is precisely what the most important spec is testing.
 *
 * Consequence: the dev server's /api proxy is not available here, so the app is
 * built pointing at the API's absolute URL and the API is told to allow this
 * origin. That is closer to production anyway, where the PWA and the API live
 * on different hosts by design.
 */

const API_ORIGIN = 'http://localhost:8080'
const APP_ORIGIN = 'http://localhost:4173'

const DATABASE_URL = 'postgres://store_app:dev_app@localhost:5433/store_system?sslmode=disable'
const MIGRATION_URL =
  'postgres://store_migrator:dev_only_no_usar_en_produccion@localhost:5433/store_system?sslmode=disable'

export default defineConfig({
  testDir: './e2e',
  // Specs share one database, so they cannot run at the same time. Fixing this
  // with a database per worker is possible but not worth it for seven specs.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env['CI'],
  retries: process.env['CI'] ? 1 : 0,
  reporter: process.env['CI'] ? [['github'], ['html', { open: 'never' }]] : [['list']],
  // These specs register a service worker, go offline, and wait for real sync
  // cycles. They are slow by nature, and a timeout tight enough to be "fast"
  // just produces flakes that get blamed on the app.
  timeout: 90_000,

  use: {
    baseURL: APP_ORIGIN,
    // Without this the service worker never registers and every offline
    // assertion silently tests nothing.
    serviceWorkers: 'allow',
    trace: 'retain-on-failure',
    video: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      // The app is used on a phone, so it is tested on one. Playwright only
      // supports service workers in Chromium, so WebKit — and therefore the
      // owner's iPhone — stays a manual checklist. See docs/TESTING.md.
      use: { ...devices['Pixel 7'] },
    },
  ],

  webServer: [
    {
      command: 'go run ./cmd/storeapi',
      cwd: '..',
      url: `${API_ORIGIN}/healthz`,
      reuseExistingServer: !process.env['CI'],
      stdout: 'pipe',
      stderr: 'pipe',
      env: {
        DATABASE_URL,
        MIGRATION_DATABASE_URL: MIGRATION_URL,
        ALLOWED_ORIGINS: APP_ORIGIN,
        ADDR: ':8080',
        // The suite logs in more often in a minute than a human does in a
        // week, so the shipped limits would block it. The defaults stay strict
        // and are covered by internal/auth/ratelimit_test.go; only this
        // environment overrides them.
        LOGIN_RATE_PER_IP_PER_MINUTE: '1000',
        LOGIN_RATE_PER_USER_PER_HOUR: '1000',
      },
    },
    {
      command: 'pnpm build && pnpm preview --port 4173 --strictPort',
      url: APP_ORIGIN,
      // Never reused, even locally. Reusing skips the build, so a stale bundle
      // gets tested while the source says otherwise — which is indistinguishable
      // from an application bug and costs an hour to spot.
      reuseExistingServer: false,
      stdout: 'pipe',
      stderr: 'pipe',
      timeout: 120_000,
      env: {
        VITE_API_BASE: `${API_ORIGIN}/api/v1`,
      },
    },
  ],
})
