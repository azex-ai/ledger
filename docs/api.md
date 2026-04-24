# API Reference

Base URL: `http://localhost:8080/api/v1`

All list endpoints support cursor-based pagination: `?cursor=<base64>&limit=50` (max 200).

All error responses use the envelope:

```json
{"error": {"code": "invalid_input", "message": "description"}}
```

---

## Journals

### POST /journals

Post a manually balanced journal.

```bash
curl -X POST http://localhost:8080/api/v1/journals \
  -H "Content-Type: application/json" \
  -d '{
    "journal_type_id": 1,
    "idempotency_key": "deposit:user1001:1",
    "entries": [
      {
        "account_holder": 1001,
        "currency_id": 1,
        "classification_id": 1,
        "entry_type": "debit",
        "amount": "500.00"
      },
      {
        "account_holder": -1001,
        "currency_id": 1,
        "classification_id": 2,
        "entry_type": "credit",
        "amount": "500.00"
      }
    ],
    "source": "api",
    "metadata": {"tx_hash": "0xabc..."}
  }'
```

**Response** `201 Created`

```json
{
  "id": 1,
  "journal_type_id": 1,
  "idempotency_key": "deposit:user1001:1",
  "total_debit": "500.00",
  "total_credit": "500.00",
  "source": "api",
  "metadata": {"tx_hash": "0xabc..."},
  "created_at": "2026-04-17T10:00:00Z"
}
```

**Errors:**
- `400` -- unbalanced entries, missing idempotency key, invalid entry type
- `200` -- duplicate idempotency key (returns existing journal)

### POST /journals/template

Post a journal using a predefined template.

```bash
curl -X POST http://localhost:8080/api/v1/journals/template \
  -H "Content-Type: application/json" \
  -d '{
    "template_code": "deposit_confirm",
    "holder_id": 1001,
    "currency_id": 1,
    "idempotency_key": "deposit_journal:0xabc...",
    "amounts": {"amount": "500.00"},
    "source": "deposit_confirm"
  }'
```

**Response** `201 Created` -- same as POST /journals.

### POST /journals/deposit-tolerance

Apply deposit tolerance settlement for a staged deposit. The server will execute one or more preset templates based on `expected_amount`, `actual_amount`, and `tolerance`.

```bash
curl -X POST http://localhost:8080/api/v1/journals/deposit-tolerance \
  -H "Content-Type: application/json" \
  -d '{
    "holder_id": 1001,
    "currency_id": 1,
    "idempotency_key": "deposit_tol:0xabc...",
    "expected_amount": "100.00",
    "actual_amount": "98.00",
    "tolerance": "5.00",
    "source": "deposit_confirm"
  }'
```

**Response** `201 Created`

```json
{
  "outcome": "shortfall_auto_released",
  "expected_amount": "100.00",
  "actual_amount": "98.00",
  "tolerance": "5.00",
  "delta": "2.00",
  "requires_manual_review": false,
  "journals": [
    {"id": 10, "idempotency_key": "deposit_tol:0xabc...:confirm-pending"},
    {"id": 11, "idempotency_key": "deposit_tol:0xabc...:release-shortfall"}
  ]
}
```

### POST /journals/:id/reverse

Create a reversal journal (swaps all debit/credit entries).

```bash
curl -X POST http://localhost:8080/api/v1/journals/1/reverse \
  -H "Content-Type: application/json" \
  -d '{"reason": "duplicate deposit"}'
```

**Response** `201 Created` -- reversal journal with `reversal_of` set to the original journal ID.

### GET /journals/:id

Get a journal with its entries.

```bash
curl http://localhost:8080/api/v1/journals/1
```

**Response** `200 OK`

```json
{
  "journal": {
    "id": 1,
    "journal_type_id": 1,
    "idempotency_key": "deposit:user1001:1",
    "total_debit": "500.00",
    "total_credit": "500.00",
    "source": "api",
    "metadata": {},
    "created_at": "2026-04-17T10:00:00Z"
  },
  "entries": [
    {
      "id": 1,
      "journal_id": 1,
      "account_holder": 1001,
      "currency_id": 1,
      "classification_id": 1,
      "entry_type": "debit",
      "amount": "500.00",
      "created_at": "2026-04-17T10:00:00Z"
    }
  ]
}
```

### GET /journals

List journals with cursor pagination.

```bash
curl "http://localhost:8080/api/v1/journals?cursor=&limit=20"
```

**Response** `200 OK`

```json
{
  "data": [{"id": 1, "...": "..."}],
  "next_cursor": "AAAAAAAAAAI="
}
```

