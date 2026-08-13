/**
 * UUIDv7 generation.
 *
 * v7 rather than v4 because the first 48 bits are a millisecond timestamp, so
 * ids sort by creation time. The outbox relies on that: iterating op_ids in
 * order is iterating operations in the order they happened on this device.
 *
 * crypto.randomUUID() only produces v4, which sorts randomly and would make the
 * queue replay operations out of order.
 *
 * The embedded timestamp is for ORDERING ONLY. A device with a wrong clock
 * still produces locally monotonic ids, which is all the outbox needs, but the
 * timestamp inside must never be read as business data — occurred_at is the
 * field that means "when the sale happened".
 */

/** Guards against a clock that jumps backwards mid-session. */
let lastMillis = 0
let sequence = 0

export function uuidv7(now: number = Date.now()): string {
  // A backwards jump (NTP correction, manual change) would otherwise emit ids
  // that sort before ones already queued, silently reordering the outbox.
  const millis = Math.max(now, lastMillis)
  sequence = millis === lastMillis ? sequence + 1 : 0
  lastMillis = millis

  const bytes = new Uint8Array(16)

  // 48 bits of big-endian milliseconds.
  const ms = BigInt(millis)
  for (let i = 0; i < 6; i++) {
    bytes[i] = Number((ms >> BigInt(8 * (5 - i))) & 0xffn)
  }

  crypto.getRandomValues(bytes.subarray(6))

  // Version 7 in the high nibble of byte 6, and 12 bits of sequence counter so
  // ids created within the same millisecond still sort in creation order.
  bytes[6] = 0x70 | ((sequence >> 8) & 0x0f)
  bytes[7] = sequence & 0xff

  // RFC 4122 variant bits.
  bytes[8] = (bytes[8]! & 0x3f) | 0x80

  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  return [
    hex.slice(0, 8),
    hex.slice(8, 12),
    hex.slice(12, 16),
    hex.slice(16, 20),
    hex.slice(20, 32),
  ].join('-')
}

/** Reads back the embedded timestamp. Diagnostics only, never business logic. */
export function uuidv7Timestamp(uuid: string): number {
  const hex = uuid.replace(/-/g, '').slice(0, 12)
  return Number.parseInt(hex, 16)
}
