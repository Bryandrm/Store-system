import { describe, expect, it } from 'vitest'

import { classify, type FailureKind } from './client'

/**
 * Failure classification is the most behaviour-defining function on the client.
 *
 * Getting a case wrong has asymmetric consequences:
 *  - calling something permanent when it is transient THROWS AWAY A REAL SALE
 *  - calling something transient when it is permanent burns battery forever
 *
 * The first is unacceptable, which is why anything unrecognized falls back to
 * transient.
 */
describe('classify', () => {
  const cases: { name: string; status: number; code: string; want: FailureKind }[] = [
    // The device may be holding sales nobody else has seen. Always retry.
    { name: 'no network at all', status: 0, code: 'NETWORK_ERROR', want: 'transient' },
    { name: 'server error', status: 500, code: 'INTERNAL_ERROR', want: 'transient' },
    { name: 'bad gateway', status: 502, code: 'HTTP_ERROR', want: 'transient' },
    { name: 'service unavailable', status: 503, code: 'HTTP_ERROR', want: 'transient' },
    { name: 'rate limited', status: 429, code: 'RATE_LIMITED', want: 'transient' },

    // Pause the loop, keep the outbox, keep selling.
    { name: 'no session', status: 401, code: 'UNAUTHENTICATED', want: 'auth' },
    { name: 'expired token', status: 401, code: 'TOKEN_EXPIRED', want: 'auth' },
    { name: 'revoked token', status: 401, code: 'TOKEN_REVOKED', want: 'auth' },
    { name: 'forbidden', status: 403, code: 'FORBIDDEN', want: 'auth' },

    // Rebuild the read model, keep the outbox.
    { name: 'cursor past retention', status: 409, code: 'CURSOR_TOO_OLD', want: 'stale_cursor' },

    // Will be rejected identically forever. Error tray.
    { name: 'total mismatch', status: 422, code: 'TOTAL_MISMATCH', want: 'permanent' },
    { name: 'validation failure', status: 422, code: 'VALIDATION_ERROR', want: 'permanent' },
    { name: 'unknown product', status: 422, code: 'UNKNOWN_PRODUCT', want: 'permanent' },
    { name: 'sale already voided', status: 409, code: 'SALE_ALREADY_VOIDED', want: 'permanent' },
    { name: 'malformed body', status: 400, code: 'INVALID_BODY', want: 'permanent' },
    { name: 'not found', status: 404, code: 'NOT_FOUND', want: 'permanent' },
  ]

  for (const c of cases) {
    it(`${c.name} (${c.status} ${c.code}) is ${c.want}`, () => {
      expect(classify(c.status, c.code)).toBe(c.want)
    })
  }

  /**
   * The safety default. A code this client version has never heard of must not
   * cost a sale, so the benefit of the doubt goes to retrying.
   */
  it('treats an unrecognized 5xx code as transient', () => {
    expect(classify(599, 'SOMETHING_NEW')).toBe('transient')
  })

  it('treats an unrecognized 4xx code as permanent', () => {
    // A 4xx is the server saying the request itself is wrong; repeating it
    // unchanged cannot help.
    expect(classify(418, 'SOMETHING_NEW')).toBe('permanent')
  })

  /**
   * Auth codes win over the status code. The server returns 401 for all three,
   * but the client needs them apart: an expired session prompts a login, while
   * a revoked one means somebody closed this device from elsewhere.
   */
  it('prefers the error code over the status for auth failures', () => {
    expect(classify(401, 'TOKEN_EXPIRED')).toBe('auth')
    expect(classify(401, 'TOKEN_REVOKED')).toBe('auth')
  })

  /**
   * CURSOR_TOO_OLD arrives as a 409, the same status as SALE_ALREADY_VOIDED,
   * but the two demand opposite reactions: rebuild everything versus discard
   * one operation. Classifying on status alone would conflate them.
   */
  it('separates the two 409 cases', () => {
    expect(classify(409, 'CURSOR_TOO_OLD')).toBe('stale_cursor')
    expect(classify(409, 'SALE_ALREADY_VOIDED')).toBe('permanent')
  })
})
