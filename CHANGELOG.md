# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two independently versioned artifacts live in this repo:

- the **Go module** `github.com/azex-ai/ledger`, tagged `vX.Y.Z`;
- the **npm package** `@azex/ledger-react`, tagged `ledger-react-vX.Y.Z`.

Entries below note which artifact a change affects.

## [Unreleased]

## [0.2.0] - 2026-07-02

### Go module — Added
- `Reserver.HeldAmount(ctx, holder, currencyID)` — returns the sum of the holder's
  active reservations in a currency (the figure Reserve subtracts from balance to
  check availability). Consumers can now derive `available = balance − held`
  through the interface instead of querying the `reservations` table directly.

### Documentation
- `docs/COOKBOOK.md` — business recipes: buy credits at a fixed rate (FX two-leg),
  discounts (price / bonus / promo), adding currencies, reserve→settle spend,
  cash-out, and expiry/insufficient-funds edges.
- `examples/credits-topup` — runnable end-to-end program for the above.

### Build / CI
- Toolchain aligned to latest: golangci-lint **v2.12.2** (v1.62 was built with
  Go 1.23 and could not load Go 1.26 projects), sqlc **1.31.1**, CI Go **1.26.x**,
  Docker base `golang:1.26-alpine`. Added `.golangci.yml`.
- Fixed `docker-build`: the main module's `replace` of the test-only
  `internal/postgrestest` submodule now resolves in the builder (its `go.mod` is
  allowed through `.dockerignore` and copied before `go mod download`).
- Cleared pre-existing lint debt surfaced once golangci-lint could finally run.

## [0.1.0] - 2026-07-02

First tagged release. Establishes the public consumption contract for both the
Go library and the React package. API is **v0.x** — no stability guarantees
between minor versions while under active development (see SemVer policy in
`README.md`).

### Go module — Added
- Root `ledger` facade: `ledger.New(pool, ...Option)` returns a `*ledger.Service`
  exposing `core` interfaces (`Booker`, `BalanceReader`, `JournalWriter`,
  `Reserver`, `EventReader`, …) — consumers depend only on `core/*`, never on the
  `postgres` adapter directly.
- `Service.RunInTx` composes ledger writes with the caller's own DB writes in one
  atomic pgx transaction.
- `ledger.Migrate(databaseURL)` runs the embedded schema migrations.
- `WithLogger` / `WithMetrics` options for injecting observability; both optional.
- Preset bundles installable via `InstallDefaultPresets` / `InstallExtendedPresets`
  (deposit, withdrawal, transfer, fee, capital, settlement, card, spread, FX).
- Inbound channel adapters via `RegisterChannel`; background jobs via `Worker`.
- Standalone HTTP service `cmd/ledgerd` and read-only investigation CLI
  `cmd/ledger-cli`.

### npm package `@azex/ledger-react` — Added
- Initial release: hooks, page components, RSC prefetch helpers, and theming for
  consuming the ledger HTTP API. Entry points `.`, `./charts`, `./server`,
  `./styles.css`.
- Published to the public npm registry.

[Unreleased]: https://github.com/azex-ai/ledger/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/azex-ai/ledger/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/azex-ai/ledger/releases/tag/v0.1.0
