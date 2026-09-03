# API Reference

Base URL: `http://localhost:8080/api/v1`

## Overview

### Authentication

Every endpoint — reads included — requires a bearer token (holder is a
guessable int64; an open read surface would expose every holder's balances
and history):

```
Authorization: Bearer <api-key>
```

Exceptions: the Kubernetes probes (`/system/health`, `/system/ready`) answer
without credentials, and inbound webhooks (`/webhooks/{channel}`)
authenticate via the channel adapter's own signature scheme (e.g. HMAC).

Keys are configured via the `API_KEYS` env var as comma-separated
`name:scope:secret` triples:

```
API_KEYS="ops:admin:s3cr3t,app:write:t0k3n,report:read:r34d"
```

The `name` identifies the caller in access logs (audit); the `scope` is one
of three ordered levels (each implies the ones below it):

| Scope | Grants |
|-------|--------|
| `read` | The query surface: balances, journals, entries, events, audit, platform analytics, metadata listings, batch balance lookup, template preview |
| `write` | `read` + business writes: posting journals, reversals, reservations, bookings |
| `admin` | Everything: configuration mutations (classifications, journal types, templates, currencies), account policies, reconciliation triggers, period close |

A request whose key lacks the required scope gets business code `10150`
(HTTP 403) naming the key and the missing scope. Secret comparison is
constant-time. If `API_KEYS` is empty in non-`dev` `ENV`, the service still
refuses to start. Unauthenticated operation is available only with `ENV=dev`.

A key may also carry one or more independent **capabilities** — privilege
bits no scope implies, not even `admin` — by joining them onto the scope
field with `+`:

```
API_KEYS="ingester:write:t0k3n,reviewer:read+deposit_review:r3v13w"
```

Today there is one capability, `deposit_review`
(`server.CapabilityDepositReview`), required by
`POST /deposits/{uid}/review/approve` and `/reject` regardless of scope
(W3-A, mi2: `docs/bugs/2026-07-11-m3-security-review.md`). It exists so the
key that ingests/creates deposit bookings does not automatically get to
approve its own review — grant it on a separate reviewer key, or on the
same key alongside `write`/`admin` if a deployment deliberately chooses
that policy (see `docs/RUNBOOK.md` §10).

### Content type

Request and response bodies are `application/json` (UTF-8). Field names are `snake_case`. Decimal amounts are transmitted as strings (e.g. `"500.00"`); never as JSON numbers (precision loss). Timestamps are RFC 3339 (e.g. `"2026-04-17T10:00:00Z"`); date-only fields use `YYYY-MM-DD`.

### Response envelopes

Success (200/201):

```json
{
  "code": 200,
  "message": null,
  "data": { /* resource or list */ }
}
```

Error (4xx/5xx):

```json
{
  "code": 14001,
  "message": { "text": "Insufficient balance for this operation" },
  "data": null
}
```

`code` is an integer business code; the HTTP status is derived from it. Clients determine retry behavior from the code table below and must reuse the same idempotency key.

**`message.fields`** (optional) carries per-field validation messages when the rejection is attributable to named request fields. Keys are the request body's own snake_case field names, values are text written for a person to read — map them onto form inputs:

```json
{
  "code": 10001,
  "message": {
    "text": "Please check your input and try again",
    "fields": { "amount": "is not a valid decimal amount" }
  },
  "data": null
}
```

`message.text` is the per-code display string and is always present; `fields` is **absent** (not an empty object) when no single field is at fault. Field messages never contain driver, storage or upstream error text — those stay in the server's logs, correlated by request id.

### Error handling contract

| Code range | HTTP status | Meaning | Retryable |
|------------|-------------|---------|-----------|
| `10000`-`10099` | 400 | Input validation failed | No |
| `10100`-`10149` | 401 | Authentication required / invalid key | No |
| `10150`-`10199` | 403 | Forbidden (channel mismatch, etc.) | No |
| `10200`-`10299` | 404 | Resource not found | No |
| `10300`-`10399` | 409 | Already exists | No |
| `10400`-`10499` | 429 | Rate limit exceeded | **Yes** |
| `10900`-`10999` | 409 | State conflict | No |
| `14000`-`14999` | 422 | Ledger / domain invariant violation | No |
| `18100`-`18199` | 503 | Service unavailable / starting | **Yes** |
| anything else | 500 | Internal error | **Yes** (default) |

Common business codes you may see:

| Code | Meaning | Retryable |
|------|---------|-----------|
| `10001` | Invalid input | No |
| `10101` | Missing or invalid API key | No |
| `10150` | Forbidden | No |
| `10201` | Not found | No |
| `10301` | Already exists | No |
| `10401` | Rate limit exceeded | Yes |
| `10901` | Conflict with current state | No |
| `14001` | Insufficient balance | No |
| `14002` | Duplicate journal (legacy low-level uniqueness error) | No |
| `14003` | Journal entries not balanced | No |
| `14004` | Invalid state transition | No |
| `14005` | Reservation expired | No |
| `14009` | Accounting period is closed | No |
| `14010` | Journal failed tamper-detection authorization check | No |
| `14011` | Authorization key not recognized (rotated out, or from another deployment) | No |
| `18103` | Rollup queue item pending for this dimension | Yes |
| `18104` | Authorization signer temporarily unavailable | Yes |
| `18105` | Temporary failure (adapter-classified transient error) | Yes |
| `19999` | Internal error | Yes |

**Retry semantics.** Retryability is derived from the business code, not from an additional wire field:

- `429` (rate limited) and `503` (service unavailable / starting) are transient by nature — back off (honor `Retry-After` when present) and retry.
- `400`/`401`/`403`/`404`/`409` and `422` describe either a defect in the request or a business-rule outcome — replaying the identical payload reproduces the identical result, so these are **not** retryable without changing the request.
- `500` and any code outside the known ranges default to retryable: an unclassified failure is assumed to be a transient dependency hiccup (DB blip, network reset) rather than a permanent defect.

**Retrying is only safe with the same `idempotency_key`.** For mutating endpoints, a retry must reuse the exact `idempotency_key` from the original attempt — that is what turns a retry into a no-op replay instead of a duplicate side effect (see "Idempotency" below). Retrying a `429`/`503`/`500` with a *new* idempotency key on a request that actually landed can create a duplicate booking/journal.

### Idempotency

**Money-path mutations** (journals, template execution, reversals,
reservations, bookings and their transitions, dev credits) enforce
key-based idempotency. Client-initiated writes generate one random UUID in
the `Idempotency-Key` header per logical submission and reuse it across
retries. System events derive a deterministic key from the source event.
Replaying the **same key with the same payload** returns the original result;
reusing it with a different payload returns HTTP `409` / code `10901`.

