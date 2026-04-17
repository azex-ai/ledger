# Phase 4: HTTP API Server

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the chi v5 HTTP API that exposes all ledger functionality as a REST service. Cursor-based pagination everywhere.

**Architecture:** Thin handlers → service layer → store. Handlers do request validation + JSON serialization only. Error responses use consistent format `{"error": {"code": "...", "message": "..."}}`.

**Tech Stack:** Go 1.25+, chi v5, encoding/json

**Depends on:** Phase 1 (core), Phase 2 (stores), Phase 3 Tasks 1-4 (services)

**Can parallelize with:** Phase 5 (frontend — after Task 1-3 here)

---

### Task 1: Server Skeleton + Health Endpoint

**Files:**
- Create: `server/server.go`
- Create: `server/routes.go`
- Create: `server/response.go`
- Create: `cmd/ledgerd/main.go`
- Test: `server/server_test.go`

**Step 1: Write server.go**

```go
// server/server.go
package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/azex-ai/ledger/core"
)

type Server struct {
	router  chi.Router
	engine  *core.Engine
	// stores injected here
}

type Config struct {
	Port string
}

func New(engine *core.Engine, cfg Config) *Server {
	s := &Server{engine: engine}
	s.router = chi.NewRouter()
	s.router.Use(middleware.Logger)
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.RequestID)
	s.setupRoutes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
```

**Step 2: Write response helpers**

```go
// server/response.go — JSON, Error, Paginated response helpers
// Cursor pagination: decode/encode cursor from ?cursor= param
```

**Step 3: Write health endpoint + test**

```go
// GET /api/v1/system/health → returns rollup queue depth, checkpoint age, active reservations
```

**Step 4: Write main.go entry point**

```go
// cmd/ledgerd/main.go — wire pgxpool, run migrations, create stores, create engine, start server + worker
```

**Commit:** `feat(server): add server skeleton, health endpoint, and ledgerd main entry`

---

### Task 2: Journal Endpoints

**Files:**
- Create: `server/handler_journals.go`
- Test: `server/handler_journals_test.go`

**Endpoints:**
```
POST   /api/v1/journals              — manual journal posting
POST   /api/v1/journals/template     — template-based posting
POST   /api/v1/journals/:id/reverse  — reversal
GET    /api/v1/journals/:id          — get journal + entries
GET    /api/v1/journals              — list (cursor pagination)
GET    /api/v1/entries               — entry-level query (holder/currency/cursor)
```

**Tests:**
- POST balanced journal → 201 + journal returned
- POST unbalanced → 400 + error message
- POST duplicate idempotency key → 200 + same journal
- POST template → 201 + entries match template
- POST reverse → 201 + reversal_of set
- GET with cursor pagination → correct ordering
- GET entries by account → filtered correctly

**Commit:** `feat(server): add journal + entry API endpoints`

---

### Task 3: Balance Endpoints

**Files:**
- Create: `server/handler_balances.go`
- Test: `server/handler_balances_test.go`

**Endpoints:**
```
GET    /api/v1/balances/:holder            — all balances for holder
GET    /api/v1/balances/:holder/:currency  — per-currency breakdown
POST   /api/v1/balances/batch              — batch query (holder IDs array)
```

**Tests:**
- Single holder balance after journal → correct amounts
- Batch query → returns map of holder → balances
- Non-existent holder → empty result, not error

**Commit:** `feat(server): add balance API endpoints with batch query`

---

### Task 4: Reservation Endpoints

**Files:**
- Create: `server/handler_reservations.go`
- Test: `server/handler_reservations_test.go`

**Endpoints:**
```
POST   /api/v1/reservations                — create
POST   /api/v1/reservations/:id/settle     — settle
POST   /api/v1/reservations/:id/release    — release
GET    /api/v1/reservations                — list (status filter)
```

**Commit:** `feat(server): add reservation API endpoints`

---

### Task 5: Deposit + Withdrawal Endpoints

**Files:**
- Create: `server/handler_deposits.go`
- Create: `server/handler_withdrawals.go`
- Test: `server/handler_deposits_test.go`
- Test: `server/handler_withdrawals_test.go`

**Endpoints:** As specified in design doc Section 7.

**Tests must cover:**
- Full deposit lifecycle via API
- Full withdrawal lifecycle via API (with and without review)
- Channel ref idempotency
- Invalid state transition → 409 Conflict

**Commit:** `feat(server): add deposit + withdrawal API endpoints with state machine`

---

### Task 6: Metadata Endpoints (Classifications, JournalTypes, Templates, Currencies)

**Files:**
- Create: `server/handler_metadata.go`
- Test: `server/handler_metadata_test.go`

**Endpoints:**
```
# Classifications, JournalTypes, Templates, Currencies — CRUD as specified
# Templates include GET /:code/preview (dry-run rendering)
```

**Tests:**
- CRUD for each entity
- Template preview → returns rendered entries without persisting
- Deactivated entity → excluded from active-only list

**Commit:** `feat(server): add metadata CRUD + template preview endpoints`

---

### Task 7: Reconciliation + Snapshot + System Endpoints

**Files:**
- Create: `server/handler_system.go`
- Test: `server/handler_system_test.go`

**Endpoints:**
```
POST   /api/v1/reconcile              — run global reconciliation
POST   /api/v1/reconcile/account      — run per-account reconciliation
GET    /api/v1/snapshots              — query by date range
GET    /api/v1/system/balances        — system rollup balances
GET    /api/v1/system/health          — operational health
```

**Commit:** `feat(server): add reconciliation, snapshot, and system endpoints`
