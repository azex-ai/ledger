# Ledger v2 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Refactor the ledger from hardcoded deposit/withdrawal entities to a classification-driven architecture with unified operations, atomic event-journal model, and pluggable webhook delivery.

**Architecture:** Classification (科目) becomes the primary entity. Deposit and withdrawal are demoted to preset configurations. All operations share a unified `operations` table. Events are atomic with journals (same transaction). Channel adapters handle inbound webhooks. Event delivery is pluggable (sync callback for library mode, async HTTP for service mode).

**Tech Stack:** Go 1.26+, chi v5, pgx v5, sqlc, shopspring/decimal, PostgreSQL 17

**Design Doc:** `docs/plans/2026-04-22-ledger-v2-design.md`

**Key Constraints:**
- All DB columns NOT NULL with meaningful defaults (0, '', 'epoch', '{}')
- `core/` has zero external dependencies (no net/http, pgx, slog)
- All amounts use `shopspring/decimal.Decimal`
- Every mutation requires idempotency_key
- Single-direction data flow: ledger never calls external systems

---

## Phase 1: Core Layer — New Types & Interfaces

> Foundation. Everything else depends on this. Commit: `feat(core): add classification lifecycle, operation, and event types`

### Task 1.1: Add Lifecycle to Classification

**Files:**
- Modify: `core/types.go`

**Step 1: Add Status and Lifecycle types**

Add after the existing `Classification` struct (line 59):

```go
// Status represents a state in a classification's lifecycle.
type Status string

// Lifecycle defines the state machine for a classification.
// Pure state transition rules — no side effects, no template/event bindings.
// nil Lifecycle on a Classification means it's a label-only classification (no operations).
type Lifecycle struct {
    Initial     Status            `json:"initial"`
    Terminal    []Status          `json:"terminal"`
    Transitions map[Status][]Status `json:"transitions"`
}

func (l *Lifecycle) Validate() error {
    if l.Initial == "" {
        return fmt.Errorf("core: lifecycle: initial status is empty")
    }
    // Initial must have outgoing transitions
    if _, ok := l.Transitions[l.Initial]; !ok {
        return fmt.Errorf("core: lifecycle: initial status %q has no transitions", l.Initial)
    }
    // Terminal states must not have outgoing transitions
    termSet := make(map[Status]bool, len(l.Terminal))
    for _, t := range l.Terminal {
        termSet[t] = true
        if _, ok := l.Transitions[t]; ok {
            return fmt.Errorf("core: lifecycle: terminal status %q has outgoing transitions", t)
        }
    }
    // All transition targets must be defined somewhere
    for from, targets := range l.Transitions {
        for _, to := range targets {
            if !termSet[to] {
                if _, ok := l.Transitions[to]; !ok {
                    return fmt.Errorf("core: lifecycle: status %q transitions to undefined %q", from, to)
                }
            }
        }
    }
    return nil
}

func (l *Lifecycle) CanTransition(from, to Status) bool {
    targets, ok := l.Transitions[from]
    if !ok {
        return false
    }
    for _, t := range targets {
        if t == to {
            return true
        }
    }
    return false
}

func (l *Lifecycle) IsTerminal(s Status) bool {
    for _, t := range l.Terminal {
        if t == s {
            return true
        }
    }
    return false
}
```

**Step 2: Add Lifecycle field to Classification**

Modify the existing `Classification` struct to add the Lifecycle pointer:

```go
type Classification struct {
    ID         int64      `json:"id"`
    Code       string     `json:"code"`
    Name       string     `json:"name"`
    NormalSide NormalSide `json:"normal_side"`
    IsSystem   bool       `json:"is_system"`
    IsActive   bool       `json:"is_active"`
    Lifecycle  *Lifecycle `json:"lifecycle"`
    CreatedAt  time.Time  `json:"created_at"`
}
```

**Step 3: Run build**

```bash
go build ./core/...
```

---

### Task 1.2: Add Operation type

**Files:**
- Create: `core/operation.go`

**Step 1: Write the Operation type**