Two exceptions, both explicit in `openapi.yaml` (`x-idempotency`) rather than
left to be discovered:

- **`POST /journals/{uid}/reverse`** derives its key server-side
  (`reversal:{uid}:{reason}`) and **rejects** a caller-supplied one with
  `400`. Use `POST /journals/{uid}/reverse-partial` with `num` = `den` = 1
  to choose the key yourself.
- **Metadata mutations** (create/deactivate for classifications, journal
  types, templates, currencies) are **not key-based**. They read no
  `idempotency_key` and no `Idempotency-Key` header. A repeated create is
  deduplicated by the `code` UNIQUE constraint — `409` / `10901`, never a
  second row — and a repeated deactivate is idempotent by state. Sending a
  key has no effect; note that a retry therefore answers `409` rather than
  replaying the original `201`, so treat "already exists" as success when
  retrying a create.

### Pagination

List endpoints use opaque cursor pagination: `?cursor=<base64>&limit=50`. `limit` defaults to `50` and is capped at `200`. The response shape is:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "list": [ /* items */ ],
    "next_cursor": "AAAAAAAAAAI="
  }
}
```

When `next_cursor` is null, there is no next page.

**Direction** is per endpoint and stated in each one's summary in
[`openapi.yaml`](openapi.yaml). The ledger reads — `GET /journals`,
`GET /entries`, `GET /audit/journals`, `GET /reservations`,
`GET /holder/transactions` — page **newest first**: the first page is the most
recent rows and the cursor walks strictly older. The queue-shaped reads —
`GET /bookings`, `GET /events`, `GET /deposits/reviews` — page **oldest
first**, because they are worked through in occurrence order.

`GET /holder/holds` is cursor-paginated as well (since H-m9); it used to
return the holder's entire hold set in one response.

### Rate limits

In-memory per-IP token bucket (single-instance only):

- Mutations (`POST`/`PUT`/`PATCH`/`DELETE`): **100 req/min/IP**
- Reads (`GET`/`HEAD`): **1000 req/min/IP**

When exceeded, the response is `429` with code `10401` and `Retry-After: 60`. Behind a load balancer, set `TRUSTED_PROXY_CIDRS` to your proxy ranges so the real client IP is used for keying (see the RUNBOOK "Deployment security boundary" section); otherwise all proxied traffic shares one bucket. For HA deployments replace with a Redis-backed limiter.

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
  "currency_uid": "cur-usdt",
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
  "code": 200,
  "message": null,
  "data": {
    "uid": "bkg-1",
    "classification_uid": "cls-deposit",
    "account_holder": 1001,
    "currency_uid": "cur-usdt",
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

`reservation_uid` and `journal_uid` are omitted (not empty-string) until a reservation or a settlement journal is linked.

Status codes: `201`, `400`, `401`, `404` (unknown classification), `422` (`14002` duplicate), `429`, `503`.

Auth: required. Idempotency: required.

### POST /bookings/{uid}/transition

Advance a booking to a new lifecycle state. The classification's lifecycle declares the legal transitions. Returns the emitted event.

Request:

```json
{
  "to_status": "confirmed",
  "channel_ref": "0xabc123",
  "amount": "500.00",
  "metadata": {"confirmations": 12},
  "actor_id": 0,
  "idempotency_key": "confirm-0xabc123-0"
}
```

`idempotency_key` is **required** (a request without it is `400` / `10001`); reuse the same key on retry — see [Idempotency](#idempotency). `amount` is optional; when set it overrides the booking's pending amount (for confirm-with-actual flows). `actor_id` of `0` denotes the system actor.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "uid": "evt-99",
    "classification_code": "deposit",
    "booking_uid": "bkg-1",
    "account_holder": 1001,
    "currency_uid": "cur-usdt",
    "from_status": "confirming",
    "to_status": "confirmed",
    "amount": "500.00",
    "settled_amount": "500.00",
    "journal_uid": "jnl-1234",
    "metadata": {"confirmations": 12},
    "occurred_at": "2026-04-17T10:05:00Z"
  }
}
```

Status codes: `200`, `400`, `401`, `404`, `422` (`14004` invalid transition; `14001` insufficient balance for journal), `429`, `503`.

Auth: required.

### GET /bookings/{uid}

Fetch a booking by uid. Returns the same shape as `POST /bookings` data.

Status codes: `200`, `400`, `404`, `503`.

### GET /bookings

List bookings. Cursor-paginated.

