/// <reference types="vite/client" />
/// <reference types="vite-plugin-pwa/client" />
/// <reference types="vite-plugin-pwa/info" />

/**
 * Declared explicitly so the API base can be read with dot access.
 *
 * Vite replaces `import.meta.env.VITE_X` statically at build time; bracket
 * access is not replaced, so it silently falls through to the default and the
 * built app talks to the wrong origin. That failure is invisible until an
 * end-to-end run notices the requests never arrive.
 */
interface ImportMetaEnv {
  /** Absolute API base. Unset in development, where Vite proxies /api. */
  readonly VITE_API_BASE?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
