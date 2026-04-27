# API Reference

Base URL: `http://localhost:8080/api/v1`

## Overview

### Authentication

State-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`) require a bearer token:

```
Authorization: Bearer <api-key>
```

Keys are configured via the `API_KEYS` env var (comma-separated). Comparison is constant-time. `GET`, `HEAD`, and `OPTIONS` are open. If `API_KEYS` is empty in non-`dev` `ENV`, the service still boots but logs a loud warning -- mutations go through unauthenticated; do not run that way in production.

### Content type

Request and response bodies are `application/json` (UTF-8). Field names are `snake_case`. Decimal amounts are transmitted as strings (e.g. `"500.00"`); never as JSON numbers (precision loss). Timestamps are RFC 3339 (e.g. `"2026-04-17T10:00:00Z"`); date-only fields use `YYYY-MM-DD`.

### Response envelopes

Success (200/201):

```json
{
  "code": 0,
  "message": "ok",
  "data": { /* resource or list */ }
}
```

Error (4xx/5xx):

```json
{
  "code": 14001,
  "message": "Insufficient balance for this operation"
}
```

`code` is an integer business code; the HTTP status is derived from it. Standard ranges:

| Code range | HTTP status | Meaning |
|------------|-------------|---------|
| `10000`-`10099` | 400 | Input validation failed |
| `10100`-`10149` | 401 | Authentication required / invalid key |
| `10150`-`10199` | 403 | Forbidden (channel mismatch, etc.) |
| `10200`-`10299` | 404 | Resource not found |
| `10300`-`10399` | 409 | Already exists |
| `10400`-`10499` | 429 | Rate limit exceeded |
| `10900`-`10999` | 409 | State conflict |
| `14000`-`14999` | 422 | Ledger / domain invariant violation |
| `18100`-`18199` | 503 | Service unavailable / starting |
| anything else | 500 | Internal error |

Common business codes you may see:

| Code | Meaning |
|------|---------|
| `10001` | Invalid input |
| `10101` | Missing or invalid API key |
| `10150` | Forbidden |
| `10201` | Not found |
| `10301` | Already exists |
| `10401` | Rate limit exceeded |
| `10901` | Conflict with current state |
| `14001` | Insufficient balance |
| `14002` | Duplicate journal (idempotency replay rejected) |
| `14003` | Journal entries not balanced |
| `14004` | Invalid state transition |
| `14005` | Reservation expired |
| `19999` | Internal error |

### Idempotency

All mutation endpoints accepting an `idempotency_key` enforce it via a database `UNIQUE` index. Replaying the **same key with the same payload** is rejected with HTTP `422` and code `14002` ("duplicate journal"); the original record is **not** re-returned. Callers must treat `14002` as success-equivalent for the operation that already ran. Use a stable, deterministic key (e.g. `deposit:user1001:0xabc...`).

### Pagination

List endpoints use opaque cursor pagination: `?cursor=<base64>&limit=50`. `limit` defaults to `50` and is capped at `200`. The response shape is:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "data": [ /* items */ ],
    "next_cursor": "AAAAAAAAAAI="
  }
}
```

When `next_cursor` is empty (or absent), there is no next page.

### Rate limits

In-memory per-IP token bucket (single-instance only):

- Mutations (`POST`/`PUT`/`PATCH`/`DELETE`): **100 req/min/IP**
- Reads (`GET`/`HEAD`): **1000 req/min/IP**

When exceeded, the response is `429` with code `10401` and `Retry-After: 60`. Behind a load balancer you must terminate `X-Forwarded-For` correctly or all traffic shares one bucket. For HA deployments replace with a Redis-backed limiter.

### Body size limit

Inbound bodies are capped via `http.MaxBytesReader`. Default `262144` (256 KB), configurable via `MAX_BODY_BYTES`. Webhook callbacks have an additional internal cap of 1 MB enforced inside the handler. Oversize requests are rejected with `400`.

### CORS

Configured via `CORS_ALLOWED_ORIGIN` (single explicit origin in production; `*` allowed only in `ENV=dev` without credentials). Preflight (`OPTIONS`) short-circuits before auth.

---

## 1. Bookings

A *booking* is an instance of a classification's lifecycle (replaces v1 `deposits` and `withdrawals`). State transitions emit events, which can post journals.

### POST /bookings

Create a new booking in its lifecycle's initial state.

Request:

```json
{
  "classification_code": "deposit",
  "account_holder": 1001,
  "currency_id": 1,
  "amount": "500.00",
  "idempotency_key": "deposit:user1001:0xabc",
  "channel_name": "evm",
  "metadata": {"chain": "ethereum", "address": "0x742d..."},
  "expires_at": "2026-04-17T11:00:00Z"
}
```