Query params: `holder` (int64), `classification_uid` (string, uuid), `status` (string), `cursor` (base64), `limit` (1-200, default 50).

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
  "booking_uid": "bkg-1",
  "amount": "500.00",
  "confirmations": 12,
  "status": "confirmed"
}
```

This is the legacy `ParseCallback` shape (transitions an existing booking by
`booking_uid`). The `evm` adapter's current, preferred ingestion path is
`ParseSighting` (`channel/onchain/evm.go`), a reorg-safe
`{chain_id, tx_hash, txlog_seq, token, from, to, amount, block_number,
confirmations}` payload that the server routes to whenever the resolved
channel adapter implements it **and** the host wired a deposit ingester
(`srv.SetDepositIngester(svc.Onchain())`) -- without that call every callback
on a sighting-capable channel answers `503` / `18102` "This feature is not
enabled on this server" (the 2026-09-03 consumer review measured exactly
that; README "Quick Start -- Serving the HTTP API" has the call) (I-20; see
`docs/plans/2026-07-11-crypto-deposit-sweep-design.md` §3 and
[COOKBOOK Recipe 9](COOKBOOK.md#recipe-9--crypto-deposit--sweep-create2-shared-address-custody)).

The handler verifies that `booking.channel_name` matches `{channel}` (defence against forged payloads pointing at a different booking) and applies the resulting transition. Responds with the emitted event (same shape as `POST /bookings/{uid}/transition`).

Three separate `403` refusals on this path, all structural rather than
configurable — a valid signature is necessary and never sufficient:

- **channel mismatch** — the booking's `channel_name` is not `{channel}`. The
  channel→booking mapping in the database is trusted, never the `booking_uid`
  in the payload.
- **not a deposit booking** — the booking's classification is not the
  `deposit` classification. A callback may only advance the deposit lifecycle
  it exists to serve; most importantly it can never transition a `sweep`
  booking, which posts no journal, so a forged `confirmed` there would leave
  no accounting trace.
- **`status: "confirmed"` is refused outright.** Confirming a deposit posts
  accounting, and [INVARIANTS I-21](INVARIANTS.md) holds that a confirmed
  deposit's journal comes from exactly one place — the service-layer
  orchestration that writes the journal in the same transaction as the
  transition. This path does not: it hands `status` straight to a booking
  transition and posts nothing, so a `confirmed` here produces a deposit the
  holder-facing surfaces call settled with no entries behind it and no
  journal for reconciliation to miss. To ingest a confirmation, implement
  `channel.SightingParser` on your adapter (the shape below); the server
  routes a sighting-capable adapter to the deposit-ingestion path
  automatically, and the built-in `evm` adapter already takes it. Any other
  lifecycle status is accepted normally.

**Replay.** The signature window (±5 min) rejects stale replays, but an
identical request replayed *inside* the window still verifies. Rejecting that
needs a nonce cache, and the cache is host-wired, not automatic:

```go
srv.SetWebhookNonceRecorder(svc.WebhookNonceRecorder())
```

`svc.WebhookNonceRecorder()` hands over the `postgres.WebhookSubscriberStore`
that implements it. **Until that call is made there is no in-window replay
protection and the `409` below cannot occur** — the library does not install
one for you, because an inbound channel that is not exposed does not need one.
If you register any channel, make the call.

Status codes: `200`, `400` (signature, parsing, replay window), `401` (channel auth fails), `403` (the three refusals above), `404` (unknown channel or booking), `409` / `10901` (in-window replay — only once a nonce recorder is wired), `422` (transition rejected), `429`, `503` (also, unconditionally, for a sighting-capable channel when `SetDepositIngester` was never called — `18102`).

Body cap: 1 MB regardless of `MAX_BODY_BYTES`.

Auth: bearer token enforced by the platform middleware **in addition to** HMAC; tooling generating callbacks must include both.

### Outbound event delivery (webhook push)

The other direction: when a consumer registers a webhook subscriber, the delivery worker POSTs each event to that URL. This is the ledger's main outbound contract and its schema is `OutboundEvent` in [`openapi.yaml`](openapi.yaml) — deliberately a **different** schema from `Event`, which is the REST surface's shape (`GET /events`). Two Go types are involved: the pushed body is the domain event's own json shape.

The body is the bare event object, **not** the `{code, message, data}` envelope (`api-contract.md` §1 exempts pushed payloads). Every field rule still applies: snake_case names, amounts as strings, timestamps as RFC3339 **UTC** (`...Z`).

```json
{
  "uid": "evt-01J...",
  "classification_code": "deposit",
  "booking_uid": "bkg-01J...",
  "account_holder": 1001,
  "currency_uid": "cur-01J...",
  "from_status": "pending",
  "to_status": "confirmed",
  "amount": "500.00",
  "settled_amount": "0",
  "journal_uid": "jnl-01J...",
  "metadata": { "tx_hash": "0xabc" },
  "occurred_at": "2026-09-02T04:00:00Z",
  "actor_id": 0,
  "source": "worker"
}
```

| Header | Value |
|--------|-------|
| `X-Ledger-Event-UID` | The event uid. Delivery is **at-least-once** — deduplicate on this. |
| `X-Ledger-Timestamp` | Unix seconds at send time; covered by the signature. |
| `X-Ledger-Signature` | `t=<timestamp>,v1=<hex>` where `<hex>` is `HMAC-SHA256(subscriber_secret, "<timestamp>.<raw body>")` |

Verify the signature over the **raw** body bytes, before any re-serialization. A subscriber registered without a secret is never delivered to (delivery fails and is retried) rather than delivered to unsigned.

Receivers must tolerate unknown fields (`api-contract.md` §8): this payload mirrors a domain type, so a field added there appears here. `journal_uid` is absent until a journal is linked to the event.

---

## 3. Events

Events are append-only records of every booking state transition. They are the "reason" for any journal posting.

### GET /events/{uid}

Fetch a single event.

Status codes: `200`, `400`, `404`, `503`.

### GET /events

List events. Cursor-paginated.

Query params: `classification_code` (string), `booking_uid` (string, uuid), `to_status` (string), `cursor`, `limit`.

Response: paged envelope of event objects.

---

## 4. Journals

### POST /journals

Post a manually balanced journal. All entries must net to zero per currency.

**This endpoint refuses any entry touching an `is_system` classification**
(custodial, suspense, equity, fees, …) by default (403) — the
handwritten-path counterpart to `POST /journals/template`'s protected-code
guard just below, so a leaked `write`-scope key cannot mint deposit-shaped
accounting through either endpoint (`docs/INVARIANTS.md` I-38). A deployment
that legitimately hand-posts system-side journals over HTTP sets
`Config.AllowSystemClassificationPost` (logs a startup warning).

So the example below deliberately moves money between two non-system,
per-holder classifications (`main_wallet` and `locked` — an earlier revision
of this document used `main_wallet` debit / `custodial` credit, which the
endpoint now answers with 403) rather than modeling a deposit confirmation,
which real deployments post through `deposit_confirm`'s template, not this
endpoint.

Request:

```json
{
  "journal_type_uid": "jt-lock_funds",
  "idempotency_key": "lock:user1001:1",
  "entries": [
    {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "entry_type": "credit", "amount": "500.00"},
    {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-locked", "entry_type": "debit", "amount": "500.00"}
  ],
  "source": "api",
  "actor_id": 0,
  "effective_at": "2026-06-01T00:00:00Z",
  "metadata": {"reason": "manual-hold"}
}
```

`effective_at` is optional — omit it to attribute the journal to now. Set it
to backdate a retroactive posting; rejected with `10001` if more than 5
minutes in the future, and with `14009` if it falls before the active period
close line (see [§14 Periods](#14-periods)).

Response `201 Created`: journal object with `uid`, `journal_type_uid`, `idempotency_key`, `total_debit`, `total_credit`, `actor_id`, `source`, `metadata`, `effective_at`, `created_at`. `entries` is omitted on the create response; use `GET /journals/{uid}` to retrieve.

Status codes: `201`, `400`, `401`, `403` (an entry touches an `is_system` classification and `AllowSystemClassificationPost` is off), `422` (`14002` duplicate, `14003` unbalanced, `14001` insufficient balance, `14009` period closed), `429`, `503`.

Auth: required. Idempotency: required.

### POST /journals/template

Post a journal by rendering a stored entry template.

**This endpoint refuses any template with a leg on an `is_system`
classification** (403, structurally derived from the template's own lines --
not a name list), plus the codes in `PROTECTED_TEMPLATE_CODES`. That covers
every deposit/capital/settlement/fee/fx template the presets install: those
postings are what a deployment's own verified orchestration performs, and a
write-scope key must not be able to mint them by naming a code. The single
opt-out is `ALLOW_GENERIC_TEMPLATE_POST` (per code, after a reviewed
decision).

So the example below deliberately uses a holder-to-holder template, not
`deposit_confirm` -- which an earlier revision of this document showed, and
which the endpoint now answers with 403.

Request:

```json
{
  "template_code": "transfer_out",
  "holder_id": 1001,
  "currency_uid": "cur-usdt",
  "idempotency_key": "transfer:req-7781",
  "amounts": {"amount": "500.00"},
  "actor_id": 0,
  "source": "api",
  "metadata": {}
}
```

`idempotency_key` is required and flat (top level). The
`Idempotency-Key` header is an accepted alias for it.

Response: same shape as `POST /journals`.

Status codes: `201`, `400`, `401`, `403` (protected template code, or a
template with an `is_system` leg), `422`, `429`, `503`.

### POST /journals/deposit-tolerance

Apply the preset deposit-tolerance plan. Server picks one of: `confirm-as-expected`, `confirm-pending` + `release-shortfall`, or `manual review` based on the delta vs tolerance.

⚠️ **Admin scope, and 403 under the default configuration.** This endpoint
takes no `template_code`, but it turns caller-supplied expected/actual
amounts into executions of `deposit_confirm_pending` / `deposit_confirm` /
`deposit_release_pending` / `deposit_record_overage` -- the same
deposit-shaped accounting `POST /journals/template` refuses by name. A
write-scope key could therefore mint through here what it could not mint next
door (contract §7.11), so:

- every template the plan would execute passes the same gate as
  `POST /journals/template`, which means the default answer is **403**;
- the route requires the **admin** scope, not write (the same group as
  `POST /dev/credits`).

A deployment that genuinely resolves deposit tolerance over HTTP opts the
four codes in via `ALLOW_GENERIC_TEMPLATE_POST` and calls this with an admin
key. The alternative -- and the path the library itself takes -- is to run
`presets.BuildDepositTolerancePlan` + `ExecuteDepositTolerancePlan` in the
consumer's own Go orchestration, where no HTTP-reachable key is involved at
all.

Request:

```json
{
  "holder_id": 1001,
  "currency_uid": "cur-usdt",
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
  "code": 200,
  "message": null,
  "data": {
    "outcome": "shortfall_auto_released",
    "expected_amount": "100.00",
    "actual_amount": "98.00",
    "tolerance": "5.00",
    "delta": "2.00",
    "requires_manual_review": false,
    "journals": [
      {"uid": "jnl-10", "idempotency_key": "deposit_tol:0xabc:confirm-pending", "total_debit": "98.00", "total_credit": "98.00", "..." : "..."},
      {"uid": "jnl-11", "idempotency_key": "deposit_tol:0xabc:release-shortfall", "total_debit": "2.00", "total_credit": "2.00", "...": "..."}
    ]
  }
}
```

### POST /journals/{uid}/reverse

Create a full reversal journal (debits and credits swapped). Rejected `409`
once the journal has any reversal history, full or partial — use
`reverse-partial` to continue a partially-reversed journal. The `reason` is
required and stored in metadata.

Request:

```json
{"reason": "duplicate deposit"}
```

Response `201 Created`: reversal journal with `reversal_of_uid` set to the original journal's uid.

Status codes: `201`, `400`, `401`, `404`, `409` (already reversed), `422`, `429`, `503`.

### POST /journals/{uid}/reverse-partial

Reverse `num`/`den` of a journal's entries, per-currency balanced via
largest-remainder allocation. Cumulative reversals never exceed the original
(`409` on overshoot). `num == den` (e.g. `"1/1"`) reverses exactly the
remaining un-reversed amount — the way to complete a reversal whose earlier
fractional steps rounded up.

Request:

```json
{"num": 1, "den": 2, "reason": "partial refund", "idempotency_key": "reverse:jnl-1:half"}
```

Response `201 Created`: partial reversal journal, same shape as a full reversal.

Status codes: `201`, `400`, `401`, `404`, `409` (cumulative reversal would exceed the original), `422`, `429`, `503`.

### GET /journals/{uid}

Fetch a journal with its entries.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "uid": "jnl-1",
    "journal_type_uid": "jt-deposit_confirm",
    "idempotency_key": "deposit:user1001:1",
    "total_debit": "500.00",
    "total_credit": "500.00",
    "actor_id": 0,
    "source": "api",
    "metadata": {},
    "created_at": "2026-04-17T10:00:00Z",
    "entries": [
      {"journal_uid": "jnl-1", "account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "entry_type": "debit", "amount": "500.00", "effective_at": "2026-04-17T10:00:00Z", "created_at": "2026-04-17T10:00:00Z"}
    ]
  }
}
```

Entries do not carry their own `uid` in this response — trace them by `journal_uid` + position, or via `GET /entries` (which is keyed by account dimension, not journal).

### GET /journals

Cursor-paginated list of journals. Query: `cursor`, `limit`.

### GET /entries

List entries by account. **Required** query params: `holder` (int64), `currency_uid` (string, uuid). Optional: `cursor`, `limit`. Cursor-paginated.

---

## 5. Balances

### GET /balances/{holder}

All classification balances for a holder in a single currency.

**Required** query param: `currency_uid`.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "list": [
      {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "balance": "404.50"}
    ],
    "next_cursor": null
  }
}
```

### GET /balances/{holder}/{currency}

Same as above but with the currency's uid in the path instead of a query param; the response also includes a precomputed `total` instead of the paged-list envelope:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "total": "404.50",
    "classifications": [
      {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "balance": "404.50"}
    ]
  }
}
```