```go
package core

import (
    "time"
    "github.com/shopspring/decimal"
)

// Operation represents an instance of a classification's lifecycle.
// Replaces the v1 Deposit and Withdrawal types.
type Operation struct {
    ID               int64              `json:"id"`
    ClassificationID int64              `json:"classification_id"`
    AccountHolder    int64              `json:"account_holder"`
    CurrencyID       int64              `json:"currency_id"`
    Amount           decimal.Decimal    `json:"amount"`
    SettledAmount    decimal.Decimal    `json:"settled_amount"`
    Status           Status             `json:"status"`
    ChannelName      string             `json:"channel_name"`
    ChannelRef       string             `json:"channel_ref"`
    ReservationID    int64              `json:"reservation_id"`
    JournalID        int64              `json:"journal_id"`
    IdempotencyKey   string             `json:"idempotency_key"`
    Metadata         map[string]any     `json:"metadata"`
    ExpiresAt        time.Time          `json:"expires_at"`
    CreatedAt        time.Time          `json:"created_at"`
    UpdatedAt        time.Time          `json:"updated_at"`
}

type CreateOperationInput struct {
    ClassificationCode string             `json:"classification_code"`
    AccountHolder      int64              `json:"account_holder"`
    CurrencyID         int64              `json:"currency_id"`
    Amount             decimal.Decimal    `json:"amount"`
    IdempotencyKey     string             `json:"idempotency_key"`
    ChannelName        string             `json:"channel_name"`
    Metadata           map[string]any     `json:"metadata"`
    ExpiresAt          time.Time          `json:"expires_at"`
}

type TransitionInput struct {
    OperationID int64              `json:"operation_id"`
    ToStatus    Status             `json:"to_status"`
    ChannelRef  string             `json:"channel_ref"`
    Amount      decimal.Decimal    `json:"amount"`
    Metadata    map[string]any     `json:"metadata"`
    ActorID     int64              `json:"actor_id"`
}

type OperationFilter struct {
    AccountHolder    int64  `json:"account_holder"`
    ClassificationID int64  `json:"classification_id"`
    Status           string `json:"status"`
    Cursor           int64  `json:"cursor"`
    Limit            int    `json:"limit"`
}
```

**Step 2: Run build**

```bash
go build ./core/...
```

---

### Task 1.3: Add Event type

**Files:**
- Create: `core/event.go`

**Step 1: Write the Event type**

```go
package core

import (
    "time"
    "github.com/shopspring/decimal"
)

// Event records a state transition. It is the "reason" a journal entry exists.
// Written atomically with the operation state change in the same DB transaction.
type Event struct {
    ID                 int64              `json:"id"`
    ClassificationCode string             `json:"classification_code"`
    OperationID        int64              `json:"operation_id"`
    AccountHolder      int64              `json:"account_holder"`
    CurrencyID         int64              `json:"currency_id"`
    FromStatus         Status             `json:"from_status"`
    ToStatus           Status             `json:"to_status"`
    Amount             decimal.Decimal    `json:"amount"`
    SettledAmount      decimal.Decimal    `json:"settled_amount"`
    JournalID          int64              `json:"journal_id"`
    Metadata           map[string]any     `json:"metadata"`
    OccurredAt         time.Time          `json:"occurred_at"`
}

type EventFilter struct {
    ClassificationCode string `json:"classification_code"`
    OperationID        int64  `json:"operation_id"`
    ToStatus           string `json:"to_status"`
    Cursor             int64  `json:"cursor"`
    Limit              int    `json:"limit"`
}
```

**Step 2: Run build**

```bash
go build ./core/...
```

---

### Task 1.4: Update Journal with EventID

**Files:**
- Modify: `core/journal.go`

**Step 1: Add EventID field to Journal struct**

Add `EventID int64` field to the Journal struct (currently lines 11-22). Also update JournalInput to accept EventID.

```go
type Journal struct {
    ID             int64
    JournalTypeID  int64
    IdempotencyKey string
    TotalDebit     decimal.Decimal
    TotalCredit    decimal.Decimal
    Metadata       map[string]any
    ActorID        int64
    Source         string
    ReversalOf     int64
    EventID        int64  // v2: the event that caused this journal (0 = manual)
    CreatedAt      time.Time
}
```

Update JournalInput similarly — add `EventID int64` field.

**Step 2: Run build**

```bash
go build ./core/...
```

---

### Task 1.5: Update Interfaces

**Files:**
- Modify: `core/interfaces.go`

**Step 1: Replace Depositor + Withdrawer with Operator**

Remove the `Depositor` interface (lines 32-38) and `Withdrawer` interface (lines 41-49). Replace with:

```go
// Operator manages operations for any classification with a lifecycle.
type Operator interface {
    CreateOperation(ctx context.Context, input CreateOperationInput) (*Operation, error)
    Transition(ctx context.Context, input TransitionInput) (*Event, error)
}

type OperationReader interface {
    GetOperation(ctx context.Context, id int64) (*Operation, error)
    ListOperations(ctx context.Context, filter OperationFilter) ([]Operation, error)
}

type EventReader interface {
    GetEvent(ctx context.Context, id int64) (*Event, error)
    ListEvents(ctx context.Context, filter EventFilter) ([]Event, error)
}

// EventDeliverer is the pluggable delivery mechanism.
// Library mode: sync callback. Service mode: async HTTP webhook.
type EventDeliverer interface {
    Deliver(ctx context.Context, event Event) error
}
```

Update `ClassificationStore` to add `GetByCode`:

```go
type ClassificationStore interface {
    Create(ctx context.Context, input ClassificationInput) (*Classification, error)
    Deactivate(ctx context.Context, id int64) error
    Get(ctx context.Context, id int64) (*Classification, error)
    GetByCode(ctx context.Context, code string) (*Classification, error)
    List(ctx context.Context, activeOnly bool) ([]Classification, error)
}
```