`expires_at` is optional (RFC 3339). `metadata` is a free-form JSON object.

Response `201 Created`:

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "id": 42,
    "classification_id": 1,
    "account_holder": 1001,
    "currency_id": 1,
    "amount": "500.00",
    "settled_amount": "0",
    "status": "pending",
    "channel_name": "evm",
    "channel_ref": "",
    "idempotency_key": "deposit:user1001:0xabc",
    "metadata": {"chain": "ethereum"},
    "expires_at": "2026-04-17T11:00:00Z",
    "created_at": "2026-04-17T10:00:00Z",
    "updated_at": "2026-04-17T10:00:00Z"
  }
}
```

Status codes: `201`, `400`, `401`, `404` (unknown classification), `422` (`14002` duplicate), `429`, `503`.

Auth: required. Idempotency: required.

### POST /bookings/{id}/transition

Advance a booking to a new lifecycle state. The classification's lifecycle declares the legal transitions. Returns the emitted event.

Request:

```json
{
  "to_status": "confirmed",
  "channel_ref": "0xabc123",
  "amount": "500.00",
  "metadata": {"confirmations": 12},
  "actor_id": 0
}
```

`amount` is optional; when set it overrides the booking's pending amount (for confirm-with-actual flows). `actor_id` of `0` denotes the system actor.

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 99,
    "classification_code": "deposit",
    "booking_id": 42,
    "account_holder": 1001,
    "currency_id": 1,
    "from_status": "confirming",
    "to_status": "confirmed",
    "amount": "500.00",
    "settled_amount": "500.00",
    "journal_id": 1234,
    "metadata": {"confirmations": 12},
    "occurred_at": "2026-04-17T10:05:00Z"
  }
}
```

Status codes: `200`, `400`, `401`, `404`, `422` (`14004` invalid transition; `14001` insufficient balance for journal), `429`, `503`.

Auth: required.

### GET /bookings/{id}

Fetch a booking by ID. Returns the same shape as `POST /bookings` data.

Status codes: `200`, `400`, `404`, `503`.

### GET /bookings

List bookings. Cursor-paginated.

Query params: `holder` (int64), `classification_id` (int64), `status` (string), `cursor` (base64), `limit` (1-200, default 50).

Response: paged envelope of booking objects.

---

## 2. Webhooks

### POST /webhooks/{channel}

Inbound channel callback. Body is delegated to the channel adapter for parsing; the adapter also verifies the signature.

For the built-in `evm` adapter, callers must send:

| Header | Value |
|--------|-------|
| `X-Timestamp` | Unix seconds at signing time |
| `X-Signature` | hex(`HMAC-SHA256(EVM_WEBHOOK_SECRET, "<X-Timestamp>.<body>")`) |

Timestamps outside ±5 minutes of server time are rejected. The signing key is configured via `EVM_WEBHOOK_SECRET`; if empty, the `evm` channel is not registered.

EVM payload:

```json
{
  "tx_hash": "0xabc123",
  "booking_id": 42,
  "amount": "500.00",
  "confirmations": 12,
  "status": "confirmed"
}
```

The handler verifies that `booking.channel_name` matches `{channel}` (defence against forged payloads pointing at a different booking) and applies the resulting transition. Responds with the emitted event (same shape as `POST /bookings/{id}/transition`).

Status codes: `200`, `400` (signature, parsing, replay window), `401` (channel auth fails), `403` (channel mismatch on booking), `404` (unknown channel or booking), `422` (transition rejected), `429`, `503`.

Body cap: 1 MB regardless of `MAX_BODY_BYTES`.

Auth: bearer token enforced by the platform middleware **in addition to** HMAC; tooling generating callbacks must include both.

---

## 3. Events

Events are append-only records of every booking state transition. They are the "reason" for any journal posting.

### GET /events/{id}

Fetch a single event.

Status codes: `200`, `400`, `404`, `503`.

### GET /events

List events. Cursor-paginated.

Query params: `classification_code` (string), `booking_id` (int64), `to_status` (string), `cursor`, `limit`.

Response: paged envelope of event objects.

---

## 4. Journals

### POST /journals

Post a manually balanced journal. All entries must net to zero per currency.

Request:

```json
{
  "journal_type_id": 1,
  "idempotency_key": "deposit:user1001:1",
  "entries": [
    {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "500.00"},
    {"account_holder": -1001, "currency_id": 1, "classification_id": 2, "entry_type": "credit", "amount": "500.00"}
  ],
  "source": "api",
  "actor_id": 0,
  "metadata": {"tx_hash": "0xabc"}
}
```

Response `201 Created`: journal object with `id`, `journal_type_id`, `idempotency_key`, `total_debit`, `total_credit`, `actor_id`, `source`, `metadata`, `created_at`. `entries` is omitted on the create response; use `GET /journals/{id}` to retrieve.