---

## Entries

### GET /entries

List entries by account with cursor pagination.

```bash
curl "http://localhost:8080/api/v1/entries?holder=1001&currency_id=1&cursor=&limit=50"
```

**Response** `200 OK`

```json
{
  "data": [
    {
      "id": 1,
      "journal_id": 1,
      "account_holder": 1001,
      "currency_id": 1,
      "classification_id": 1,
      "entry_type": "debit",
      "amount": "500.00",
      "created_at": "2026-04-17T10:00:00Z"
    }
  ],
  "next_cursor": ""
}
```

---

## Balances

### GET /balances/:holder

Get all balances for a holder across all classifications.

```bash
curl http://localhost:8080/api/v1/balances/1001
```

**Response** `200 OK`

```json
{
  "data": [
    {
      "account_holder": 1001,
      "currency_id": 1,
      "classification_id": 1,
      "balance": "404.50"
    }
  ]
}
```

### GET /balances/:holder/:currency

Get balances for a specific holder and currency.

```bash
curl http://localhost:8080/api/v1/balances/1001/1
```

**Response** `200 OK` -- same format as above, filtered to one currency.

### POST /balances/batch

Batch query balances for multiple holders.

```bash
curl -X POST http://localhost:8080/api/v1/balances/batch \
  -H "Content-Type: application/json" \
  -d '{"holder_ids": [1001, 1002], "currency_id": 1}'
```

**Response** `200 OK`

```json
{
  "1001": [
    {"account_holder": 1001, "currency_id": 1, "classification_id": 1, "balance": "404.50"}
  ],
  "1002": []
}
```

---

## Reservations

### POST /reservations

Create an amount reservation (pessimistic lock).

```bash
curl -X POST http://localhost:8080/api/v1/reservations \
  -H "Content-Type: application/json" \
  -d '{
    "account_holder": 1001,
    "currency_id": 1,
    "amount": "100.00",
    "idempotency_key": "spend:user1001:order42",
    "expires_in": "15m"
  }'
```

**Response** `201 Created`

```json
{
  "id": 1,
  "account_holder": 1001,
  "currency_id": 1,
  "reserved_amount": "100.00",
  "status": "active",
  "idempotency_key": "spend:user1001:order42",
  "expires_at": "2026-04-17T10:15:00Z",
  "created_at": "2026-04-17T10:00:00Z"
}
```

### POST /reservations/:id/settle

Settle a reservation with the actual amount.

```bash
curl -X POST http://localhost:8080/api/v1/reservations/1/settle \
  -H "Content-Type: application/json" \
  -d '{"actual_amount": "95.50"}'
```

**Response** `200 OK`

### POST /reservations/:id/release

Release (cancel) an active reservation.

```bash
curl -X POST http://localhost:8080/api/v1/reservations/1/release
```

**Response** `200 OK`

### GET /reservations

List reservations with optional status filter.

```bash
curl "http://localhost:8080/api/v1/reservations?holder=1001&status=active&limit=20"
```

**Response** `200 OK` -- paginated list of reservations.

---

## Deposits

### POST /deposits

Initiate a deposit (creates in `pending` status).

```bash
curl -X POST http://localhost:8080/api/v1/deposits \
  -H "Content-Type: application/json" \
  -d '{
    "account_holder": 1001,
    "currency_id": 1,
    "expected_amount": "500.00",
    "channel_name": "evm_create2",
    "idempotency_key": "deposit:user1001:1",
    "metadata": {"chain": "base", "address": "0x742d..."}
  }'
```

**Response** `201 Created`

```json
{
  "id": 1,
  "account_holder": 1001,
  "currency_id": 1,
  "expected_amount": "500.00",
  "status": "pending",
  "channel_name": "evm_create2",
  "idempotency_key": "deposit:user1001:1",
  "metadata": {"chain": "base"},
  "created_at": "2026-04-17T10:00:00Z"
}
```

### POST /deposits/:id/confirming

Mark deposit as confirming (chain tx detected, awaiting confirmations).

```bash
curl -X POST http://localhost:8080/api/v1/deposits/1/confirming \
  -H "Content-Type: application/json" \
  -d '{"channel_ref": "0xabc123..."}'
```

**Response** `200 OK`

### POST /deposits/:id/confirm

Confirm deposit with actual amount.

```bash
curl -X POST http://localhost:8080/api/v1/deposits/1/confirm \
  -H "Content-Type: application/json" \
  -d '{
    "actual_amount": "500.00",
    "channel_ref": "0xabc123..."
  }'
```