### GET /balances/{holder}/{currency}/breakdown

Holder-facing liquidity view: `available = Σ role=available − held`, `locked = Σ role=locked + held`, `pending = Σ role=pending`, `total = available + locked + pending` (I-11).

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "account_holder": 1001,
    "currency_uid": "cur-usdt",
    "available": "304.50",
    "pending": "0.00",
    "locked": "100.00",
    "total": "404.50"
  }
}
```

### POST /balances/batch

Fetch balances for up to **100 holders** in one currency.

Request:

```json
{"holder_ids": [1001, 1002], "currency_uid": "cur-usdt"}
```

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": [
    {"holder_id": 1001, "balances": [{"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "balance": "404.50"}]},
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
  "currency_uid": "cur-usdt",
  "amount": "100.00",
  "idempotency_key": "spend:user1001:order42",
  "expires_in_sec": 900
}
```

Response `201 Created`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "uid": "rsv-1",
    "account_holder": 1001,
    "currency_uid": "cur-usdt",
    "reserved_amount": "100.00",
    "status": "active",
    "idempotency_key": "spend:user1001:order42",
    "expires_at": "2026-04-17T10:15:00Z",
    "created_at": "2026-04-17T10:00:00Z",
    "updated_at": "2026-04-17T10:00:00Z"
  }
}
```

`settled_amount` and `journal_uid` are omitted until the reservation has a settlement leg / a linked journal.

Status codes: `201`, `400`, `401`, `422` (`14001` insufficient balance, `14002` duplicate), `429`, `503`.

**`require_verified_balance` (optional, default `false`).** An opt-in gate,
decided per call. When `true` the reservation is refused unless *every*
`balance_role = available` classification this holder has touched in
`currency_uid` passes the tamper-evident authorization check
(`422` / `14010`; `14011` when the signing key on a journal is not one this
deployment recognises).

It is also a **stricter amount check**, which is the part easy to miss: a
gated reservation is sized from an entries-only recompute of the available
base, *not* from `balance_checkpoints`. An inflated checkpoint row therefore
cannot raise what a gated reservation will lock, even when every journal is
genuinely signed. Insufficiency under that recomputed base answers `14001`
(insufficient balance), not `14010` — the two are different verdicts and
should be handled differently: `14001` means "this holder does not have it",
`14010`/`14011` means "this ledger cannot currently vouch for what it has"
(see [INVARIANTS I-32](INVARIANTS.md) and I-49).

Off by default and never inferred: the library provides the mechanism and
takes no view on when the extra check is warranted — a caller that never
sends the field sees no behaviour change. In library mode the same field on
`core.ReserveInput` is refused with `400` / `ErrInvalidInput` when the
`Reserve` is issued from inside a `RunInTx` callback, because the gate may
call a remote verifier and must run before a transaction is opened; it is
never silently downgraded to an ungated reservation.

### POST /reservations/{uid}/settle

Settle a reservation with the actual amount. Posts the settlement journal and releases any remainder.

Request: `{"actual_amount": "95.50"}`. Response `200 OK`: `{"status": "settled"}`.

Status codes: `200`, `400`, `401`, `404`, `422` (`14004` invalid state, `14005` expired), `429`, `503`.

### POST /reservations/{uid}/settle-partial

Settle part of a reservation; call repeatedly to accumulate. First call transitions `active` -> `settling`. Cumulative settled amount may never exceed `reserved_amount`. The unsettled remainder stays held against the balance until `/reservations/{uid}/finalize`.

Request: `{"amount": "40.00", "idempotency_key": "spend:user1001:order42:leg1"}`. `idempotency_key` is required (I-3): `SettlePartial` accumulates, so a retried request without a key would double-apply the amount.

### POST /reservations/{uid}/finalize

Finalize a `settling` reservation once every partial settlement leg has landed, posting the settlement journal for the accumulated `settled_amount` and releasing any remainder. No request body. Response `200 OK`: `{"status": "settled"}`.

### POST /reservations/{uid}/release

Release an active or settling reservation without settlement.

Response `200 OK`: `{"status": "released"}`.

### GET /reservations

List reservations.

Query: `holder` (int64), `status` (string), `cursor` (base64), `limit` (default 50, max 200). Cursor-paginated.

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

#### POST /classifications/{uid}/deactivate

Soft-disables the classification. Response `200 OK`: `{"status": "deactivated"}`.

#### GET /classifications

Query: `active_only=true|false`. Returns array of classifications.

### Journal Types

#### POST /journal-types

`{"code": "deposit", "name": "Deposit Confirmation"}` -> 201.

#### POST /journal-types/{uid}/deactivate

#### GET /journal-types

`?active_only=true`.

### Templates

#### POST /templates

```json
{
  "code": "deposit_confirm",
  "name": "Confirm Deposit",
  "journal_type_uid": "jt-deposit_confirm",
  "lines": [
    {"classification_uid": "cls-main_wallet", "entry_type": "debit", "holder_role": "user", "amount_key": "amount", "sort_order": 1},
    {"classification_uid": "cls-custodial", "entry_type": "credit", "holder_role": "system", "amount_key": "amount", "sort_order": 2}
  ]
}
```

`holder_role` is `user` or `system`. `amount_key` references the matching key in the `amounts` map at execution time.

Response `201 Created`: template object including its lines.

#### POST /templates/{uid}/deactivate

#### POST /templates/{code}/preview

Render a template without persisting. Body:

```json
{"holder_id": 1001, "currency_uid": "cur-usdt", "amounts": {"amount": "500.00"}}
```

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "entries": [
      {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "entry_type": "debit", "amount": "500.00"},
      {"account_holder": -1001, "currency_uid": "cur-usdt", "classification_uid": "cls-custodial", "entry_type": "credit", "amount": "500.00"}
    ]
  }
}
```

