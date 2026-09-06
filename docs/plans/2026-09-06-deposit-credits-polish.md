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

## Push-time CI follow-up

The first pushed commit `838476f` exposed two additional integration failures:

- React typecheck imported the host navigation, whose package self-import needed
  generated `dist` declarations. The package TypeScript config now resolves its
  own import to source; typecheck passes with `dist` temporarily absent. The host
  build still consumes the package exports. Package build, all 241 tests and the
  root web ESLint check pass after this correction.
- Root vulnerability scanning now reaches the PostgreSQL fixture's testcontainers
  SSH dependency. `GO-2026-6354` and `GO-2026-6355` identify `x/crypto v0.54.0`
  as affected and `v0.56.0` as fixed. All four module manifests now select
  `v0.56.0`; root `x/text` also moves to the required `v0.41.0`.

These are follow-up fixes prompted by actual Linux CI evidence; the earlier local
results did not establish that the clean CI checkout would pass.

After the dependency update, local `govulncheck` passes for root, EVM and R2 using
the same Go 1.26.8 toolchain as CI; core and credits tests and the external consumer
build also pass. Remote CI status is recorded in the gpv session summary.

## Independent review and individual acceptance — 2026-09-07

The user requested three independent agents, an explicit plan, then individual
acceptance. Review started at `55829d9`; the lead accepted money, external Go
consumption, then shadcn UI after each required fix was reproduced and reviewed.
Bus tasks: consumer #16, UI #17, money #19; lead acceptance #18. Detailed local
reports and browser captures are in `.team/reviews/credits-20260907-*`.

| Item | Finding / change | Acceptance evidence |
|---|---|---|
| Money: reservation replay | Transaction `now()` and Go current time produced different creation/expiry bases, so a 1.2s transaction delay caused unchanged TTL to conflict. Insert now persists both timestamps from one clock reading | `TestReserveReplayAfterTransactionDelay`: same UID on unchanged retry, changed TTL conflicts, hold remains exactly 10; independent reviewer reran with race detection |
| Money: composed lock order | Reserve acquired user/USDC before the FX batch's system pair, deadlocking with ordinary deposit. `LockForTemplates` reuses the existing sorted idempotency/balance lock mechanism before Reserve | `TestPurchaseConcurrentDepositLockOrder`: deterministic barrier with real purchase/deposit calls succeeds; balances 1 USDC / 1000 credits, no holds. Tx-only/no-write guard test also passes |
| Money: existing refund capability | Document `ReverseJournalFraction`, cumulative per-entry caps, concurrent/replay protection and 1/1 remainder completion | Existing conservation/remainder, overshoot, replay and concurrency tests passed in independent review; host eligibility/approval remains a policy decision |
| Go consumer | Root and paired EVM baseline SHA download and compile outside all local workspaces/replaces; current local source also passes fresh consumer check | Official proxy root and root+EVM go get/tidy/build passed; `make test-consumer` passes and excludes Docker/testcontainers from production imports |
| Go docs and fixture | Correct outdated cash-out, module/import and simulated-deposit claims; bounded cleanup reports errors. README generated programs now use system temp directories to avoid races with source scanners | Fixture CRUD race test and static gates passed; final root/core run together passes, including README runtime/compile and API snapshot checks |
| shadcn large amounts | Use card container width for total font size, constrain amount button and allow currency wrapping | Real 320px browser: amount right 288 < card right 304, button scrollWidth=clientWidth=256; 640px dual-column and exact tooltip also pass |
| shadcn deposit errors | Fullstack wallet mounts Toaster; update component props/host requirements | Actual wallet route GET 404 → generate POST 500 displays a visible error notification in the packed standalone host |

- [x] Each lane delivered its report; its required fixes were reviewed again.
- [x] Final `make test`: entire root module passes `-race -count=1`, including
  PostgreSQL (178.304s), service (48.420s), root (37.784s), core and all 11 credits
  scenario tests. Log: `/tmp/ledger-gpv-go-final-20260907.log`.
- [x] `go vet ./...`, affected-package golangci-lint (zero issues), sqlc generation
  and current external consumer build pass.
- [x] React typecheck, package build and 42 files / 241 tests pass. The rebuilt
  tarball (`e12c77646c0b83cd04206c6fbe0ef2dc9af8dd78`) installs/builds in the
  independent Next.js 16.3.4 host; browser evidence verifies the corrected state.
- [x] HeroUI source and package versions remain untouched; no release tag or npm
  publication was requested. Push-time CI is recorded in the gpv session summary.

The timestamp correction applies to newly created reservations. Old rows already
containing more than one second of clock drift cannot reveal the original TTL;
this change neither guesses it nor widens conflict tolerance. Host events must
bind their ID to an immutable amount and operation kind before calling the
example: separate charge/release receipt keys do not deduplicate a changed
provider event. Both limits were explicitly accepted as documented boundaries.

Generic delivery-gate results remain qualified: the genuine fixture cleanup issue
is fixed; FNV writes cannot return an error, terminal HTTP response writes cannot
undo committed bookkeeping, and `LoadConfig`, example entrypoints and test
fixtures are valid environment-reading boundaries. Directory/string scans do not
replace the executed domain tests. Optional pagination error presentation is not
a required fix in this scope.

Memory: added a learning seed for transaction-wide lock order and timestamp
composition; updated the stale project lock-key memory. `hive doctor --strict`
passes. Session: `~/.claude/session-summaries/2026-09-07-02-00.md`.