Update `ChannelAdapter` — remove it from core (it depends on net/http, moves to `channel/` package).

**Step 2: Run build (expect failures — downstream code references removed interfaces)**

```bash
go build ./core/...
```

Expected: PASS (core has no downstream dependencies within itself)

**Step 3: Commit Phase 1**

```bash
git add core/
git commit -m "feat(core): add classification lifecycle, operation, and event types

- Lifecycle: pure state machine on Classification (states + transitions)
- Operation: unified type replacing Deposit/Withdrawal
- Event: atomic record of state transitions (cause of journals)
- Journal.EventID: links journal to triggering event
- Operator interface: replaces Depositor + Withdrawer
- EventReader, EventDeliverer interfaces"
```

---

## Phase 2: Presets + Channel Adapter

> New packages that depend only on core. Commit: `feat: add classification presets and channel adapter interface`

### Task 2.1: Create preset configurations

**Files:**
- Create: `presets/deposit.go`
- Create: `presets/withdrawal.go`

**Step 1: Write deposit preset**

```go
package presets

import "github.com/azex-ai/ledger/core"

var DepositLifecycle = &core.Lifecycle{
    Initial:  "pending",
    Terminal: []core.Status{"confirmed", "failed", "expired"},
    Transitions: map[core.Status][]core.Status{
        "pending":    {"confirming", "failed", "expired"},
        "confirming": {"confirmed", "failed"},
    },
}
```

**Step 2: Write withdrawal preset**

```go
package presets

import "github.com/azex-ai/ledger/core"

var WithdrawalLifecycle = &core.Lifecycle{
    Initial:  "locked",
    Terminal: []core.Status{"confirmed", "failed", "expired"},
    Transitions: map[core.Status][]core.Status{
        "locked":     {"reserved"},
        "reserved":   {"reviewing", "processing"},
        "reviewing":  {"processing", "failed"},
        "processing": {"confirmed", "failed", "expired"},
        "failed":     {"reserved"},
    },
}
```

**Step 3: Run build**

```bash
go build ./presets/...
```

---

### Task 2.2: Create channel adapter interface + EVM demo

**Files:**
- Create: `channel/adapter.go`
- Create: `channel/onchain/evm.go`

**Step 1: Write adapter interface**

```go
package channel

import (
    "net/http"
    "github.com/shopspring/decimal"
)

type CallbackPayload struct {
    OperationID  int64
    ChannelRef   string
    Status       string
    ActualAmount decimal.Decimal
    Metadata     map[string]any
}

type Adapter interface {
    Name() string
    VerifySignature(header http.Header, body []byte) error
    ParseCallback(header http.Header, body []byte) (*CallbackPayload, error)
}
```

**Step 2: Write EVM demo adapter**

```go
package onchain

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "net/http"

    "github.com/azex-ai/ledger/channel"
    "github.com/shopspring/decimal"
)

type EVMAdapter struct {
    SigningKey []byte
}

func New(signingKey []byte) *EVMAdapter {
    return &EVMAdapter{SigningKey: signingKey}
}

func (a *EVMAdapter) Name() string { return "evm" }

func (a *EVMAdapter) VerifySignature(header http.Header, body []byte) error {
    sig := header.Get("X-Signature")
    if sig == "" {
        return fmt.Errorf("channel: evm: missing X-Signature header")
    }
    mac := hmac.New(sha256.New, a.SigningKey)
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    if !hmac.Equal([]byte(sig), []byte(expected)) {
        return fmt.Errorf("channel: evm: signature mismatch")
    }
    return nil
}

func (a *EVMAdapter) ParseCallback(header http.Header, body []byte) (*channel.CallbackPayload, error) {
    var raw struct {
        TxHash        string `json:"tx_hash"`
        OperationID   int64  `json:"operation_id"`
        Amount        string `json:"amount"`
        Confirmations int    `json:"confirmations"`
        Status        string `json:"status"`
    }
    if err := json.Unmarshal(body, &raw); err != nil {
        return nil, fmt.Errorf("channel: evm: parse: %w", err)
    }

    amount, err := decimal.NewFromString(raw.Amount)
    if err != nil {
        return nil, fmt.Errorf("channel: evm: invalid amount %q: %w", raw.Amount, err)
    }

    return &channel.CallbackPayload{
        OperationID:  raw.OperationID,
        ChannelRef:   raw.TxHash,
        Status:       raw.Status,
        ActualAmount: amount,
        Metadata: map[string]any{
            "confirmations": raw.Confirmations,
            "tx_hash":       raw.TxHash,
        },
    }, nil
}
```

**Step 3: Run build**

```bash
go build ./channel/... ./presets/...
```

**Step 4: Commit**

```bash
git add presets/ channel/
git commit -m "feat: add classification presets and channel adapter interface

- presets/deposit.go: deposit lifecycle (pending→confirming→confirmed)
- presets/withdrawal.go: withdrawal lifecycle (locked→reserved→...→confirmed)
- channel/adapter.go: ChannelAdapter interface for inbound webhooks
- channel/onchain/evm.go: demo EVM adapter with HMAC signature verification"
```

