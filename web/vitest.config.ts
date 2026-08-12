import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
    // fake-indexeddb/auto installs a real, in-memory IndexedDB implementation.
    // The alternative would be mocking the database, which would make these
    // tests exercise the mock instead of the storage layer.
    setupFiles: ['./src/test-setup.ts'],
  },
})
