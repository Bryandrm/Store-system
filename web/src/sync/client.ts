/**
 * The typed API client.
 *
 * Its real job is not fetching, it is CLASSIFYING failures. The client behaves
 * completely differently depending on why a request failed, and getting that
 * wrong is how a real sale gets thrown away or how a phone burns its battery
 * retrying something that can never succeed.
 */
import type { Change, OperationResultStatus, Session } from '@/domain/types'

/** How the client should react to a failure. */
export type FailureKind =
  /** Retry forever. The device may hold sales nobody else has seen. */
  | 'transient'
  /** Pause syncing, keep the outbox, let the user go on selling. */
  | 'auth'
  /** Drop the read model and bootstrap again, keeping the outbox. */
  | 'stale_cursor'
  /** Never going to succeed. Send it to the error tray. */
  | 'permanent'

export class ApiError extends Error {
  readonly code: string
  readonly status: number
  readonly kind: FailureKind
  readonly requestId: string | null

  constructor(opts: {
    code: string
    status: number
    message: string
    kind: FailureKind
    requestId?: string | null
  }) {
    super(opts.message)
    this.name = 'ApiError'
    this.code = opts.code
    this.status = opts.status
    this.kind = opts.kind
    this.requestId = opts.requestId ?? null
  }
}

/**
 * Decides how to react to a failed response.
 *
 * Exported and tested on its own because it is the single most behaviour-
 * defining function on the client side.
 */
export function classify(status: number, code: string): FailureKind {
  // The network never answered, or the server is unreachable or overloaded.
  // A device that was offline for days still holds real sales, so this is
  // always worth retrying, without limit.
  if (status === 0 || status >= 500 || status === 429) return 'transient'

  switch (code) {
    // Auth failures pause the loop but touch nothing: selling keeps working
    // offline, and the queue is preserved until the user logs back in.
    case 'UNAUTHENTICATED':
    case 'TOKEN_EXPIRED':
    case 'TOKEN_REVOKED':
      return 'auth'

    // The device fell behind the retention window. Its read model is missing
    // history it can never receive incrementally, so it must start over.
    case 'CURSOR_TOO_OLD':
      return 'stale_cursor'
  }

  if (status === 401 || status === 403) return 'auth'

  // Any other 4xx is a rejection that will be rejected identically forever.
  if (status >= 400) return 'permanent'

  return 'transient'
}

interface Envelope<T> {
  ok: boolean
  statusCode: number
  message?: string
  data: T
  error?: string
  request_id?: string
}

export interface SyncOperationResult {
  op_id: string
  status: OperationResultStatus
  error_code?: string
  message?: string
}

export interface SyncResponse {
  results: SyncOperationResult[]
  changes: Change[]
  cursor: string
  has_more: boolean
  server_time: string
}

export interface BootstrapResponse {
  snapshot: Record<string, unknown[]>
  cursor: string
}

export interface OutgoingOperation {
  op_id: string
  type: string
  payload: unknown
}

export class ApiClient {
  constructor(
    private readonly baseUrl: string,
    private token: string | null = null,
  ) {}

  setToken(token: string | null): void {
    this.token = token
  }

  async login(username: string, password: string, deviceLabel: string): Promise<Session> {
    const data = await this.request<Omit<Session, 'token'> & { token: string }>(
      'POST',
      '/auth/login',
      { username, password, device_label: deviceLabel },
      { authenticated: false },
    )
    return data
  }

  async logout(): Promise<void> {
    await this.request<null>('POST', '/auth/logout', undefined)
  }

  async me(): Promise<Omit<Session, 'token'>> {
    return this.request<Omit<Session, 'token'>>('GET', '/auth/me', undefined)
  }

  async bootstrap(): Promise<BootstrapResponse> {
    return this.request<BootstrapResponse>('GET', '/bootstrap', undefined)
  }

  async sync(
    deviceId: string,
    cursor: string,
    operations: OutgoingOperation[],
  ): Promise<SyncResponse> {
    return this.request<SyncResponse>('POST', '/sync', {
      device_id: deviceId,
      cursor,
      operations,
    })
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
    opts: { authenticated?: boolean } = {},
  ): Promise<T> {
    const authenticated = opts.authenticated ?? true

    if (authenticated && !this.token) {
      throw new ApiError({
        code: 'UNAUTHENTICATED',
        status: 401,
        message: 'No hay sesion activa',
        kind: 'auth',
      })
    }

    const headers: Record<string, string> = {}
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (authenticated && this.token) headers['Authorization'] = `Bearer ${this.token}`

    let response: Response
    try {
      response = await fetch(`${this.baseUrl}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        // No cookies anywhere: auth is a bearer token held in JS, which means
        // there is no CSRF surface to defend.
        credentials: 'omit',
      })
    } catch (cause) {
      // fetch only rejects on a genuine network failure, which is precisely the
      // case that must be retried rather than surfaced as an error.
      throw new ApiError({
        code: 'NETWORK_ERROR',
        status: 0,
        message: cause instanceof Error ? cause.message : 'Sin conexion',
        kind: 'transient',
      })
    }

    let envelope: Envelope<T> | null = null
    try {
      envelope = (await response.json()) as Envelope<T>
    } catch {
      // A non-JSON body from a proxy or gateway. Trust the status code.
      envelope = null
    }

    if (!response.ok || !envelope?.ok) {
      const code = envelope?.error ?? 'HTTP_ERROR'
      throw new ApiError({
        code,
        status: response.status,
        message: envelope?.message ?? `Error ${response.status}`,
        kind: classify(response.status, code),
        requestId: envelope?.request_id ?? response.headers.get('X-Request-ID'),
      })
    }

    return envelope.data
  }
}