---

## Phase 3: Database Schema

> Migrations for new tables + no-null adaptations. Commit: `feat(postgres): add operations, events tables and no-null migrations`

### Task 3.1: Write migrations

**Files:**
- Create: `postgres/sql/migrations/012_operations.up.sql`
- Create: `postgres/sql/migrations/012_operations.down.sql`
- Create: `postgres/sql/migrations/013_events.up.sql`
- Create: `postgres/sql/migrations/013_events.down.sql`
- Create: `postgres/sql/migrations/014_journal_event_id.up.sql`
- Create: `postgres/sql/migrations/014_journal_event_id.down.sql`
- Create: `postgres/sql/migrations/015_classification_lifecycle.up.sql`
- Create: `postgres/sql/migrations/015_classification_lifecycle.down.sql`
- Create: `postgres/sql/migrations/016_webhook_subscribers.up.sql`
- Create: `postgres/sql/migrations/016_webhook_subscribers.down.sql`
- Create: `postgres/sql/migrations/017_no_null_cleanup.up.sql`
- Create: `postgres/sql/migrations/017_no_null_cleanup.down.sql`

**Step 1: Write 012_operations migration**

```sql
-- 012_operations.up.sql
CREATE TABLE operations (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_id BIGINT NOT NULL,
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    amount            NUMERIC(30,18) NOT NULL,
    settled_amount    NUMERIC(30,18) NOT NULL DEFAULT 0,
    status            TEXT NOT NULL,
    channel_name      TEXT NOT NULL DEFAULT '',
    channel_ref       TEXT NOT NULL DEFAULT '',
    reservation_id    BIGINT NOT NULL DEFAULT 0,
    journal_id        BIGINT NOT NULL DEFAULT 0,
    idempotency_key   TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}',
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_operations_idempotency UNIQUE (idempotency_key)
);

CREATE UNIQUE INDEX uq_operations_channel_ref
    ON operations (channel_name, channel_ref)
    WHERE channel_ref != '';

CREATE INDEX idx_operations_holder_class
    ON operations (account_holder, classification_id, status);

CREATE INDEX idx_operations_expires
    ON operations (expires_at)
    WHERE expires_at != 'epoch';
```

```sql
-- 012_operations.down.sql
DROP TABLE IF EXISTS operations;
```

**Step 2: Write 013_events migration**

```sql
-- 013_events.up.sql
CREATE TABLE events (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_code TEXT NOT NULL,
    operation_id        BIGINT NOT NULL DEFAULT 0,
    account_holder      BIGINT NOT NULL DEFAULT 0,
    currency_id         BIGINT NOT NULL DEFAULT 0,
    from_status         TEXT NOT NULL DEFAULT '',
    to_status           TEXT NOT NULL,
    amount              NUMERIC(30,18) NOT NULL DEFAULT 0,
    settled_amount      NUMERIC(30,18) NOT NULL DEFAULT 0,
    journal_id          BIGINT NOT NULL DEFAULT 0,
    metadata            JSONB NOT NULL DEFAULT '{}',
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    delivery_status     TEXT NOT NULL DEFAULT 'pending',
    attempts            INT NOT NULL DEFAULT 0,
    max_attempts        INT NOT NULL DEFAULT 10,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_delivery_pending
    ON events (next_attempt_at)
    WHERE delivery_status = 'pending';

CREATE INDEX idx_events_operation
    ON events (operation_id)
    WHERE operation_id != 0;
```

```sql
-- 013_events.down.sql
DROP TABLE IF EXISTS events;
```

**Step 3: Write 014_journal_event_id migration**

```sql
-- 014_journal_event_id.up.sql
ALTER TABLE journals ADD COLUMN event_id BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_journals_event ON journals (event_id) WHERE event_id != 0;
```

```sql
-- 014_journal_event_id.down.sql
DROP INDEX IF EXISTS idx_journals_event;
ALTER TABLE journals DROP COLUMN IF EXISTS event_id;
```

**Step 4: Write 015_classification_lifecycle migration**

```sql
-- 015_classification_lifecycle.up.sql
ALTER TABLE classifications ADD COLUMN lifecycle JSONB NOT NULL DEFAULT '{}';
```

```sql
-- 015_classification_lifecycle.down.sql
ALTER TABLE classifications DROP COLUMN IF EXISTS lifecycle;
```

**Step 5: Write 016_webhook_subscribers migration**

```sql
-- 016_webhook_subscribers.up.sql
CREATE TABLE webhook_subscribers (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL,
    secret           TEXT NOT NULL DEFAULT '',
    filter_class     TEXT NOT NULL DEFAULT '',
    filter_to_status TEXT NOT NULL DEFAULT '',
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

```sql
-- 016_webhook_subscribers.down.sql
DROP TABLE IF EXISTS webhook_subscribers;
```

**Step 6: Write 017_no_null_cleanup migration**

Adapt existing tables to No NULL convention:

```sql
-- 017_no_null_cleanup.up.sql