#### GET /templates

`?active_only=true`.

### Currencies

#### POST /currencies

`{"code": "USDT", "name": "Tether USD", "exponent": 6}` -> 201. `exponent` is **required** (`0`-`18`; e.g. JPY=0, USD=2, wei=18) — I-16's business decimal precision. There is no silent default: omitting it is a `400`, precisely because `0` is itself a legal exponent (JPY) and can't double as "not set."

#### POST /currencies/{uid}/deactivate

Soft-disables the currency. Response `200 OK`: `{"status": "deactivated"}`.

#### GET /currencies

Returns the full list (no pagination).

---

## 8. Reconciliation

### POST /reconcile

Global reconciliation: verifies the accounting equation across all journals.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
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

Request: `{"holder": 1001, "currency_uid": "cur-usdt"}`.

Response: same shape as global reconciliation, with `details[]` populated when drift is detected:

```json
{
  "balanced": false,
  "gap": "0.01",
  "details": [
    {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "expected": "500.00", "actual": "500.01", "drift": "0.01"}
  ],
  "checked_at": "2026-04-17T12:00:00Z"
}
```

### POST /reconcile/full

Runs the complete reconciliation suite on demand (`service.FullReconciliationService`, see `docs/RUNBOOK.md` §1 for the check-by-check meaning and incident response) — heavier than `/reconcile`, which only runs the global debit=credit check. Same schedule as the background worker; this endpoint triggers an out-of-band run. No request body.

Response `200 OK`: the report shape is `overall_passed` / `full_coverage` / `run_at` / `checks[]` (each with `name`, `passed`, `complete`, `checked_at`, `findings[]`) — **not** the `{balanced, gap, details}` shape `/reconcile` and `/reconcile/account` return. `overall_passed` alone is not a clean bill of health; require it together with `full_coverage` (a check can be capped, timed out, or skipped and still report no violation).

