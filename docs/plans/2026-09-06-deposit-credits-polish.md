# Deposit + AI credits integration

Scope: an importable Go ledger and replaceable Next.js/shadcn components. The
current product accepts crypto deposits and sells 1,000 credits for 1 USDC.
Withdrawals and credit cash-out are outside this integration. Existing generic
library capabilities remain available to other consumers; HeroUI is not polished
in this work.

## Pipeline

Input: current ledger, presets, credits/fullstack examples and React exports →
Output: corrected consumer examples, focused library/UI fixes and business tests →
Handoff: runnable Go and shadcn integration with explicit host responsibilities →
Gate/Review: PostgreSQL money-flow tests, Go build/vet, package tests/typecheck/build,
rendered shadcn checks and a requirement-to-evidence review →
Memory: record uncovered business decisions separately; only send new reusable
failure modes back to Hive.

## Acceptance and evidence (verified 2026-09-06–07)

- [x] Go library consumption works outside this workspace; production imports
  exclude Docker/testcontainers (test dependencies remain in module metadata).
- [x] Confirmed deposit is the source of USDC; pending/failed deposits do not issue
  spendable credits. Purchase atomically exchanges 1 USDC for 1,000 credits.
- [x] Credits example uses deposit + FX + consumption primitives, with stable
  keys and no cash-out path; replay of completed operations does not mint or
  charge again. Interrupted jobs whose holds expire require host reconciliation.
- [x] Fixed, metered and incremental usage charges debit the correct currency;
  zero-cost/failure releases, overspend, expiry, retries, conflicting replay,
  rollback and concurrent budgets have reproducible tests.
- [x] Go accounting remains balanced per currency, holds are accounted for, and
  no example assumes Settle alone debits a wallet or balance validates an FX rate.
- [x] Default shadcn demo follows the deposit-only product scope; package exports,
  headless hooks and component replacement work for an external Next.js host.
- [x] User UI exposes exact wire amounts and honest loading/error/empty states.
  Holder transactions work against the running backend; deposit-address behavior
  has component/API coverage and EVM deposit/sweep has local-chain coverage.
  Production chain configuration and a browser-to-chain funding run remain host
  integration work, explicitly recorded in the gaps document.
- [x] Missing product policy (pricing versions, rounding, promotions/expiry,
  refunds, provider retry/usage reconciliation) is recorded without new modules.

## Journal trace

This repository uses debit-normal `main_wallet` and credit-normal system accounts.

1. Confirmed deposit, USDC: DR user.main_wallet 1 / CR system.custodial 1.
2. Purchase, one transaction: reserve and settle 1 USDC; USDC journal DR
   system.settlement 1 / CR user.main_wallet 1; CREDITS journal DR user.main_wallet
   1000 / CR system.settlement 1000. The host derives the rate; the ledger only
   enforces per-currency balance.
3. Usage: Reserve holds a maximum, with no journal. In one transaction Settle (or
   SettlePartial for an incremental usage event) and DR system.settlement N /
   CR user.main_wallet N in CREDITS. A partial event is a delta, not a cumulative
   token counter. Finalize/Release write no additional charge.
4. Failed/zero-cost work: Release only. Reversing a consumption journal restores
   credits, and does not send USDC onchain.

## Resolved findings

- Cookbook incorrectly says an invalid rate is rejected by balancing; existing
  `TestFX_LedgerDoesNotCheckTheRate` explicitly establishes the opposite.
- Cookbook creates settlement and charge keys inside the transaction callback,
  teaching unsafe replay; default raw journals do not guarantee a balance floor.
- Credits example used USDT/100 with bonus and cash-out; random restart keys
  broke its hardcoded final-balance assertion on a second run. It now exercises
  1 USDC → 1,000 credits and ends at exactly 912.875 credits with zero held.
- A separate internal PostgreSQL fixture used an unpublished zero version.
  Workspace builds hid the resulting external-consumer `go mod tidy` failure.
  It is now a root internal package; a fresh consumer build runs in CI.
- Empty wallet states discarded funding actions; multi-currency actions lacked
  currency context; the panel did not expose its children’s replacement options.
  Actions now receive the current balance (or null), with render and region slots.
- Rounded wallet amounts hid fractional credits. Exact amounts are now exposed
  by an accessible, touch/keyboard-operable tooltip while retaining display bands.
- The fullstack host had Tailwind utility classes without a Tailwind setup.
  Plain host CSS now supplies page height and responsive wallet layout.

## Validation record

| Check | Result and scope |
|---|---|
| `make test-consumer` | Fresh external module, `GOWORK=off`, facade/core/presets imports: tidy/build pass; production import graph excludes Docker/testcontainers |
| `go build ./...`, `go vet ./...` | Pass after the fixture module change |
| Root `go test ./... -race -count=1` | Full run passed before fixture consolidation; the later delivery-gate test run passed all non-root packages, with one root README snippet-format failure. That snippet was corrected from `go` to `gomod`; `go test . -count=1` then passed. No accounting test failure remains |
| `go test ./examples/credits-topup -race -count=1` | Eight PostgreSQL tests pass, including pending/cancelled deposits, atomic FX rollback, precision, expiry, replay conflicts, concurrent budgets, partial charges and cancellation cleanup |
| Targeted `golangci-lint` | Credits example and PostgreSQL fixture: zero issues |
| EVM `go test -tags e2e -race -timeout 5m -count=1 -run TestE2E ./...` | Pass with local anvil; tests chain deposit decoding and sweep execution, not a production network |
| React package `npm run typecheck`, `npm test`, `npm run build` | Pass; 241 tests across 42 files, including empty funding actions, per-currency actions, slot replacement without unwanted queries, cursor pagination, exact values and navigation filtering |
| Packed package → standalone Next.js host | Local npm tarball installed into `examples/fullstack/web`; production build passes. Package manifest remains at the existing version; nothing was published |
| Root dogfood `npm run build` | Pass; no withdrawal route. Stale generated Next dev types referencing the removed page were cleared before rebuilding |
| Browser against local Go API/PostgreSQL | Desktop 1920px and mobile 390px wallet rendered; mobile scroll width equals viewport width. Tap reveals `1000.091234 CREDITS`; USDC/CREDITS and holder activity remain separate. Desktop/mobile admin nav excludes Withdrawals. Missing live deposit configuration displays unavailable |
| `git diff --check` | Pass |

The generic delivery-gate script is **not reported green**: beyond the resolved
README compilation failure, its text scans flag existing ignored writes/fixture
cleanup, environment reads in `server.LoadConfig` and the test fixture, a message
that explicitly rejects float64 amounts, and intentional domain/SQL model names.
Those findings were reviewed as existing cleanup/configuration/style concerns,
not evidence of a new accounting failure; this work does not rewrite those
unrelated paths to satisfy the generic scanner.

The browser used isolated local fixtures, including a fractional credits balance
for display verification. It did not demonstrate automatic onchain-deposit-to-
purchase delivery. That durable host workflow, deployed network/token settings,
pricing, usage reconciliation and refund policy are tracked in
[deposit/credits gaps](../gaps/deposit-credits.md).

Memory: recorded the observed workspace/tarball consumer-boundary failures in
Hive `learnings/26-09-07-ledger-consumer-boundary.md`; `hive doctor --strict`
passed. No Hive rule promotion or role change was made.
