# azex-ai/ledger

Production-grade double-entry ledger engine for Go.

## Features

- **Double-entry accounting** -- every journal enforces `total_debit = total_credit` at the database level
- **Dynamic classifications** -- create and deprecate account classifications at runtime, no code changes
- **Entry templates** -- define reusable debit/credit recipes, call with parameters
- **Checkpoint + delta balances** -- materialized checkpoints with incremental rollup for fast reads
- **Reserve / Settle / Release** -- pessimistic locking with advisory locks for concurrent safety
- **Deposit state machine** -- `pending -> confirming -> confirmed` with channel abstraction
- **Withdrawal state machine** -- `locked -> reserved -> reviewing -> processing -> confirmed` with optional review
- **Reconciliation engine** -- accounting equation verification and per-account drift detection
- **Daily snapshots** -- historical balance snapshots for reporting
- **System rollups** -- aggregated balances across all accounts by classification
- **Idempotent writes** -- every mutation requires an idempotency key, duplicates return the original result
- **Async rollup worker** -- background checkpoint materialization with `SKIP LOCKED` queue

## Quick Start -- As a Library

**1. Install**

```bash
go get github.com/azex-ai/ledger@latest
```

**2. Connect and migrate**

```go
pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
postgres.Migrate("pgx5://user:pass@host/db?sslmode=disable")
```

**3. Use**

```go
ledgerStore := postgres.NewLedgerStore(pool)
classStore  := postgres.NewClassificationStore(pool)
currStore   := postgres.NewCurrencyStore(pool)
tmplStore   := postgres.NewTemplateStore(pool)

// Post a journal using a template
journal, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
    HolderID:       userID,
    CurrencyID:     usdtID,
    IdempotencyKey: txHash,
    Amounts:        map[string]decimal.Decimal{"amount": amount},
    Source:         "deposit",
})

// Query balance
balance, err := ledgerStore.GetBalance(ctx, userID, usdtID, mainWalletClassID)
```

## Quick Start -- As a Service

```bash
git clone https://github.com/azex-ai/ledger.git
cd ledger
docker compose up --build
```

- API: http://localhost:8080/api/v1/system/health
- Frontend: http://localhost:3000

## Architecture

```
ledger/
  core/               Pure domain layer (zero external dependencies)
    types.go           Currency, Classification, JournalType, Balance
    journal.go         Journal, Entry, JournalInput + validation
    template.go        EntryTemplate, Render()
    reserve.go         Reservation state machine
    deposit.go         Deposit state machine
    withdraw.go        Withdrawal state machine
    checkpoint.go      BalanceCheckpoint, RollupQueueItem, BalanceSnapshot
    interfaces.go      All store interfaces (consumer-side)
    engine.go          Engine with Logger + Metrics injection

  postgres/            pgx v5 + sqlc adapter (only officially supported DB)
    sql/migrations/    Schema migrations (embed.FS)
    sqlcgen/           Generated query code
    ledger_store.go    JournalWriter + BalanceReader
    reserver_store.go  Reserver (advisory locks)
    deposit_store.go   Depositor
    withdraw_store.go  Withdrawer
    ...

  service/             Business orchestration
    rollup.go          Async checkpoint materialization
    reconcile.go       Accounting equation + per-account verification
    snapshot.go        Daily balance snapshots
    worker.go          Background worker loop

  server/              HTTP API (chi v5)
    routes.go          All endpoint definitions
    handler_*.go       Request handlers by resource

  web/                 Next.js management dashboard

  examples/
    crypto-deposit/    Full EVM CREATE2 deposit demo

  cmd/ledgerd/         Service entry point
```

Account dimensions are fixed at three: `(AccountHolder, CurrencyID, ClassificationID)`.
Positive holder IDs are users; negative IDs are system counterparts (`-userID`).

PostgreSQL is the only officially supported database. The `core/` package defines interfaces; `postgres/` is the default implementation.

For the full design document, see [docs/plans/2026-04-17-design.md](docs/plans/2026-04-17-design.md).

## API Reference

See [docs/api.md](docs/api.md) for the complete HTTP API documentation with request/response examples.

All list endpoints use cursor-based pagination (`?cursor=...&limit=50`).
Error responses use a consistent envelope: `{"error": {"code": "...", "message": "..."}}`.

## Examples

- [**crypto-deposit**](examples/crypto-deposit/) -- Full EVM CREATE2 deposit lifecycle: metadata setup, deposit state machine, template-based journaling, reserve/settle, balance queries, and reconciliation.

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `HTTP_PORT` | HTTP server listen port | `8080` |
| `ROLLUP_INTERVAL` | Rollup worker tick interval | `5s` |
| `ROLLUP_BATCH_SIZE` | Max rollup items per batch | `100` |
| `RESERVATION_EXPIRE_DURATION` | Default reservation TTL | `15m` |
| `RECONCILE_CRON` | Reconciliation schedule (cron) | `0 */6 * * *` |
| `SNAPSHOT_CRON` | Daily snapshot schedule (cron) | `0 2 * * *` |
| `WITHDRAW_REVIEW_THRESHOLD` | Amount above which withdrawals require review | `0` (disabled) |

## License

MIT