---

## 9. Snapshots

### GET /snapshots

Historical daily balance snapshots.

**Required** query params: `holder`, `currency_uid`, `start` (`YYYY-MM-DD`), `end` (`YYYY-MM-DD`). Returns array of snapshot rows.

```json
{
  "code": 200,
  "message": null,
  "data": [
    {"account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "snapshot_date": "2026-04-16", "balance": "404.50"}
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
  "code": 200,
  "message": null,
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

Kubernetes-style readiness probe. **Returns 200 only when the host process
says it is ready** — either `Deps.ReadyProbe` returns true, or
`(*server.Server).SetReady(true)` has been called. The library observes
neither migrations nor the worker: it runs no migrator, ships no binary, and
the worker is optional, so readiness is a property of your composition root
(E-M11 — this section used to say the probe turned green "once migrations +
worker have booted", which nothing implemented; a deployment that wired
neither got a permanent 503).

Not ready is the standard envelope, not a bespoke shape:

```json
{ "code": 18101, "message": { "text": "Service is starting or temporarily unavailable" }, "data": null }
```

with HTTP status `503`. Probe scripts key on the HTTP status, not on a
`status` string in the body.

Wiring, in the host:

```go
srv, err := server.NewFromDeps(cfg, server.Deps{ /* … */
    ReadyProbe: func() bool { return migrationsDone.Load() && worker.Healthy() },
})
// or, for a one-shot flip after boot:
srv.SetReady(true)
```

Two other lifecycle methods are equally easy to miss and equally
host-owned: `SetHolderSurface` (until called, every `/holder*` route answers
404) and `StartRateLimiterGC` (without it, per-IP rate-limit buckets are
never reclaimed).

### GET /system/balances

Aggregate system-wide balances by `(currency_uid, classification_uid)`.
This endpoint is a realtime ledger snapshot: each row is computed as
`checkpoint.balance + delta` across every contributing account, so fresh
journals are visible immediately without waiting for the rollup worker.
`updated_at` is the timestamp of the query snapshot returned by the API, not
the refresh time of the `system_rollups` table.

```json
{
  "code": 200,
  "message": null,
  "data": {
    "list": [
      {"currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "total_balance": "50000.00", "updated_at": "2026-04-17T12:00:00Z"}
    ],
    "next_cursor": null
  }
}
```

---

## 11. Audit

Read-only investigation endpoints for support / audit workflows. These expose the same `core.AuditQuerier` capability that `cmd/ledger-cli`'s `trace` command uses.

### GET /audit/journals

List journals either by account dimension or by a global time range — exactly one mode, selected by which params are present:

- `holder` + `currency_uid` (required together; optionally narrowed by `classification_uid`, `from`, `to`) — journals touching that account dimension.
- `from` and/or `to` alone (no `holder`) — a global scan across every account in that time window.

Providing neither, or `holder` without `currency_uid`, is a `400`.

Query params: `holder` (int64), `currency_uid` (string, uuid), `classification_uid` (string, uuid, omitted = all), `from` (RFC 3339), `to` (RFC 3339), `cursor`, `limit`. Cursor-paginated, same envelope shape as `GET /journals`.

Status codes: `200`, `400`.

### GET /audit/bookings/{uid}/trace

Fetch a booking together with every event and every journal generated by those events — the standard "trace this booking end-to-end" shape for support/audit investigation.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "booking": { "uid": "bkg-1", "status": "confirmed", "...": "..." },
    "events": [ { "uid": "evt-99", "from_status": "pending", "to_status": "confirmed", "...": "..." } ],
    "journals": [ { "uid": "jnl-1234", "...": "..." } ]
  }
}
```

Status codes: `200`, `400`, `404`.

### GET /audit/journals/{uid}/reversals

List the full reversal chain for a journal — the root journal plus any journals that transitively reverse it, oldest first.

Status codes: `200`, `400`.

---

## 12. Platform

Real-time, system-wide balance and solvency reads. These expose the same `core.PlatformBalanceReader` / `core.SolvencyChecker` capability that `cmd/ledger-cli`'s `solvency` command uses.

### GET /platform/balances

Per-classification breakdown of user-side (holder > 0) vs. system-side (holder < 0) balances for a currency, computed as `checkpoint.balance + delta` (no rollup-worker lag).

**Required** query param: `currency_uid`.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "currency_uid": "cur-usdt",
    "user_side": { "main_wallet": "125000.00" },
    "system_side": { "custodial": "125000.00" }
  }
}
```

Status codes: `200`, `400`.

### GET /platform/solvency

Compares total user-side liability against the custodial system balance for a currency.

**Required** query param: `currency_uid`.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "currency_uid": "cur-usdt",
    "liability": "125000.00",
    "custodial": "126500.00",
    "solvent": true,
    "margin": "1500.00"
  }
}
```

`margin` is `custodial - liability`; negative means under-collateralised in the ledger's picture. Comparing `custodial` to an off-chain custody position is the caller's responsibility.

Status codes: `200`, `400`.

---

## 13. Balance Trends

### GET /balances/trends

Historical daily balance series for a single account dimension. This exposes the same `core.BalanceTrendReader` capability previously only reachable via `cmd/ledger-cli`.

One point per calendar day in `[from, to]`. Days with no journal activity are forward-filled from the previous known balance; the point for today is always overridden with the live checkpoint+delta balance.