**Response** `200 OK`

### POST /deposits/:id/fail

Mark deposit as failed.

```bash
curl -X POST http://localhost:8080/api/v1/deposits/1/fail \
  -H "Content-Type: application/json" \
  -d '{"reason": "transaction reverted"}'
```

**Response** `200 OK`

**Errors:**
- `409 Conflict` -- invalid state transition (e.g., confirming a failed deposit)

### GET /deposits

List deposits.

```bash
curl "http://localhost:8080/api/v1/deposits?holder=1001&limit=20"
```

**Response** `200 OK` -- paginated list of deposits.

---

## Withdrawals

### POST /withdrawals

Initiate a withdrawal (creates in `locked` status).

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals \
  -H "Content-Type: application/json" \
  -d '{
    "account_holder": 1001,
    "currency_id": 1,
    "amount": "200.00",
    "channel_name": "evm_create2",
    "idempotency_key": "withdraw:user1001:1",
    "review_required": true
  }'
```

**Response** `201 Created`

```json
{
  "id": 1,
  "account_holder": 1001,
  "currency_id": 1,
  "amount": "200.00",
  "status": "locked",
  "channel_name": "evm_create2",
  "review_required": true,
  "created_at": "2026-04-17T10:00:00Z"
}
```

### POST /withdrawals/:id/reserve

Lock balance for withdrawal (Reserve).

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals/1/reserve
```

**Response** `200 OK`

### POST /withdrawals/:id/review

Approve or reject withdrawal.

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals/1/review \
  -H "Content-Type: application/json" \
  -d '{"approved": true}'
```

**Response** `200 OK`

### POST /withdrawals/:id/process

Submit to channel for execution.

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals/1/process \
  -H "Content-Type: application/json" \
  -d '{"channel_ref": "0xdef456..."}'
```

**Response** `200 OK`

### POST /withdrawals/:id/confirm

Mark withdrawal as confirmed by channel.

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals/1/confirm
```

**Response** `200 OK`

### POST /withdrawals/:id/fail

Mark withdrawal as failed.

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals/1/fail \
  -H "Content-Type: application/json" \
  -d '{"reason": "insufficient gas"}'
```

**Response** `200 OK`

### POST /withdrawals/:id/retry

Retry a failed withdrawal (transitions back to `reserved`).

```bash
curl -X POST http://localhost:8080/api/v1/withdrawals/1/retry
```

**Response** `200 OK`

**Errors:**
- `409 Conflict` -- invalid state transition
- Note: expired withdrawals cannot be retried

### GET /withdrawals

List withdrawals.

```bash
curl "http://localhost:8080/api/v1/withdrawals?holder=1001&limit=20"
```

**Response** `200 OK` -- paginated list of withdrawals.

---

## Metadata

### Classifications

#### POST /classifications

```bash
curl -X POST http://localhost:8080/api/v1/classifications \
  -H "Content-Type: application/json" \
  -d '{
    "code": "main_wallet",
    "name": "Main Wallet",
    "normal_side": "debit",
    "is_system": false
  }'
```

**Response** `201 Created`

```json
{
  "id": 1,
  "code": "main_wallet",
  "name": "Main Wallet",
  "normal_side": "debit",
  "is_system": false,
  "is_active": true,
  "created_at": "2026-04-17T10:00:00Z"
}
```

#### POST /classifications/:id/deactivate

```bash
curl -X POST http://localhost:8080/api/v1/classifications/1/deactivate
```

**Response** `200 OK`

#### GET /classifications

```bash
curl "http://localhost:8080/api/v1/classifications?active_only=true"
```

**Response** `200 OK` -- array of classifications.

### Journal Types

#### POST /journal-types

```bash
curl -X POST http://localhost:8080/api/v1/journal-types \
  -H "Content-Type: application/json" \
  -d '{"code": "deposit", "name": "Deposit Confirmation"}'
```

**Response** `201 Created`

#### POST /journal-types/:id/deactivate

```bash
curl -X POST http://localhost:8080/api/v1/journal-types/1/deactivate
```

#### GET /journal-types

```bash
curl "http://localhost:8080/api/v1/journal-types?active_only=true"
```

### Templates

#### POST /templates

Create a template with its line definitions.