-- journals: nullable columns → NOT NULL with defaults
ALTER TABLE journals ALTER COLUMN actor_id SET DEFAULT 0;
UPDATE journals SET actor_id = 0 WHERE actor_id IS NULL;
ALTER TABLE journals ALTER COLUMN actor_id SET NOT NULL;

ALTER TABLE journals ALTER COLUMN source SET DEFAULT '';
UPDATE journals SET source = '' WHERE source IS NULL;
ALTER TABLE journals ALTER COLUMN source SET NOT NULL;

ALTER TABLE journals ALTER COLUMN reversal_of SET DEFAULT 0;
UPDATE journals SET reversal_of = 0 WHERE reversal_of IS NULL;
ALTER TABLE journals ALTER COLUMN reversal_of SET NOT NULL;
-- Drop FK constraint on reversal_of since 0 is not a valid FK target
ALTER TABLE journals DROP CONSTRAINT IF EXISTS journals_reversal_of_fkey;

-- reservations: settled_amount nullable → NOT NULL DEFAULT 0
UPDATE reservations SET settled_amount = 0 WHERE settled_amount IS NULL;
ALTER TABLE reservations ALTER COLUMN settled_amount SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN settled_amount SET DEFAULT 0;

-- reservations: journal_id nullable → NOT NULL DEFAULT 0
UPDATE reservations SET journal_id = 0 WHERE journal_id IS NULL;
ALTER TABLE reservations ALTER COLUMN journal_id SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN journal_id SET DEFAULT 0;
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_journal_id_fkey;

-- rollup_queue: processed_at nullable → status-based approach
ALTER TABLE rollup_queue ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';
UPDATE rollup_queue SET status = 'processed' WHERE processed_at IS NOT NULL;
ALTER TABLE rollup_queue ALTER COLUMN processed_at SET DEFAULT 'epoch';
UPDATE rollup_queue SET processed_at = 'epoch' WHERE processed_at IS NULL;
ALTER TABLE rollup_queue ALTER COLUMN processed_at SET NOT NULL;

-- balance_checkpoints: last_entry_at nullable → NOT NULL DEFAULT epoch
UPDATE balance_checkpoints SET last_entry_at = 'epoch' WHERE last_entry_at IS NULL;
ALTER TABLE balance_checkpoints ALTER COLUMN last_entry_at SET NOT NULL;
ALTER TABLE balance_checkpoints ALTER COLUMN last_entry_at SET DEFAULT 'epoch';
```

```sql
-- 017_no_null_cleanup.down.sql
-- Reverse null cleanup (best effort)
ALTER TABLE journals ALTER COLUMN actor_id DROP NOT NULL;
ALTER TABLE journals ALTER COLUMN source DROP NOT NULL;
ALTER TABLE journals ALTER COLUMN reversal_of DROP NOT NULL;
ALTER TABLE reservations ALTER COLUMN settled_amount DROP NOT NULL;
ALTER TABLE reservations ALTER COLUMN journal_id DROP NOT NULL;
ALTER TABLE rollup_queue DROP COLUMN IF EXISTS status;
ALTER TABLE rollup_queue ALTER COLUMN processed_at DROP NOT NULL;
ALTER TABLE balance_checkpoints ALTER COLUMN last_entry_at DROP NOT NULL;
```

**Step 7: Commit**

```bash
git add postgres/sql/migrations/
git commit -m "feat(postgres): add operations, events tables and no-null migrations