**Required** query params: `holder`, `currency_uid`, `from` (RFC 3339), `to` (RFC 3339). Optional: `classification_uid` (omitted = sum across all classifications).

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": [
    {"date": "2026-04-16", "balance": "404.50", "inflow": "50.00", "outflow": "0.00"},
    {"date": "2026-04-17", "balance": "454.50", "inflow": "50.00", "outflow": "0.00"}
  ]
}
```

Status codes: `200`, `400`.

---

## 14. Periods

Accounting period close: an append-only barrier that rejects new (or
reversal) postings whose `effective_at` predates the active close line.
Real-time balances are never affected — only future writes are gated. See
[COOKBOOK Recipe 8](COOKBOOK.md#recipe-8--retroactive-posting-and-period-close)
for the correction pattern.

### POST /periods/close

Append a new close line. `close_before` is the new barrier: journals with
`effective_at < close_before` are rejected. Reopening a period is done by
posting an earlier `close_before` than the current one — the previous line is
never overwritten (append-only; `GET /periods/closes` shows full history).

Request:

```json
{
  "close_before": "2026-04-01T00:00:00Z",
  "note": "March 2026 close",
  "actor_id": 7
}
```

Response `201 Created`: `{ "uid", "close_before", "note", "actor_id", "created_at" }`.

Status codes: `201`, `400`.

Auth: required.

### GET /periods/closes

List the close-line history, most recent first.

Query params: `limit` (default 50, max 200).

Response `200 OK`: array of the same object shape as `POST /periods/close`.

Status codes: `200`.

---

## 15. Reports

### GET /reports/trial-balance

Per-classification debit/credit totals for one currency as of a point in
time, plus the global debit=credit balanced check — the standard
close-readiness verification.

Query params: `currency_uid` (required), `as_of` (RFC 3339, optional — default now).

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "currency_uid": "cur-usdt",
    "as_of": "2026-04-01T00:00:00Z",
    "rows": [
      {"classification_uid": "cls-main_wallet", "classification_code": "main_wallet", "classification_name": "Main Wallet", "normal_side": "debit", "total_debit": "500.00", "total_credit": "0.00", "net": "500.00"},
      {"classification_uid": "cls-custodial", "classification_code": "custodial", "classification_name": "Custodial", "normal_side": "credit", "total_debit": "0.00", "total_credit": "500.00", "net": "500.00"}
    ],
    "total_debit": "500.00",
    "total_credit": "500.00",
    "balanced": true
  }
}
```

Status codes: `200`, `400`.

---

## 16. Accounts

Per-account (holder × currency × classification, any dimension may be
omitted to mean "all currencies" / "all classifications" for that holder)
freeze/close + balance-floor overrides — the HTTP face of
`core.AccountPolicyStore`, the same store library-mode callers reach via
`svc.AccountPolicies()`.

### PUT /accounts/{holder}/policy

Set (create or update) the policy for one account dimension.

Request:

```json
{
  "currency_uid": "cur-usdt",
  "classification_uid": "cls-main_wallet",
  "status": "frozen",
  "min_balance": "0",
  "enforce_min_balance": true,
  "note": "chargeback investigation #4821",
  "actor_id": 99
}
```

`currency_uid` / `classification_uid` empty = the policy applies to every
currency / classification for this holder. `status` is one of `active` /
`frozen` / `closed` — `frozen` rejects the write side of a flow (e.g. a
withdrawal reservation) while still allowing the credit side (e.g. a
deposit); `closed` rejects both. `min_balance` overrides the platform-wide
minimum-balance floor for this dimension and only takes effect when
`enforce_min_balance` is `true`: `0` = no overdraft, negative = an overdraft
limit, positive = a dust floor above zero.

Response `200 OK`: the `AccountPolicy` row — `uid`, `account_holder`,
`currency_uid`, `classification_uid`, `status`, `min_balance`,
`enforce_min_balance`, `note`, `updated_at`, `created_at`.

Status codes: `200`, `400`, `401`, `403`, `422`, `429`, `503`.

Auth: required, `admin` scope.

### GET /accounts/{holder}/policies

List every policy row set for a holder, across every currency and
classification.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "list": [
      {"uid": "pol-1", "account_holder": 1001, "currency_uid": "cur-usdt", "classification_uid": "cls-main_wallet", "status": "frozen", "min_balance": "0", "enforce_min_balance": true, "note": "chargeback investigation #4821", "updated_at": "2026-06-01T00:00:00Z", "created_at": "2026-06-01T00:00:00Z"}
    ],
    "next_cursor": null
  }
}
```

Status codes: `200`, `400`, `401`.

Auth: required, `read` scope.

## 17. Crypto Deposit

Address issuance and human-review-queue disposition for the CREATE2
deposit + sweep add-on (`svc.EnableOnchain`,
`docs/plans/2026-07-11-crypto-deposit-sweep-design.md`). Every endpoint in
this section answers `503`/`18102` ("This feature is not enabled on this
server") until that add-on is wired into the server's composition root — a
crypto-deposit-shaped 404/409 elsewhere in this section means the add-on
*is* wired in, just not for this particular booking.

### POST /holders/{holder}/deposit-address

Issue (idempotently) a holder's CREATE2 custody deposit address. Derives and
registers the address on first call; repeated calls for the same holder
always return the same one.

Response `201 Created`: `uid`, `account_holder`, `address` (EIP-55
checksummed), `created_at`. The CREATE2 derivation fingerprint
(factory/init-code hash) is an internal audit-only detail, deliberately not
part of this wire shape.

Status codes: `201`, `400`, `401`, `503`.

Auth: required, `write` scope. See [§18 Holder Wallet
Surface](#18-holder-wallet-surface) for the holder-token-scoped mirror of
this endpoint (`POST /holder/deposit-address`) — same address, issued to the
token's own holder rather than a path parameter, for a frontend that only
holds a holder token.

### GET /holders/{holder}/deposit-address

Look up a holder's already-registered deposit address. Does not create one.

Status codes: `200`, `401`, `404` (no address registered yet — `POST` the
same path first), `503`.

Auth: required, `read` scope.

### GET /deposits/reviews

List deposit bookings parked in human review (the M3 compensating control —
see `docs/plans/2026-07-11-crypto-deposit-sweep-design.md` §9). A deposit
lands here when it exceeds the configured auto-credit ceiling, or its
second, independent confirmation source disagrees with the primary sighting.
The `review` booking status *is* the review queue — this endpoint is a
filtered `GET /bookings`, not a separate table. Zero ledger effect until
approved.

Query params: `cursor`, `limit` (default 50, max 200 — [§Pagination](#pagination)).

Status codes: `200`, `401`, `503`.

Auth: required, `read` scope.

### POST /deposits/{uid}/review/approve

Approve a review-parked deposit, posting its `deposit_confirm` journal.
Idempotent — a no-op returning the current booking if it is already
confirmed. Any other non-`review` status is a `409` conflict.

Requires the `deposit_review` **capability** (`server.CapabilityDepositReview`),
independent of scope — see [Authentication](#authentication) above. A key
that only ingests/creates deposit bookings does not automatically get to
approve its own review; grant `deposit_review` on a separate reviewer key.

Status codes: `200`, `401`, `403` (missing the `deposit_review` capability),
`409` (booking is not in `review` status), `503`.

Auth: required, `deposit_review` capability.

### POST /deposits/{uid}/review/reject

Reject a review-parked deposit to `failed` — no journal is ever posted.
Idempotent — a no-op returning the current booking if it is already
`failed`. Any other non-`review` status is a `409` conflict.

Request:

```json
{ "reason": "confirmation sources disagree on amount" }
```

`reason` is required and lands on the booking's audit trail and the emitted
`deposit.review_rejected` event.

Status codes: `200`, `400` (missing `reason`), `401`, `403` (missing the
`deposit_review` capability), `409`, `503`.

Auth: required, `deposit_review` capability.

### GET /deposits/dead-letters

List dead-lettered deposit sightings: transfers that **are on chain**, to a
registered address, in a whitelisted token, that the ingestion path refused
(an idempotency-key payload conflict, an unregistered currency, an amount the
currency's exponent cannot represent, a watcher wedged on this sighting) after
which the forward scan moved past them. No booking exists for one, so nothing
else in the system revisits it: this queue and the replay below are the only
way back (`docs/RUNBOOK.md` §18).

Cursor-paginated, newest first (`?cursor=&limit=`, `limit` max 200). Each row
carries the sighting as recorded (`token`, `to`, `amount`) plus `booked`,
recomputed on every read: `true` means a booking now exists for that
sighting's idempotency key -- it was replayed, or the cause self-healed -- and
the row needs no action.

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "list": [
      {
        "uid": "01a0…",
        "chain_id": 1,
        "tx_hash": "0xdead…",
        "txlog_seq": 0,
        "idempotency_key": "deposit-1-0xdead…-0",
        "reason": "currency \"USDT-late\" is not registered",
        "booked": false,
        "token": "0xusdt…",
        "to": "0xabc…",
        "amount": "7",
        "created_at": "2026-09-04T00:00:00Z"
      }
    ],
    "next_cursor": null
  }
}
```