Status codes: `201`, `400`, `401`, `422` (`14002` duplicate, `14003` unbalanced, `14001` insufficient balance), `429`, `503`.

Auth: required. Idempotency: required.

### POST /journals/template

Post a journal by rendering a stored entry template.

Request:

```json
{
  "template_code": "deposit_confirm",
  "holder_id": 1001,
  "currency_id": 1,
  "idempotency_key": "deposit_journal:0xabc",
  "amounts": {"amount": "500.00"},
  "actor_id": 0,
  "source": "deposit_confirm",
  "metadata": {}
}
```

Response: same shape as `POST /journals`.

### POST /journals/deposit-tolerance

Apply the preset deposit-tolerance plan. Server picks one of: `confirm-as-expected`, `confirm-pending` + `release-shortfall`, or `manual review` based on the delta vs tolerance.

Request:

```json
{
  "holder_id": 1001,
  "currency_id": 1,
  "idempotency_key": "deposit_tol:0xabc",
  "expected_amount": "100.00",
  "actual_amount": "98.00",
  "tolerance": "5.00",
  "actor_id": 0,
  "source": "deposit_confirm"
}
```

Response `201 Created`:

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "outcome": "shortfall_auto_released",
    "expected_amount": "100.00",
    "actual_amount": "98.00",
    "tolerance": "5.00",
    "delta": "2.00",
    "requires_manual_review": false,
    "journals": [
      {"id": 10, "idempotency_key": "deposit_tol:0xabc:confirm-pending", "total_debit": "98.00", "total_credit": "98.00", "..." : "..."},
      {"id": 11, "idempotency_key": "deposit_tol:0xabc:release-shortfall", "total_debit": "2.00", "total_credit": "2.00", "...": "..."}
    ]
  }
}
```

### POST /journals/{id}/reverse

Create a reversal journal (debits and credits swapped). The `reason` is required and stored in metadata.

Request:

```json
{"reason": "duplicate deposit"}
```

Response `201 Created`: reversal journal with `reversal_of` set to the original ID.

Status codes: `201`, `400`, `401`, `404`, `422` (`14002` already reversed), `429`, `503`.

### GET /journals/{id}

Fetch a journal with its entries.

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 1,
    "journal_type_id": 1,
    "idempotency_key": "deposit:user1001:1",
    "total_debit": "500.00",
    "total_credit": "500.00",
    "actor_id": 0,
    "source": "api",
    "metadata": {},
    "created_at": "2026-04-17T10:00:00Z",
    "entries": [
      {"id": 1, "journal_id": 1, "account_holder": 1001, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "500.00", "created_at": "2026-04-17T10:00:00Z"}
    ]
  }
}
```

### GET /journals

Cursor-paginated list of journals. Query: `cursor`, `limit`.

### GET /entries

List entries by account. **Required** query params: `holder` (int64), `currency_id` (int64). Optional: `cursor`, `limit`. Cursor-paginated.

---

## 5. Balances

### GET /balances/{holder}

All classification balances for a holder in a single currency.

**Required** query param: `currency_id`.

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "balance": "404.50"}
  ]
}
```

### GET /balances/{holder}/{currency}

Same as above but with `currency_id` in the path; the response also includes a precomputed `total`:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "total": "404.50",
    "classifications": [
      {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "balance": "404.50"}
    ]
  }
}
```

### POST /balances/batch

Fetch balances for up to **100 holders** in one currency.

Request:

```json
{"holder_ids": [1001, 1002], "currency_id": 1}
```

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {"holder_id": 1001, "balances": [{"account_holder": 1001, "currency_id": 1, "classification_id": 1, "balance": "404.50"}]},
    {"holder_id": 1002, "balances": []}
  ]
}
```

Auth: required. Status codes: `200`, `400` (`> 100` holders, missing fields), `401`, `429`, `503`.

---

## 6. Reservations

### POST /reservations

Create a reservation (cross-classification fund lock). Idempotent.

Request:

```json
{
  "account_holder": 1001,
  "currency_id": 1,
  "amount": "100.00",
  "idempotency_key": "spend:user1001:order42",
  "expires_in_sec": 900
}
```

Response `201 Created`:

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "id": 1,
    "account_holder": 1001,
    "currency_id": 1,
    "reserved_amount": "100.00",
    "status": "active",
    "idempotency_key": "spend:user1001:order42",
    "expires_at": "2026-04-17T10:15:00Z",
    "created_at": "2026-04-17T10:00:00Z",
    "updated_at": "2026-04-17T10:00:00Z"
  }
}
```

Status codes: `201`, `400`, `401`, `422` (`14001` insufficient balance, `14002` duplicate), `429`, `503`.

### POST /reservations/{id}/settle