```bash
curl -X POST http://localhost:8080/api/v1/templates \
  -H "Content-Type: application/json" \
  -d '{
    "code": "deposit_confirm",
    "name": "Confirm Deposit",
    "journal_type_id": 1,
    "lines": [
      {
        "classification_id": 1,
        "entry_type": "debit",
        "holder_role": "user",
        "amount_key": "amount",
        "sort_order": 1
      },
      {
        "classification_id": 2,
        "entry_type": "credit",
        "holder_role": "system",
        "amount_key": "amount",
        "sort_order": 2
      }
    ]
  }'
```

**Response** `201 Created`

```json
{
  "id": 1,
  "code": "deposit_confirm",
  "name": "Confirm Deposit",
  "journal_type_id": 1,
  "is_active": true,
  "lines": [
    {
      "id": 1,
      "classification_id": 1,
      "entry_type": "debit",
      "holder_role": "user",
      "amount_key": "amount",
      "sort_order": 1
    }
  ],
  "created_at": "2026-04-17T10:00:00Z"
}
```

#### POST /templates/:id/deactivate

```bash
curl -X POST http://localhost:8080/api/v1/templates/1/deactivate
```

#### GET /templates/:code/preview

Dry-run a template with parameters. Returns the entries that would be created without persisting.

```bash
curl "http://localhost:8080/api/v1/templates/deposit_confirm/preview?holder_id=1001&currency_id=1&amount=500.00"
```

**Response** `200 OK`

```json
{
  "entries": [
    {
      "account_holder": 1001,
      "currency_id": 1,
      "classification_id": 1,
      "entry_type": "debit",
      "amount": "500.00"
    },
    {
      "account_holder": -1001,
      "currency_id": 1,
      "classification_id": 2,
      "entry_type": "credit",
      "amount": "500.00"
    }
  ],
  "total_debit": "500.00",
  "total_credit": "500.00"
}
```

#### GET /templates

```bash
curl "http://localhost:8080/api/v1/templates?active_only=true"
```

### Currencies

#### POST /currencies

```bash
curl -X POST http://localhost:8080/api/v1/currencies \
  -H "Content-Type: application/json" \
  -d '{"code": "USDT", "name": "Tether USD"}'
```

**Response** `201 Created`

```json
{"id": 1, "code": "USDT", "name": "Tether USD"}
```

#### GET /currencies

```bash
curl http://localhost:8080/api/v1/currencies
```

---

## Reconciliation

### POST /reconcile

Run global reconciliation (verifies `SUM(all debits) == SUM(all credits)`).

```bash
curl -X POST http://localhost:8080/api/v1/reconcile
```

**Response** `200 OK`

```json
{
  "balanced": true,
  "gap": "0",
  "checked_at": "2026-04-17T12:00:00Z"
}
```

### POST /reconcile/account

Run per-account reconciliation (verifies checkpoint balances match entry sums).

```bash
curl -X POST http://localhost:8080/api/v1/reconcile/account \
  -H "Content-Type: application/json" \
  -d '{"holder": 1001, "currency_id": 1}'
```

**Response** `200 OK`

```json
{
  "balanced": true,
  "gap": "0",
  "details": [],
  "checked_at": "2026-04-17T12:00:00Z"
}
```

If drift is detected:

```json
{
  "balanced": false,
  "gap": "0.01",
  "details": [
    {
      "account_holder": 1001,
      "currency_id": 1,
      "classification_id": 1,
      "expected": "500.00",
      "actual": "500.01",
      "drift": "0.01"
    }
  ],
  "checked_at": "2026-04-17T12:00:00Z"
}
```

---

## Snapshots

### GET /snapshots

Query historical balance snapshots by date range.

```bash
curl "http://localhost:8080/api/v1/snapshots?holder=1001&currency_id=1&start=2026-04-01&end=2026-04-17"
```

**Response** `200 OK`

```json
{
  "data": [
    {
      "account_holder": 1001,
      "currency_id": 1,
      "classification_id": 1,
      "snapshot_date": "2026-04-16",
      "balance": "404.50"
    }
  ]
}
```

---

## System

### GET /system/health

Operational health check.

```bash
curl http://localhost:8080/api/v1/system/health
```

**Response** `200 OK`

```json
{
  "status": "ok",
  "rollup_queue_depth": 0,
  "checkpoint_max_age_seconds": 12,
  "active_reservations": 3
}
```

### GET /system/balances

Aggregated system-wide balances by (currency, classification).

```bash
curl http://localhost:8080/api/v1/system/balances
```

**Response** `200 OK`

```json
{
  "data": [
    {
      "currency_id": 1,
      "classification_id": 1,
      "total_balance": "50000.00",
      "updated_at": "2026-04-17T12:00:00Z"
    }
  ]
}
```
