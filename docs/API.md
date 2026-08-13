# API

Base path `/api/v1`. All bodies are JSON. Auth is `Authorization: Bearer <token>`.

## The response contract

Exactly two shapes, produced in exactly one place — `internal/httpx/envelope.go`.
That file being the only thing able to build a body is not a style rule: when
any handler can assemble its own, drift is a matter of time, and the client ends
up with an adapter that guesses which variant arrived. That adapter never gets
deleted.

**Success**

```jsonc
{
  "ok": true,
  "statusCode": 200,
  "message": "Venta registrada",   // optional, human-facing, Spanish
  "data": { }
}
```

**Error**

```jsonc
{
  "ok": false,
  "statusCode": 422,
  "error": "TOTAL_MISMATCH",        // machine-readable: branch on THIS
  "message": "El total no coincide con el detalle de la venta",
  "details": [                       // optional, per-field
    { "field": "total_cents", "code": "TOTAL_MISMATCH" }
  ],
  "path": "/api/v1/sync",
  "timestamp": "2026-08-13T16:20:00Z",
  "request_id": "019ff78c-ea37-7abe-b78b-110d294c6836"
}
```

Clients branch on `error`, never on `message`. `message` is Spanish because it
is shown to the people running the store; everything else in the codebase is
English.

**5xx never leaks internals.** The client always gets the generic
`INTERNAL_ERROR` body plus a `request_id`, which the user can read off the
screen and quote. The real error goes to the log under that same id.

## Pagination

Cursor only. One shape, always — including when there is a single page.

```
?limit=50&cursor=<opaque>          limit default 50, maximum 200
```

```jsonc
{ "items": [ ], "next_cursor": "…" }
```

No `page`, no `pageSize`, no `offset`, no `skip`/`take`.

## Error codes

| Code | Status | Meaning |
|---|---|---|
| `VALIDATION_ERROR` | 422 | The payload is malformed or incoherent |
| `INVALID_BODY` | 400 | Not valid JSON, wrong content type, or too large |
| `UNAUTHENTICATED` | 401 | Missing or unknown token |
| `TOKEN_EXPIRED` | 401 | The session lapsed |
| `TOKEN_REVOKED` | 401 | Closed from another device, or the user was deactivated |
| `FORBIDDEN` | 403 | Authenticated, but not permitted |
| `NOT_FOUND` | 404 | |
| `CURSOR_TOO_OLD` | 409 | Fell behind retention; re-bootstrap required |
| `SALE_ALREADY_VOIDED` | 409 | |
| `UNKNOWN_PRODUCT` | 422 | |
| `INACTIVE_PRODUCT` | 422 | |
| `TOTAL_MISMATCH` | 422 | Client and server disagree by more than one cent |
| `RATE_LIMITED` | 429 | |
| `INTERNAL_ERROR` | 500 | |

The three 401 codes are deliberately distinct: the client pauses syncing on all
of them but tells the user different things, and only `UNAUTHENTICATED` means
"you never logged in".

## Endpoints

Eleven, and that is the point. Every device holds a full local replica, so
reports, sales lists and customer statements are computed on the device. A read
endpoint is added only when a report provably needs data the client does not
have.

```
POST   /api/v1/auth/login
POST   /api/v1/auth/logout
GET    /api/v1/auth/me
GET    /api/v1/bootstrap
POST   /api/v1/sync
GET    /api/v1/users            (owner)   ── not yet implemented
POST   /api/v1/users            (owner)   ── not yet implemented
PATCH  /api/v1/users/{id}       (owner)   ── not yet implemented
GET    /api/v1/sessions         (owner)   ── not yet implemented
DELETE /api/v1/sessions/{id}    (owner)   ── not yet implemented
GET    /healthz                 no /api/v1, no auth, no envelope
GET    /readyz                  no /api/v1, no auth, no envelope
```

Health checks sit outside the envelope on purpose: a load balancer should never
have to parse JSON to decide whether a process is alive.

---

### `POST /auth/login`

```jsonc
{ "username": "bryan", "password": "…", "device_label": "iPhone" }
```

```jsonc
{ "token": "…", "user_id": "…", "username": "bryan",
  "display_name": "Bryan", "role": "owner" }
```

The token is returned **once**. Only its SHA-256 is stored, so a database dump
hands over no live sessions.

Rate limited to 5 attempts/minute per IP and 10/hour per username. That is not
optional: password hashing allocates 19 MiB per call, so an unthrottled login
route is a one-line memory DoS against a 1 GB box.

An unknown username still pays for a full password verification, so response
time does not reveal which accounts exist. Unknown user, wrong password and
deactivated account all return the same message.

---

### `GET /bootstrap`

Returns a complete replica plus the cursor to start from. See
[SYNC.md](SYNC.md#bootstrap).

```jsonc
{
  "snapshot": {
    "products": [ ], "prices": [ ], "customers": [ ],
    "sales": [ ], "sale_lines": [ ], "stock_movements": [ ],
    "customer_ledger": [ ], "payments": [ ]
  },
  "cursor": "1218"
}
```

---

### `POST /sync`

The single endpoint every operational mutation goes through.

```jsonc
{
  "device_id": "…",
  "cursor": "1218",                 // "" or "0" means start from the beginning
  "operations": [
    {
      "op_id": "<uuidv7>",          // client-generated; makes replay safe
      "type": "create_sale",
      "payload": {
        "sale_id": "<uuidv7>",
        "customer_id": null,
        "total_cents": 175,
        "paid_cents": 175,
        "payment_method": "cash",   // cash | credit | mixed
        "occurred_at": "2026-08-13T16:20:00Z",
        "device_id": "…",
        "lines": [
          { "product_id": "…", "qty_milli": 2000,
            "unit_price_cents": 50, "line_total_cents": 100 }
        ]
      }
    }
  ]
}
```

```jsonc
{
  "results": [ { "op_id": "…", "status": "applied" } ],
  "changes": [ { "entity": "sale", "entity_id": "…",
                 "op": "insert", "payload": { } } ],
  "cursor": "1220",
  "has_more": false,
  "server_time": "2026-08-13T16:20:01Z"
}
```

At most 100 operations per request; at most 500 changes per response.

`status` is one of `applied`, `duplicate`, `rejected`, `retry`. A `duplicate` is
treated by the client exactly as `applied`.

`server_time` lets the client measure its own clock drift. Sync is clock-immune,
but `occurred_at` is not and every report groups by it.

#### Coherence rules enforced on a sale

- `cash` ⇒ `paid_cents == total_cents`
- `credit` ⇒ `paid_cents == 0`, and a `customer_id` is required
- `mixed` ⇒ `0 < paid_cents < total_cents`, and a `customer_id` is required
- Every line needs a positive quantity and a known product
- The server recomputes each line total and the sale total. A difference of one
  cent is tolerated **and the client's number is kept** — the figure quoted to
  the buyer is what actually happened. Anything larger is `TOTAL_MISMATCH`.
- An `occurred_at` more than five minutes in the future is clamped and flagged,
  never rejected.