- 012: unified operations table (replaces deposits + withdrawals)
- 013: events table (outbox for event-journal atomicity)
- 014: journals.event_id (causal link event → journal)
- 015: classifications.lifecycle (JSONB state machine definition)
- 016: webhook_subscribers (service mode delivery targets)
- 017: no-null cleanup across existing tables"
```

---

### Task 3.2: Write SQL queries + regenerate sqlc

**Files:**
- Create: `postgres/sql/queries/operations.sql`
- Create: `postgres/sql/queries/events.sql`
- Create: `postgres/sql/queries/webhook_subscribers.sql`
- Modify: `postgres/sql/queries/journals.sql` (add event_id)
- Modify: `postgres/sql/queries/classifications.sql` (add lifecycle, GetByCode)
- Modify: `postgres/sql/queries/checkpoints.sql` (adapt for no-null)

**Step 1: Write operations.sql**

```sql
-- name: InsertOperation :one
INSERT INTO operations (
    classification_id, account_holder, currency_id, amount, status,
    channel_name, idempotency_key, metadata, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetOperation :one
SELECT * FROM operations WHERE id = $1;

-- name: GetOperationForUpdate :one
SELECT * FROM operations WHERE id = $1 FOR UPDATE;

-- name: GetOperationByIdempotencyKey :one
SELECT * FROM operations WHERE idempotency_key = $1;

-- name: UpdateOperationStatus :exec
UPDATE operations
SET status = $2, updated_at = now()
WHERE id = $1;

-- name: UpdateOperationTransition :exec
UPDATE operations
SET status = $2, channel_ref = $3, settled_amount = $4,
    journal_id = $5, metadata = $6, updated_at = now()
WHERE id = $1;

-- name: ListOperationsByFilter :many
SELECT * FROM operations
WHERE (account_holder = $1 OR $1 = 0)
  AND (classification_id = $2 OR $2 = 0)
  AND (status = $3 OR $3 = '')
  AND id > $4
ORDER BY id
LIMIT $5;

-- name: GetExpiredOperations :many
SELECT o.* FROM operations o
JOIN classifications c ON c.id = o.classification_id
WHERE o.expires_at != 'epoch'
  AND o.expires_at < now()
  AND o.status NOT IN (SELECT unnest(
      ARRAY(SELECT jsonb_array_elements_text(c.lifecycle->'terminal'))
  ))
LIMIT $1;
```

**Step 2: Write events.sql**

```sql
-- name: InsertEvent :one
INSERT INTO events (
    classification_code, operation_id, account_holder, currency_id,
    from_status, to_status, amount, settled_amount, journal_id,
    metadata, occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: ListEventsByFilter :many
SELECT * FROM events
WHERE (classification_code = $1 OR $1 = '')
  AND (operation_id = $2 OR $2 = 0)
  AND (to_status = $3 OR $3 = '')
  AND id > $4
ORDER BY id
LIMIT $5;

-- name: GetPendingEvents :many
SELECT * FROM events
WHERE delivery_status = 'pending'
  AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: UpdateEventDelivered :exec
UPDATE events
SET delivery_status = 'delivered', delivered_at = now()
WHERE id = $1;

-- name: UpdateEventRetry :exec
UPDATE events
SET delivery_status = 'pending',
    attempts = attempts + 1,
    next_attempt_at = $2
WHERE id = $1;

-- name: UpdateEventDead :exec
UPDATE events
SET delivery_status = 'dead'
WHERE id = $1;
```

**Step 3: Write webhook_subscribers.sql**

```sql
-- name: InsertWebhookSubscriber :one
INSERT INTO webhook_subscribers (name, url, secret, filter_class, filter_to_status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetWebhookSubscriber :one
SELECT * FROM webhook_subscribers WHERE id = $1;

-- name: ListActiveWebhookSubscribers :many
SELECT * FROM webhook_subscribers WHERE is_active = true;

-- name: DeleteWebhookSubscriber :exec
DELETE FROM webhook_subscribers WHERE id = $1;
```

**Step 4: Update journals.sql**

Add event_id to InsertJournal query. Modify the INSERT to include the new column.

**Step 5: Update classifications.sql**

Add GetClassificationByCode query and include lifecycle in results.

**Step 6: Run sqlc generate**

```bash
cd postgres && sqlc generate
```

Verify generated code compiles:

```bash
cd .. && go build ./postgres/sqlcgen/...
```

**Step 7: Commit**

```bash
git add postgres/sql/queries/ postgres/sqlcgen/
git commit -m "feat(postgres): add sqlc queries for operations, events, subscribers

- operations: CRUD + filter + expired scan
- events: CRUD + delivery polling + retry/dead letter
- webhook_subscribers: CRUD
- journals: add event_id to insert
- classifications: add GetByCode, lifecycle in results"
```

---

## Phase 4: Postgres Adapters

> Implement new stores. Commit: `feat(postgres): implement operation and event stores`

### Task 4.1: Implement OperationStore

**Files:**
- Create: `postgres/operation_store.go`

Implement `core.Operator` + `core.OperationReader`. Follow the pattern in `postgres/deposit_store.go`:
- `CreateOperation`: Load classification by code, validate lifecycle exists, check idempotency, insert with initial status
- `Transition`: Load operation FOR UPDATE, load classification, validate `CanTransition(from, to)`, update status + optional fields, insert event, return event
- `GetOperation`, `ListOperations`: Straightforward queries

Key: `Transition` must insert into both `operations` and `events` in the same transaction.

### Task 4.2: Implement EventStore

**Files:**
- Create: `postgres/event_store.go`

Implement `core.EventReader` + delivery polling methods:
- `GetEvent`, `ListEvents`: Read queries
- `GetPendingEvents`, `UpdateEventDelivered`, `UpdateEventRetry`, `UpdateEventDead`: Delivery worker support

### Task 4.3: Update LedgerStore

**Files:**
- Modify: `postgres/ledger_store.go`

Update `PostJournal` to accept and store `EventID`. Update the sqlcgen insert call to include the new column.

### Task 4.4: Update ClassificationStore

**Files:**
- Modify: `postgres/classification_store.go`

- `Create`: Accept and store lifecycle JSONB
- `GetByCode`: New method
- Map lifecycle JSON to/from `core.Lifecycle`

### Task 4.5: Update ReserverStore for No NULL

**Files:**
- Modify: `postgres/reserver_store.go`

Remove nullable handling for `settled_amount` and `journal_id`. Use 0 defaults.

### Task 4.6: Run build + tests

```bash
go build ./postgres/...
go test ./postgres/... -race -count=1
```

### Task 4.7: Commit

```bash
git add postgres/*.go
git commit -m "feat(postgres): implement operation and event stores

- OperationStore: CreateOperation + Transition (atomic event+state change)
- EventStore: read + delivery polling
- LedgerStore: journal.event_id support
- ClassificationStore: lifecycle JSONB, GetByCode
- ReserverStore: no-null adaptation"
```

---

## Phase 5: Service Layer

> Event delivery + worker updates. Commit: `feat(service): add event delivery and update workers`

### Task 5.1: Create callback deliverer (library mode)

**Files:**
- Create: `service/delivery/callback.go`

```go
package delivery

import (
    "context"
    "github.com/azex-ai/ledger/core"
)

type CallbackDeliverer struct {
    handlers []func(context.Context, core.Event) error
}

func NewCallbackDeliverer() *CallbackDeliverer {
    return &CallbackDeliverer{}
}

func (d *CallbackDeliverer) OnEvent(fn func(context.Context, core.Event) error) {
    d.handlers = append(d.handlers, fn)
}

func (d *CallbackDeliverer) Deliver(ctx context.Context, event core.Event) error {
    for _, h := range d.handlers {
        if err := h(ctx, event); err != nil {
            return fmt.Errorf("delivery: callback: %w", err)
        }
    }
    return nil
}
```

### Task 5.2: Create webhook deliverer (service mode)

**Files:**
- Create: `service/delivery/webhook.go`

Worker that:
1. Polls `events` table for `delivery_status = 'pending'`
2. Loads active webhook_subscribers
3. Matches subscriber filters against event
4. HTTP POST with HMAC signature
5. Updates delivery_status (delivered/retry/dead)
6. Exponential backoff: 1m, 5m, 30m, 2h, 24h

### Task 5.3: Update ExpirationService

**Files:**
- Modify: `service/expiration.go`

Replace `ExpireStaleDeposits()` and `ExpireStaleWithdrawals()` with a single `ExpireStaleOperations()` that queries the unified `operations` table.

### Task 5.4: Update Worker

**Files:**
- Modify: `service/worker.go`

Add event delivery loop (configurable interval, e.g., 5s). Replace deposit/withdrawal expiration calls with unified operation expiration.

### Task 5.5: Commit

```bash
git add service/
git commit -m "feat(service): add event delivery and update workers

- delivery/callback.go: sync callback for library mode
- delivery/webhook.go: async HTTP delivery with retry + dead letter
- expiration: unified operation expiration (replaces deposit+withdrawal)
- worker: add event delivery loop"
```

---

## Phase 6: HTTP Server

> New handlers + route updates. Commit: `feat(server): unified operation handlers and webhook endpoints`

### Task 6.1: Create handler_operations.go

**Files:**
- Create: `server/handler_operations.go`

Replaces `handler_deposits.go` + `handler_withdrawals.go`. Endpoints:
- `POST /api/v1/operations` → CreateOperation
- `POST /api/v1/operations/{id}/transition` → Transition
- `GET /api/v1/operations/{id}` → GetOperation
- `GET /api/v1/operations` → ListOperations (filter by classification, status, holder)

Follow existing handler patterns (decode request, call store, write response).

### Task 6.2: Create handler_webhooks.go

**Files:**
- Create: `server/handler_webhooks.go`

- `POST /api/v1/webhooks/{channel}` → Receive channel callback, verify signature, parse, transition

### Task 6.3: Create handler_events.go

**Files:**
- Create: `server/handler_events.go`

- `GET /api/v1/events` → ListEvents
- `GET /api/v1/events/{id}` → GetEvent

### Task 6.4: Create handler_subscribers.go

**Files:**
- Create: `server/handler_subscribers.go`

- `POST /api/v1/subscribers` → Create
- `GET /api/v1/subscribers` → List
- `DELETE /api/v1/subscribers/{id}` → Delete

### Task 6.5: Update routes.go and server.go

**Files:**
- Modify: `server/routes.go`
- Modify: `server/server.go`

Remove deposit/withdrawal routes. Add operation/webhook/event/subscriber routes. Update Server struct dependencies (replace Depositor/Withdrawer with Operator, add EventReader, channel adapters map).

### Task 6.6: Remove old handlers

**Files:**
- Delete: `server/handler_deposits.go`
- Delete: `server/handler_withdrawals.go`

### Task 6.7: Commit

```bash
git add server/
git commit -m "feat(server): unified operation handlers and webhook endpoints

- handler_operations: replaces deposit + withdrawal handlers
- handler_webhooks: inbound channel adapter callbacks
- handler_events: event query endpoints
- handler_subscribers: webhook subscriber management
- Removed: handler_deposits.go, handler_withdrawals.go"
```

---

## Phase 7: Wiring + Cleanup

> Update main.go, remove old code. Commit: `refactor: wire v2 architecture and remove v1 deposit/withdrawal code`

### Task 7.1: Update main.go

**Files:**
- Modify: `cmd/ledgerd/main.go`

Replace DepositStore/WithdrawStore with OperationStore. Wire EventStore, delivery, channel adapters. Update Server constructor.

### Task 7.2: Remove old core types

**Files:**
- Delete: `core/deposit.go`
- Delete: `core/withdraw.go`

### Task 7.3: Remove old postgres stores

**Files:**
- Delete: `postgres/deposit_store.go`
- Delete: `postgres/withdraw_store.go`

### Task 7.4: Remove old SQL queries

**Files:**
- Delete: `postgres/sql/queries/deposits.sql`
- Delete: `postgres/sql/queries/withdrawals.sql`

Regenerate sqlc:

```bash
cd postgres && sqlc generate
```

### Task 7.5: Build + test

```bash
go build ./...
go test ./... -race -count=1
```

### Task 7.6: Commit

```bash
git add -A
git commit -m "refactor: wire v2 architecture and remove v1 deposit/withdrawal code

- main.go: OperationStore + EventStore + delivery wiring
- Removed: core/deposit.go, core/withdraw.go
- Removed: postgres/deposit_store.go, postgres/withdraw_store.go
- Removed: deposits.sql, withdrawals.sql queries
- Regenerated sqlc"
```

---

## Phase 8: Tests

> Integration tests for the new architecture. Commit: `test: add operation, event, and channel adapter tests`

### Task 8.1: Core unit tests

**Files:**
- Create: `core/lifecycle_test.go`

Test Lifecycle.Validate(), CanTransition(), IsTerminal() with table-driven tests.

### Task 8.2: Operation store integration tests

**Files:**
- Create: `postgres/operation_store_test.go`

Using testcontainers (follow existing `postgres/*_test.go` patterns):
- CreateOperation with valid lifecycle
- CreateOperation idempotency
- Transition happy path (deposit: pending → confirming → confirmed)
- Transition invalid state → ErrInvalidTransition
- Transition produces Event atomically
- Event.JournalID links to journal when caller posts journal in same tx

### Task 8.3: Event store integration tests

**Files:**
- Create: `postgres/event_store_test.go`

- GetPendingEvents returns only undelivered
- UpdateEventDelivered marks as delivered
- UpdateEventRetry increments attempts
- ListEvents with filters

### Task 8.4: Channel adapter unit tests

**Files:**
- Create: `channel/onchain/evm_test.go`

- VerifySignature with valid/invalid signatures
- ParseCallback with valid/invalid payloads

### Task 8.5: Preset validation tests

**Files:**
- Create: `presets/presets_test.go`

- DepositLifecycle.Validate() passes
- WithdrawalLifecycle.Validate() passes
- DepositLifecycle transitions are correct
- WithdrawalLifecycle retry path works (failed → reserved)

### Task 8.6: Run full test suite

```bash
go test ./... -race -count=1
```

### Task 8.7: Commit

```bash
git add -A
git commit -m "test: add operation, event, and channel adapter tests

- core/lifecycle_test.go: lifecycle validation and transitions
- postgres/operation_store_test.go: CRUD + transition + atomicity
- postgres/event_store_test.go: delivery polling + status updates
- channel/onchain/evm_test.go: signature + parsing
- presets/presets_test.go: preset lifecycle validation"
```

---

## Phase 9: Documentation

> Update CLAUDE.md and docs. Commit: `docs: update for v2 classification-driven architecture`

### Task 9.1: Update CLAUDE.md

Reflect new package structure (`presets/`, `channel/`), new workflow (operations instead of deposits/withdrawals), updated file layout table.

### Task 9.2: Update docs/api.md

Replace deposit/withdrawal endpoints with unified operations endpoints. Add webhook, event, subscriber endpoints.

### Task 9.3: Commit

```bash
git add CLAUDE.md docs/
git commit -m "docs: update for v2 classification-driven architecture"
```

---

## Summary

| Phase | Commit | Key Changes |
|-------|--------|-------------|
| 1 | `feat(core)` | Lifecycle, Operation, Event types + Operator interface |
| 2 | `feat(presets+channel)` | Deposit/Withdrawal presets + EVM adapter |
| 3 | `feat(postgres/schema)` | Migrations: operations, events, no-null |
| 4 | `feat(postgres/stores)` | OperationStore, EventStore implementations |
| 5 | `feat(service)` | Event delivery + worker updates |
| 6 | `feat(server)` | Unified operation handlers + webhook endpoints |
| 7 | `refactor(wiring)` | main.go + remove old deposit/withdrawal code |
| 8 | `test` | Integration + unit tests |
| 9 | `docs` | CLAUDE.md + API docs |