Status codes: `200`, `401`, `503` (crypto-deposit add-on not wired:
`server.SetDeadLetterService`).

Auth: required, `read` scope.

### POST /deposits/dead-letters/{uid}/replay

Re-drive one dead-lettered sighting through the **real** ingestion path
(`service.Onchain.ReplayDeadLetter` → `IngestDeposit`), after the cause has
been fixed -- a currency registered, an exponent corrected, a token added
back to `CreditTokens`. Runs the identical path a watcher sighting takes,
review gate included, so it is idempotent (a sighting already booked resolves
to the same booking and posts nothing new) and it cannot credit anything the
ordinary path would refuse. Returns the resulting booking.

Requires the `deposit_review` **capability**, not `write` scope: a replay can
end in the same `deposit_confirm` journal an approval posts, so the key that
can forge a sighting must not also be able to push one back in.

Status codes: `200`, `400` (this ledger has nothing to book for that sighting
-- the address is not registered or the token is not in the chain's
allowlist; deliberately an error rather than a silent no-op), `401`, `403`
(missing the capability), `404` (unknown dead-letter uid), `503`.

Auth: required, `deposit_review` capability.

## 18. Holder Wallet Surface

A second, narrower authentication scheme for the routes under `/holder/*`:
instead of the platform's own `API_KEYS` bearer token, these authenticate
with a **holder token** — short-lived, bound to exactly one account holder,
read-only except for the two deposit-address routes, which only ever
provision the token's own holder's receiving address and move no funds. This
is the surface a host backend hands to *its own* frontend after its own
session auth, so the frontend can render one user's wallet without ever
holding a platform-wide key. Disabled (every route 404s) unless
`HOLDER_TOKEN_SECRET` is configured — see [Configuration](../README.md#configuration).

### POST /holder-tokens

Mint a holder-scoped read token. Called by the **host backend** — with its
own platform `API_KEYS` bearer token, `write` scope — after its own session
auth, then handed to its frontend.

Request:

```json
{ "holder": 1001, "ttl_seconds": 900 }
```

`ttl_seconds` is optional; defaults to 15 minutes and is capped server-side
(default 1 hour) regardless of what a caller asks for. Leak blast radius: one
holder, read-only (plus the two deposit-address routes' narrow write), until
`exp`.

Response `200 OK`: `token` (an `lht_`-prefixed bearer value — same
`Authorization: Bearer <token>` header shape as an API key, but only the
`/holder/*` routes below accept it), `expires_at`.

Status codes: `200`, `400`, `401`.

Auth: required (platform key), `write` scope.

Every endpoint below authenticates with **the minted holder token** instead
— `Authorization: Bearer lht_...`, not a platform `API_KEYS` entry — and
scopes every response to that token's own holder; there is no holder path
parameter to spoof.

### GET /holder/balances

The token-bound holder's balances, one row per currency.

Query params: `currency_uid` (optional — omit for every currency the holder
has a nonzero dimension in).

Response `200 OK`:

```json
{
  "code": 200,
  "message": null,
  "data": {
    "list": [
      {"currency_uid": "cur-usdt", "currency_code": "USDT", "available": "500.00", "pending": "0.00", "locked": "50.00", "total": "550.00"}
    ],
    "next_cursor": null
  }
}
```

`total` = `available` + `pending` + `locked`.

Status codes: `200`, `401`.

Auth: required, holder token.

### GET /holder/transactions

The token-bound holder's translated transaction view — one row per
(journal, currency) net effect on the holder, newest first, in user
language (`kind` / `kind_label` / `direction` / `amount`), no double-entry
vocabulary. Cursor-paginated at journal granularity; see
[Pagination](#pagination) above for the newest-first cursor direction this
endpoint uses.

Query params: `cursor`, `limit` (max 100).

Status codes: `200`, `401`.

Auth: required, holder token.

### GET /holder/holds

The token-bound holder's outstanding holds (locked amounts with expiry),
cursor-paginated, newest first — same cursor shape and direction as `GET
/holder/transactions` (H-m9: this endpoint used to return the holder's
entire hold set with no limit).

Query params: `cursor`, `limit` (default 20, max 100).

Status codes: `200`, `401`.

Auth: required, holder token.

### GET /holder/deposit-address

Holder-scoped mirror of `GET /holders/{holder}/deposit-address` — same
`DepositAddressProvider`, but the holder comes exclusively from the token,
never a request parameter. Does not create one.

Status codes: `200`, `401`, `404` (no address registered yet), `503`
(crypto-deposit add-on not enabled).

Auth: required, holder token.

### POST /holder/deposit-address

Holder-scoped mirror of `POST /holders/{holder}/deposit-address` — derives
and registers the address on first call, repeated calls return the same
one. The holder is bound to the token; this route moves no funds, it only
provisions the caller's own receiving address.

Status codes: `201`, `401`, `503` (crypto-deposit add-on not enabled).

Auth: required, holder token.

---

## OpenAPI

The canonical machine-readable contract lives at [`openapi.yaml`](openapi.yaml). It is the source of truth for SDK generation; this Markdown is human-readable narrative.