Settle a reservation with the actual amount. Posts the settlement journal and releases any remainder.

Request: `{"actual_amount": "95.50"}`. Response `200 OK`: `{"status": "settled"}`.

Status codes: `200`, `400`, `401`, `404`, `422` (`14004` invalid state, `14005` expired), `429`, `503`.

### POST /reservations/{id}/release

Release an active or settling reservation without settlement.

Response `200 OK`: `{"status": "released"}`.

### GET /reservations

List reservations.

Query: `holder` (int64), `status` (string), `limit` (default 50, max 200). No cursor pagination on this endpoint -- limit only.

---

## 7. Metadata

### Classifications

#### POST /classifications

```json
{
  "code": "main_wallet",
  "name": "Main Wallet",
  "normal_side": "debit",
  "is_system": false,
  "lifecycle": null
}
```

`lifecycle` is optional; `null` means the classification is label-only (cannot create bookings against it). When present, must be a valid finite-state machine: see `core/types.go#Lifecycle`.

Response `201 Created`: classification object.

#### POST /classifications/{id}/deactivate

Soft-disables the classification. Response `200 OK`: `{"status": "deactivated"}`.

#### GET /classifications

Query: `active_only=true|false`. Returns array of classifications.

### Journal Types

#### POST /journal-types

`{"code": "deposit", "name": "Deposit Confirmation"}` -> 201.

#### POST /journal-types/{id}/deactivate

#### GET /journal-types

`?active_only=true`.

### Templates

#### POST /templates

```json
{
  "code": "deposit_confirm",
  "name": "Confirm Deposit",
  "journal_type_id": 1,
  "lines": [
    {"classification_id": 1, "entry_type": "debit", "holder_role": "user", "amount_key": "amount", "sort_order": 1},
    {"classification_id": 2, "entry_type": "credit", "holder_role": "system", "amount_key": "amount", "sort_order": 2}
  ]
}
```

`holder_role` is `user` or `system`. `amount_key` references the matching key in the `amounts` map at execution time.

Response `201 Created`: template object including its lines.

#### POST /templates/{id}/deactivate

#### POST /templates/{code}/preview

Render a template without persisting. Body:

```json
{"holder_id": 1001, "currency_id": 1, "amounts": {"amount": "500.00"}}
```

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "entries": [
      {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "500.00"},
      {"account_holder": -1001, "currency_id": 1, "classification_id": 2, "entry_type": "credit", "amount": "500.00"}
    ]
  }
}
```

#### GET /templates

`?active_only=true`.

### Currencies

#### POST /currencies

`{"code": "USDT", "name": "Tether USD"}` -> 201.

#### GET /currencies

Returns the full list (no pagination).

---

## 8. Reconciliation

### POST /reconcile

Global reconciliation: verifies the accounting equation across all journals.

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "balanced": true,
    "gap": "0",
    "details": [],
    "checked_at": "2026-04-17T12:00:00Z"
  }
}
```

### POST /reconcile/account

Per-account reconciliation: verifies checkpoint balance matches `SUM(entries since checkpoint)` for each `(holder, currency, classification)` tuple.

Request: `{"holder": 1001, "currency_id": 1}`.

Response: same shape as global reconciliation, with `details[]` populated when drift is detected:

```json
{
  "balanced": false,
  "gap": "0.01",
  "details": [
    {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "expected": "500.00", "actual": "500.01", "drift": "0.01"}
  ],
  "checked_at": "2026-04-17T12:00:00Z"
}
```

---

## 9. Snapshots

### GET /snapshots

Historical daily balance snapshots.

**Required** query params: `holder`, `currency_id`, `start` (`YYYY-MM-DD`), `end` (`YYYY-MM-DD`). Returns array of snapshot rows.

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "snapshot_date": "2026-04-16", "balance": "404.50"}
  ]
}
```

---

## 10. System

### GET /system/health

DB-backed health check. Returns 200 with metrics on success, 503 on DB failure.

Response `200 OK`:

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "status": "ok",
    "db": "up",
    "rollup_queue_depth": 0,
    "checkpoint_max_age_seconds": 12,
    "active_reservations": 3
  }
}
```

### GET /system/ready

Kubernetes-style readiness probe. Returns 200 once migrations + worker have booted; 503 with `{"status": "starting"}` otherwise.

### GET /system/balances

Aggregate system-wide balances by `(currency_id, classification_id)`.

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {"currency_id": 1, "classification_id": 1, "total_balance": "50000.00", "updated_at": "2026-04-17T12:00:00Z"}
  ]
}
```

---

## OpenAPI

A machine-readable spec covering the high-traffic endpoints lives at [`openapi.yaml`](../openapi.yaml). It is the source of truth for SDK generation; this Markdown is human-readable narrative.
