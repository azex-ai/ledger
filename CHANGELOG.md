# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two artifacts live in this repo:

- the **Go module** `github.com/azex-ai/ledger`, tagged `vX.Y.Z`;
- the **npm package** `@azex/ledger-react`, tagged `ledger-react-vX.Y.Z`.

From 0.5.0 onward the two are **version-aligned**: a coordinated release
tags both at the same X.Y.Z (npm 0.4.0 / 0.5.0 were never published; the
npm line jumps 0.3.0 → 0.5.1 to converge with the Go module). Entries
below note which artifact a change affects.

## [Unreleased]

Second-round audit remediation (2026-09-02,
[`docs/plans/2026-09-02-remediation-contracts.md`](docs/plans/2026-09-02-remediation-contracts.md)):
six Critical findings, all merged to main (Wave 1); Major/Minor findings
(Wave 2) in progress. Same no-compat premise as the first round (Aaron's
2026-09-02 ruling, carried from `61177f9`): breaking changes ship directly,
no compatibility shims, no dual-read — every one below says what a consumer
needs to do. Full detail for any entry:
[`docs/audits/2026-09-02-deep-audit/TODO.md`](docs/audits/2026-09-02-deep-audit/TODO.md)
(each finding's `file:line`, minimal repro, and fix) and that TODO's own
"破坏性变更清单" / "Lead 追加" tables, which this section transcribes.

Also landed since `[0.6.0]`, not previously logged: `anchors/r2` (Cloudflare
R2 + Object Lock `core.Anchor`) and `anchortest.RunConformance` (I-48) — see
the **anchors/r2** entry near the bottom of this section. `[0.6.0]`'s "known
open items" note above is now partially closed by this; that note is left as
written because it was true when `[0.6.0]` shipped.

### Go module — Breaking

- **`core.HolderReader.ListHolderHolds` is paginated and
  `core.JournalQuerier` gains `ListRecentJournals`** (H-m9, D-tamper).
  `ListHolderHolds(ctx, holder)` becomes
  `ListHolderHolds(ctx, holder, cursor string, limit int32) ([]HolderHold,
  string, error)` — the unpaginated form returned a holder's entire hold set,
  an unbounded body from an unbounded scan. `JournalQuerier` gains
  `ListRecentJournals(ctx, limit) ([]Journal, error)`, a NEWEST-FIRST sample
  that attestation verification and reconciliation both need: the ascending
  `ListJournals` page they read before could never contain a freshly forged
  row. Self-built implementations of either interface must add the method
  (`postgres.LedgerStore` / `postgres.NewQueryAdapter` users need no change).
  Both were missing from this list until the breaking-change gate learned to
  diff interface method sets (M11).

- **`Migrate` / `MigrateContext` and the period-close barrier** (D-lock:
  B-M5, B-m4, B-X1, B-m9). `ledger.Migrate(databaseURL)` becomes
  `ledger.Migrate(databaseURL, opts ...postgres.MigrateOption)` and gains
  `ledger.MigrateContext(ctx, databaseURL, opts...)` — source-compatible
  (variadic), no caller changes needed. The cluster migration lock is now a
  polled `pg_try_advisory_lock` with a five-minute default budget
  (`postgres.WithMigrateLockBudget`) and a per-attempt `Info` line
  (`postgres.WithMigrateLogger`, falling back to `slog.Default()`) instead
  of an unbounded, silent, uncancellable wait. `PeriodCloseStore.ClosePeriod`
  now runs under an exclusive advisory barrier and can return
  `core.ErrTransient` — **it does not append the close line in that case**;
  callers must retry, and composing `ClosePeriod` into a `RunInTx` that also
  takes ledger locks can surface 40P01 as `ErrTransient` (see I-61's
  residual note). Deactivated currencies / classifications / journal types
  are now **refused in new journals** (`ErrInvalidInput`) instead of being a
  silent no-op, and all four `Deactivate*` methods return `core.ErrNotFound`
  for a uid that matches no row instead of `nil`. `service.ReconcileQuerier`
  gains `PeriodCloseViolations` (self-built implementations must add it;
  `postgres.NewReconcileAdapter` users need no change), and the suite runs
  sixteen checks rather than fifteen. I-3's wording is scoped to money
  movement, with the configuration writes that need no idempotency key
  listed explicitly.

- **`capital_injection` / `capital_withdraw` sign correction** (A-C1,
  migration 016): `equity` changed from credit-normal to debit-normal, and
  both templates' legs reversed (injecting capital is now CR custodial + DR
  equity). Migration 016 runs as owner, disables the mutation guard for the
  one statement, and forces a full rollup/snapshot recompute for `equity`.
  **If your deployment has posted through these two templates before
  upgrading**: those journals are append-only, and every read of them
  (`ledger_signed_amount` takes `normal_side` from `classifications`, live)
  now interprets them under the NEW polarity — so their meaning flips, which
  is exactly why they must be reversed and re-posted under the new
  templates. (This paragraph used to say "read with the OLD polarity", which
  described the opposite mechanism and made the required action look
  optional — w3-review/money-path.md m-5.) Amount keys are unchanged. Deployments that never used these
  two templates need no action.
- **`checkout_settlement_gross` / `_net` sign correction** (A-M2):
  `checkout_settlement_gross` legs reversed (merchant `main_wallet` is now
  DR +gross / `custodial` CR +gross). `checkout_settlement_net` grew from 3
  legs to 4 and **no longer accepts `gross_amount`** — it now takes only
  `net_amount` + `fee_amount` (gross is the derived sum), and posts a new
  `fee_expense` (memo) leg plus a `fees` leg. `SettlementBundle()` now also
  installs the `fee_expense` classification. A caller passing only
  `gross_amount` now gets `missing amount key`; one already passing
  `net_amount`/`fee_amount` is unaffected by the signature (only by the
  polarity fix).
- **`fee_charge` sign correction** (A-M4): grew from 2 legs to 4
  (`fee_expense` DR / `custodial` DR / `main_wallet` CR / `fees` CR).
  `FeeBundle()` now also installs `fee_expense`. Amount key (`amount`)
  unchanged. Deployments that already posted through `fee_charge`: the
  `fees` account's historical balance reads with the opposite sign —
  reverse and re-post.
- **`SolvencyCheck` custodial-asset scope is now injectable** (A-M6 /
  A-N3): replaces the hardcoded `code='custodial'` test with a
  classification-code set, default `{custodial, settlement}`. If none of
  your classifications match the configured set, `SolvencyChecker` now
  returns `core.ErrInvalidInput` instead of silently reporting
  `Custodial=0`. Deployments whose custodial classification isn't named
  `custodial` (or that relied on `settlement` being excluded from the
  asset side): pass `ledger.WithCustodialClassCodes(...)`.
- **Holder-facing balance/transaction scope widened** (A-M3 / A-N4): the
  four `balance_role <> ''` predicates across `holder.sql` / `reconcile.sql`
  changed to `NOT IN ('', 'memo')`. Withdrawal fees (posted to a
  `balance_role=memo` account) now appear in `ListHolderTransactions`; a
  currency with only memo-role postings no longer appears in
  `ListHolderBalances`. No API signature change.
- **Balance-trend inflow/outflow is now `normal_side`-aware** (A-M1): `GET
  /holders/{h}/trends` (and `BalanceTrendReader`) used to report the raw
  credit total as inflow and debit total as outflow regardless of the
  classification's polarity. For debit-normal classifications
  (`main_wallet`, `locked`) the two columns are now swapped to their correct
  economic sense — a correction, not a regression, but any dashboard
  graphing these columns will see a sign flip on upgrade.
- **`RequireVerifiedBalance` now sizes the reservation from entries alone**
  (C-C1, I-49): the verified withdrawal gate's ceiling is the live
  entries-only recomputation under the balance lock, never
  `balance_checkpoints` — I-32's prior wording ("not a stricter amount
  check") was itself wrong and is corrected. A deployment using
  `RequireVerifiedBalance: true` may see reservations refused that a
  corrupted or stale checkpoint previously allowed to through; that is the
  fix, not a regression.
- **`ReversalOfUID` and `EventUID` are now verified, not just recorded**
  (A-C2, I-51): `PostJournal` with `ReversalOfUID` set now rejects (a)
  reversing an already-reversed journal, (b) an entry set that is not a
  same-dimension reversal subset of the referenced journal within its
  remaining reversible amount (`ErrInvalidInput`), and (c) cumulative
  over-reversal (`ErrConflict`). `EventUID` is now verified to reference a
  real, previously-unlinked event (was existence-only). A caller
  constructing well-formed-but-semantically-wrong values now gets an error
  instead of a silently accepted mislinked or partial reversal.
- **Protected-template gate is now structural** (D-C1): `POST
  /journals/template` refuses any template with a leg on an `is_system`
  classification (403), not just a fixed four-code list. Newly caught:
  `dev_credit`, `capital_injection`, `capital_withdraw`, `fee_charge`,
  `checkout_settlement_gross`, `checkout_settlement_net`, `fx_buy`,
  `fx_sell`, `transfer_in`, `transfer_out`, `withdraw_confirm`,
  `withdraw_fee`, `deposit_pending`, `deposit_resolve_overage`,
  `deposit_release_overage` (only `lock_funds`/`unlock_funds` still post
  through this endpoint by default). `presets.ProtectedTemplateCodes()` grew
  from 4 codes to 5 (added `dev_credit`). Deployments that legitimately post
  one of these over HTTP: add it to `Config.AllowGenericTemplatePost` /
  `ALLOW_GENERIC_TEMPLATE_POST` — the single, structural-plus-named place
  that grants the exception.
- **`POST /journals/deposit-tolerance` moved to admin scope and now
  default-403's** (contract §7.11, same mechanism as above): every model it
  can execute passes through the same protected-template gate, so with the
  default config it refuses all four deposit codes — a `write`-scope key can
  no longer post an arbitrary tolerance/actual pair to mint a full deposit
  through this endpoint. Deployments relying on it: either list the
  specific deposit codes in `ALLOW_GENERIC_TEMPLATE_POST` and call with an
  **admin** key, or switch to the in-process
  `presets.BuildDepositTolerancePlan` + `ExecuteDepositTolerancePlan` (no
  HTTP hop).
- **`POST /journals/{uid}/reverse` no longer accepts a client-supplied
  idempotency key** (H-M3 Go side): the field (and its `Idempotency-Key`
  header alias) is now rejected with 400 — the key is derived server-side
  from journal uid + reason. It used to be silently ignored despite
  `docs/openapi.yaml` marking it required. Callers needing a self-chosen key
  should use `POST /journals/{uid}/reverse-partial` with `num=den=1`
  (equivalent full reversal; that endpoint does accept `idempotency_key`).
- **`(*ledger.Service).Worker(cfg)` now returns `(*service.Worker, error)`**
  (E-M5 / B-M6), was `*service.Worker`: `worker := svc.Worker(cfg)` →
  `worker, err := svc.Worker(cfg)`. The only case that errors is calling it
  from inside a `RunInTx` callback (previously silently built a
  half-tx-bound worker).
- **`(*service.Worker).Subscribe(handler)` now returns `error`** (E-M1),
  was `void`: check it. The only case that errors is subscribing after
  `Run` has started (previously only logged, and the default logger drops
  it).
- **`(*service.Worker).Run` now refuses to start when its logger is
  `core.NopLogger`** (E-M1 / I-M11) — a **runtime behavior change**, not a
  signature change: a consumer wired per the old README Quick Start (no
  `WithLogger`) previously ran with zero output and events stuck in
  `pending` forever; it now fails fast at `Run` instead. Fix: pass
  `ledger.WithLogger(...)` (recommended), or opt into the old silent
  behavior explicitly with `ledger.WithSilentWorker()` /
  `(*service.Worker).AllowSilent()`.
- **`(*ledger.Service).Worker` now auto-wires `SetFullReconciler`**
  (I-M11): a consumer that already called `SetFullReconciler` themselves is
  unaffected (the later call wins); one that didn't now gets the full
  15-check reconciliation suite running on `FullReconcileInterval` by
  default — new DB read load and a new alerting surface where there was
  previously silence.
- **`(*ledger.Service).RegisterChannel` now errors on a `RunInTx` clone**
  (E-M3), was silent success + drop: move the call to the top-level
  `Service`, before `svc.Channels()` is evaluated.
- **`core.DepositSighting.TxLogSeq` redefined** (G-C2): was "position
  within the log query's result set" (the watcher and webhook ingestion
  could compute different values for the same transfer), now "zero-based
  position of the Transfer log within its own tx receipt" (stable across
  reorg re-mining, identical regardless of which query found it). No-compat:
  existing `bookings.metadata.txlog_seq` values are not migrated — old keys
  stay unique, just no longer equal to what a re-observation under the new
  definition would compute. **External scanners feeding `channel/onchain`
  webhooks must switch to the same definition.** Confirm zero in-flight
  (`pending`/`confirming`) deposit bookings before upgrading.
- **`service.Onchain.Run` now requires a `ReorgRecorder` when a
  `ChainReader` is configured** (G-M8): previously a configured watcher with
  no reorg recorder ran anyway, and any deep-reorg detection above the
  recheck window vanished with no trace. Pass
  `service.WithReorgRecorder(postgres.NewDepositReorgStore(pool))` (or the
  equivalent `EnableOnchain` option). Webhook-only deployments (no
  `ChainReader`) are unaffected.
- **`core.SweepPolicy.GasCeiling` unit corrected from wei to gwei** (G-M3):
  the field was always compared as gwei in code despite being documented as
  wei; `Validate()` now rejects values above 1e6 gwei. A deployment
  configured in wei (e.g. `50000000000` meaning 50 gwei) must switch to the
  gwei number (`50`) or fail to boot — this turns a silently
  10⁹×-too-permissive gas ceiling into a startup-time error.
- **`core.TokenConfig.Validate()` added and enforced at startup** (G-M7):
  `Decimals < 0` or `> 36` now refuses to boot (`service.Onchain.Run` /
  `EnableOnchain`). Fix misconfigured decimals; consider also calling the
  new `(*evm.ClientSet).VerifyTokenDecimals(ctx)` against the live contract.
- **`core.Sweeper` gained a new required method** —
  `ReplacementGasPrice(ctx, chainID, signerNonce, priorTxHash)
  (decimal.Decimal, error)` (G-M4): a hand-written `core.Sweeper`
  implementation will not compile until it adds this, deliberately — a
  silent fallback to the old `GasPrice()` semantics would mean the gas-bump
  ceiling continues to not apply. `chains/evm.Sweeper` already implements
  it.
- **Shallow reorg no longer flips a deposit to `failed` on one
  `TxIncluded=false`** (G-M1): now requires 3 consecutive misses by default
  (`service.WithShallowReorgMisses`). No action needed; pass `1` to restore
  the old immediate-failure behavior (not recommended).
- **`SetChainCursor` gained monotonic protection** (B-m7): writing the
  cursor backward is now a no-op instead of silently regressing it. No
  action needed for facade consumers — `EnableOnchain` now passes
  `service.WithPool(pool)` automatically so the per-chain advisory lock
  actually takes effect (it silently didn't before). Consumers calling
  `service.NewOnchain` directly (not through the facade) must pass
  `service.WithPool(pool)` themselves.
- **Sweep now verifies nonce on-chain before replaying a pending booking**
  (G-M5): on finding the nonce already spent by the signer's own EOA, sweep
  returns `core.ErrConflict` and stops that (chain,token)'s collection until
  an operator resolves it (RUNBOOK §15) — replaces blindly replaying, which
  either stalled forever on "nonce too low" or underpriced-replaced a real
  transaction.
- **`service.Onchain.Run` now refuses to start on a cyclic deposit
  lifecycle** (F-m10): the one startup check that reads the database. A
  deployment using the shipped `presets.DepositLifecycle` (acyclic by
  construction) is unaffected.
- **`core.Metrics` currency-labelled methods take `currencyUID string`, not
  `currencyID int64`** (H-M9): `.BalanceDrift(`, `.NegativeBalanceDetected(`,
  `.ReconcileGap(`, and `.ReservedAmount(` all changed signature — an
  internal `currencies.id` has no meaning outside this database and is not
  stable across a restore-from-backup, so every other currency-labelled
  metric on this interface already used the uid; these four were the last
  holdouts. A hand-written `core.Metrics` implementation (not embedding
  `NoopMetrics`) will not compile until it updates these four methods.
- **`core.Metrics` grew from 32 to 41 methods** (I-M1, I-M8/C-M9, I-M10,
  B-m10): the entire `postgres/` write layer was previously not wired to
  `core.Metrics` at all (12 of the 32 existing methods had zero production
  call sites), and three background-job families and the tamper-evidence
  chain had none. New methods: `JobTickCompleted` / `JobTickFailed` /
  `JobTickSkippedLocked` / `JobPanicked` (worker + `LockedJob` + `Onchain`
  job ticks), `StuckRollups` (rollup items that exhausted their retry
  budget — see the new `ledger-cli rollup reset-claim`), `PendingEvents`
  (declared; not yet emitted — see `crossBranchExclusions` in
  `observability/emission_coverage_test.go`), `AttestationBatchResult` /
  `AnchorPublishResult` / `AnchorLagSeqs` (P5/P6 tamper-evidence chain).
  A hand-written `core.Metrics` implementation must add all nine (embedding
  `NoopMetrics` is unaffected). `LedgerStore` / `ReserverStore` /
  `BookingStore` each gained a `WithMetrics(core.Metrics)` chain method,
  defaulting to `core.NopMetrics()`; `ledger.New` wires them automatically.

- **Watcher-path deposits no longer pass through `pending` / `confirming`**
  (W3 re-review, w3-review/money-path.md m-6; a behavioural consequence of
  I-53's confirmation-depth lower bound). The forward scan's window now ends
  at `latest - Confirmations + 1`, so every sighting handed to
  `IngestDeposit` already meets the confirmation threshold and
  `advanceConfirmation` takes it straight to `confirmed`. The
  `deposit_confirm_pending` template and the two intermediate booking states
  therefore no longer occur on the **watcher** path (the webhook path is
  unchanged, and both states remain part of the model). Consumers whose UX
  shows "pending → confirmed" for on-chain deposits will see only the final
  state; if you need the intermediate one, lower `Confirmations` or drive
  ingestion from the webhook path.

### Go module — Security (verified-balance gate: the hold term)

- **A gated `Reserve` now holds conservatively, and an expired reservation
  can no longer be settled** (Wave 3 C-1,
  `docs/audits/2026-09-02-deep-audit/w3-review/money-path.md`). I-49 made the
  *base* of the gated availability expression tamper-resistant and left the
  term subtracted from it alone: the hold came from `reservations.status` /
  `.settled_amount`, and `ledger_reservations_guard` permits
  `active → settling/settled/released` and permits `settled_amount` to grow
  because those are the legitimate transitions. `ledger_app` holds `UPDATE`,
  so one permitted statement reported a live 1000 hold as zero and the gate
  authorized 2000 against a balance of 1000.

  Reading the discharge from the append-only settlement record instead does
  not fix it — `ledger_app` must keep `INSERT` on those tables, and a forged
  receipt discharges a hold at the same one-statement cost (measured). In
  this threat model the application's credential *is* the attacker, so the
  only claims it cannot manufacture are a signature and the passage of time.
  So: **when `RequireVerifiedBalance` is set, the hold is the full
  `reserved_amount` of every not-yet-expired reservation on the dimension,
  with no credit for settlement or release**, and `Settle` / `SettlePartial`
  return `core.ErrInvalidTransition` once `expires_at` has passed (without
  that half, waiting out an expiry would double-authorize with no tampering
  at all). `Release` and `FinalizeSettlement` still work on an expired
  reservation — neither records a new amount, and both are what
  `service.ExpirationService` calls to wind one down.

  **What consumers must do.** If you set `RequireVerifiedBalance`: after a
  `Settle` or `Release`, those funds stay unavailable *to gated calls* until
  the reservation's `ExpiresAt` passes — set `ExpiresIn` to the real lifetime
  of the operation (default 15 minutes) rather than leaving a long expiry on
  a short-lived reservation. If you settle reservations late — after their
  `ExpiresAt`, relying on the old behavior where `Settle` ignored expiry —
  that call now fails; either extend `ExpiresIn` or release and re-reserve.
  Consumers who never set the flag and never settle past expiry see no
  change. The ungated `Reserve`, `HeldAmount` and `GetBalanceBreakdown` keep
  reporting the state machine's own figure.

  Not closed, recorded rather than implied away: signing the settlement
  receipts (attested on write, verified before the transaction opens like V
  is) would make the discharge unforgeable and restore immediate recycling.
  That is a composition-root change and is deferred; I-49 carries the
  analysis, including the residual boundary window between a settle
  transaction and the gate.

- Migration **025** (`gated_hold_expiry_index`) adds
  `idx_reservations_account_currency_expiry` — a query that must not mention
  `status` cannot use the `status = 'active'` partial index the ordinary
  path uses.

### Go module — Security (pending deposits: the confirmation amount)

- **`ConfirmPending` / `CancelPending` size the confirmation from
  `journal_entries`, not from `balance_checkpoints`** (Wave 4, contract
  §7.18; the sibling of `w3-review/money-path.md` M-1, found by `w3-holds`'
  sibling scan). New invariant **I-64**. `ConfirmPending` mints spendable
  balance — it debits `pending` and credits the holder's `main_wallet` — and
  in pool mode this store signs that journal with the deployment's real
  `core.Attestor`. Deciding *how much* off `checkpoint + delta` therefore
  made the checkpoint hole strictly worse than the one I-49 closed on
  `Reserve`: one forged `balance_checkpoints` row on the pending dimension
  was laundered into a **genuinely signed** credit to an available-role
  classification, which both terms of the withdrawal gate then accept (E
  sums it because it is a real entry; V accepts it because the signature is
  real). Measured: a forged pending checkpoint of 1,000,000 let
  `ConfirmPending(1000)` through against a true pending balance of 60.

  Both calls now recompute the pending dimension with
  `RecomputeCheckpointFromEntries` — the same query I-49's E term and
  `CheckpointIntegrityStore.RecomputeBalance` use — inside the transaction,
  under the `(holder, currency)` advisory lock, in pure SQL.

  **What consumers must do: nothing.** No signature changed and no honest
  call behaves differently; on an untampered database the two figures agree.
  The cost is a full-history sum per confirm instead of a delta read, served
  by `idx_entries_account_id`. `ExpirePendingOlderThan` keeps its
  checkpoint-based pre-filter on purpose — it is lock-free, only chooses
  which rows to attempt, and the decision that moves money is
  `CancelPending`'s own check.

  Not closed, recorded rather than implied away: the gate reads *real*
  entries, not *authorized* ones, and unlike I-49 it has no V term. Measured
  as `ledger_app` over a real socket: two `INSERT`s forge an unsigned pending
  credit that this gate accepts and then signs on the way into `main_wallet`.
  Giving the gate a V term is a composition-root change and needs a ruling on
  what a `RunInTx`-composed `ConfirmPending` does; I-64's "What this does NOT
  close" carries the analysis and a pin that holds the boundary as measured.

- **`core.AccountPolicy`'s `enforce_min_balance` keeps reading
  `checkpoint + delta`, and I-17 now says so** (contract §7.18, the M-1
  ruling). It is a business rule for honest callers, not an anti-tampering
  control: it can only refuse a journal (overstating the balance creates no
  money), and `account_policies` is itself a config table the application's
  credential writes, so hardening the balance term alone buys nothing. Money
  leaving the ledger is gated by `core.ReserveInput.RequireVerifiedBalance`
  (I-49) and, for pending deposits, by I-64. No behavior change; the
  invariant, the `core.AccountPolicy` godoc and the enforcement site now
  state the boundary instead of leaving it to be inferred.

### Go module — Fixed

- **`Migrate`'s `ledger_owner` authority is now scoped to the connection it
  migrates on, not to the credential** (W3 re-review M-5). Until now, for a
  non-superuser runner, `Migrate` granted that **role** `ledger_owner WITH
  INHERIT TRUE` for the length of each migration. `pg_auth_members` is a
  cluster-wide shared catalog and Postgres's ownership check does not
  distinguish sessions, so every connection authenticated as the same
  credential — the application's own pool, in a single-credential deployment
  — was owner-equivalent for the duration. Measured on `postgres:17.10` with
  a non-superuser `CREATEROLE` runner: a second connection executed `DROP
  TRIGGER journal_entries_no_update ON journal_entries` mid-run, it stayed
  dropped, and `Migrate` returned `nil` — I-22 did not hold while a deploy
  was in flight.

  `Migrate` now applies `001` on the credential you give it, then opens **one**
  connection, switches it to `ledger_owner` (`SET ROLE`) and runs `002..N`
  there, via `migrate.NewWithInstance` over a `*sql.DB` pinned to that single
  connection. A superuser, or a connection as `ledger_owner` itself, is
  unaffected and migrates exactly as before. Where the credential cannot yet
  switch roles — the state `001` leaves a fresh install in, since its closing
  `REVOKE` removes the row `CREATE ROLE` created — `Migrate` grants itself the
  narrowest membership that permits it (`WITH SET TRUE, INHERIT FALSE`) and
  revokes it before returning; `pg_auth_members` ends byte-for-byte where it
  started, and while it is held no session that does not deliberately `SET
  ROLE` sees anything. This also removes the previous mechanism's side effect
  of rewriting the operator's `SET`-only membership to `INHERIT` and then
  deleting the row outright. Pinned by
  `TestMigrate_WindowIsNotVisibleToOtherSessionsOfTheSameCredential`, which
  parks a real run inside `002..N` with an exclusive lock and requires `42501`
  from a connection opened on the same credential mid-run.

  **`Migrate` also refuses to run while another session holds the migration
  credential** (non-superuser path only, and a behaviour change worth reading
  before your next deploy). What the SET-only membership cannot take away is
  that a session on that credential can switch to `ledger_owner`
  deliberately — a compromised application is a session that does what it is
  told — so a single-credential deployment is now a failed deploy with an
  instruction in it rather than a silent risk. Before arranging anything, and
  again once the membership exists, `Migrate` counts the other sessions
  connected as that credential (`pg_stat_activity`, its own connections
  excluded by `application_name = azex-ledger-migrate`) and returns an error
  naming the count, what it saw, and the remedy. In-process migration on a
  live pod sharing one credential with its own pool will hit this; a separate
  `MIGRATE_DATABASE_URL`, a deploy-step migration, or a superuser /
  `ledger_owner` connection will not. Pinned by
  `TestMigrate_RefusesWhileAnotherSessionHoldsTheMigrationCredential`, which
  also asserts the same run succeeds once the other session is gone.

  The residual is stated rather than implied away: the guard runs at the start
  of a run, so a connection opened on the migration credential *while*
  migrations are in flight can still `SET ROLE ledger_owner` deliberately.
  Measured, and pinned as such — bounded by the membership being `SET`-only
  (nothing is inherited), by its revocation when `Migrate` returns (the same
  statement then fails with `42501`, also pinned), and by not existing at all
  for a superuser or `ledger_owner` credential. See `docs/INVARIANTS.md` I-22's
  "Note on the migration window".

  **Behaviour change worth knowing before a shared cluster surprises you**: on
  the non-superuser path, `002..N` now execute *as* `ledger_owner` and no
  longer carry the runner's own role attributes. The only statement in the set
  that wants them is migration `007`'s attribute hardening, and only when one
  of the three roles pre-existed on the cluster holding `SUPERUSER` /
  `CREATEROLE` / …; `007` then stops the install with its actionable message
  asking for a superuser connection, where a `CREATEROLE` runner could
  previously have stripped the attribute itself. A fresh install issues zero
  such statements. A credential that is neither superuser, nor `ledger_owner`,
  nor able to reach `SET ROLE ledger_owner` is still refused before the first
  migration runs, with a message naming all three ways out.

  All eight `examples/**/main.go` read `MIGRATE_DATABASE_URL` separately from
  `DATABASE_URL` and log a warning when they fall back to one URL for both; a
  static gate (`TestExamplesUseASeparateMigrationURL`) keeps them teaching it.
  `docs/RUNBOOK.md`'s "Database roles" section carries the operator-facing
  version, including what is still worth a separate credential now that the
  passive elevation is gone. **Action for consumers**: none required for a
  superuser or `ledger_owner` migration credential; otherwise keep pointing
  migrations at a credential the serving application does not use, and run
  them as a deploy step rather than in-process on a live pod.
- **`SolvencyCheck` no longer reads `balance_checkpoints`** (W3 re-review,
  w3-review/money-path.md M-2). Both of its figures are recomputed from
  `journal_entries` (the I-23 basis) instead of `checkpoint + delta`. A
  checkpoint is a derived cache the application credential may INSERT into,
  and one forged row moved the library's only unbacked-issuance alarm from
  `solvent=false` to `solvent=true`. Same values on a healthy ledger, higher
  read cost (a scan per currency, on a periodic report).
- **`unauthorized_journals` reports partial coverage** (M-7). `Complete` is
  now `checked == len(page)` rather than `checked > 0`, and the findings
  carry `skipped_unsigned=N`. One genuinely signed journal in the page used
  to make the check report GREEN with any number of unsigned, direct-SQL
  journals beside it. `Passed` is unchanged (an unsigned journal is a
  coverage gap, not a violation), so alerting on `Passed && Complete` gets
  stricter — a fleet with pre-signing history will now see this check
  incomplete, which is the honest reading.
- **`VerifyLedger` stops treating `auth_status` as a trust signal** (M-4).
  `journals.auth_status` is a plain column the app credential writes, so a
  forged journal claiming `unsigned_tx_mode` used to be counted as a benign
  backlog forever while the identical row under the column default was
  TAMPERED. Step 3b now accounts for an uncovered entry only on a valid
  signature; anything else is DRIFT for `VerifyConfig.UncoveredGracePeriod`
  (new, default 5m) and TAMPERED after, with no grace at all for a
  future-dated `effective_at`. `unsigned_no_attestor` keeps its immediate
  TAMPERED. **Deployments whose attestation job is not actually running will
  now go TAMPERED** once the window passes — that is the intended reading
  (nothing is verifying anything), but raise `UncoveredGracePeriod` if your
  attest interval is longer than a few minutes.
- Claim-lost warnings inside `EventStore` now go through the injected
  `core.Logger` (via `SetLogger`, wired automatically by `ledger.New` and
  `(*Service).Worker`) instead of `slog.Default()` (I-R1 / B-m1). If you
  scrape these three log lines from `slog.Default()`'s output specifically,
  update your log pipeline.
- `(*ledger.Service).Onchain()` called on a `RunInTx` clone no longer
  returns `nil` when the top-level `Service` has `EnableOnchain`'d — it now
  returns the real subsystem (E-M4). `EnableOnchain` itself is still
  rejected on a clone.
- `Ping()` on a `RunInTx` clone that escaped its callback now reports
  through the transaction, so it fails the same way every other read on
  that clone does, instead of reporting healthy through the pool (E-m14).

### Go module — Added

- `ledger.WithCustodialClassCodes(codes ...string)` — see the
  `SolvencyCheck` entry above. As of the W3 re-review (m-1 / m-2) a scope
  the CONSUMER declares is validated per code: every code must name an
  existing classification, and each must be `is_system` with no
  `balance_role` (a custodied asset, not a holder liability and not the
  deliberately-unbacked `dev_credit`). Any violation is `ErrInvalidInput`
  naming the offending codes. The library's own default scope keeps its
  older, looser rule (it fails only when NO code matches), because
  `{custodial, settlement}` is a guess about your install, not your
  declaration.
- `service.StartupReport.VerifiedBalanceVerifier` + `Worker.SetAuthVerifier`
  (M-6): the default install — `ledger.New(pool)` with no options — has the
  whole tamper-evidence stack switched off and used to report
  `Warnings: []`, while the strictly milder "attesting without an anchor"
  warned. Every absent subsystem (Attestor, anchor, `AuthVerifier`,
  `FullReconciler`, leader election) now emits its own warning saying what
  is off and how to turn it on, in the report and in `Run`'s log.
- `service.VerifyConfig.UncoveredGracePeriod` and
  `service.VerifyReport.UncoveredUnverifiedJournals` — see the
  `VerifyLedger` entry above.
- `ledger.RetryIdempotent(ctx, scope, attempts, fn)` — retries `fn` with the
  same idempotency key on every attempt, closing the other half of the
  library-mode retry contract (`core.IsRetryable` shipped in `[0.6.0]`; this
  is the "reuse the same key" half — see README's "Retrying a failed write").
- New invariants **I-49 through I-54** in `docs/INVARIANTS.md` pin the
  Critical fixes above.
- New migrations **016** (`preset_sign_correction`, owner-run polarity
  correction + forced rollup/snapshot recompute) and **017**
  (`deposit_reorgs`, durable reorg-anomaly table + monotonic scan cursor).
- Migration **027** (`unlink_event_journal`, W3 re-review M-3, contract
  §7.15): `ledger_unlink_event_journal(uuid)` — SECURITY DEFINER, owned by
  `ledger_owner`, **never granted to `ledger_app`** — clears
  `events.journal_id` (and `bookings.journal_id`, when the booking holds the
  same journal) so a booking whose event was claimed by an unrelated journal
  can settle. Before it there was no unlink of any kind: both columns are
  set-once and journals are append-only, so one 0.01 journal touching the
  booking's `(holder, currency)` — all I-51 rule 4 requires — stopped that
  booking's accounting forever. The two set-once guards gained exactly one
  exception for it (`journal_id` non-NULL → **NULL** only, requiring
  `ledger_owner` membership AND a transaction-local flag). The claiming
  journal is left untouched; if it moved money, reverse it. Procedure:
  `docs/RUNBOOK.md` §17.
- Migration **024** (`owner_written_anchor_observations`, W3 re-review m-4):
  `ledger_app` loses `INSERT` on `anchor_observations` and gains `EXECUTE`
  on `ledger_record_anchor_observation(uuid, bigint, bytea)`, a
  `SECURITY DEFINER` writer that refuses any observation ahead of this
  deployment's own attestation chain. One forged row at `observed_seq =
  999999` used to pin `VerifyLedger` to TAMPERED permanently, on a table
  that refuses `UPDATE` and `DELETE` to everybody. Consumers using the
  shipped `postgres.AttestationStore` need no change.

### anchors/r2 (separate Go module)

- `anchors/r2` (Cloudflare R2 + Object Lock `core.Anchor`) and
  `anchortest.RunConformance` (I-48, a reusable conformance suite any
  `core.Anchor` implementation can self-test against in one line) shipped
  after `[0.6.0]` — see `docs/RUNBOOK.md` ("Choosing an Anchor carrier") for
  deployment steps and the two credential scopes it needs. **Not yet
  independently `go get`-able** — see `docs/RUNBOOK.md`'s "Consuming the
  submodule today" and the corresponding README note; consume it via a
  local `go.work` checkout for now.

## [0.6.0] — 2026-08-27

> **本版本已知敞开项**（不是缺陷，是尚未做的工作，列出以免被绿灯的测试掩盖）：
> 外部锚定没有生产实现（`anchordev` 是 DEV/TEST ONLY，防篡改链最外环缺失）；
> 本库尚无真实生产消费方，全部正确性由测试而非流量验证。见评估文档 §4。


Two release-sized bodies of work landed this cycle. **Tamper-evident ledger**
(2026-08-21, `docs/plans/2026-08-21-tamper-evident-ledger-design.md`) answers
the question a leaked database credential raises: if an attacker gets DB
write access, what stops them from crediting themselves with a perfectly
balanced, forged journal? The answer is now "checkpoints are an untrusted
cache, per-journal writes are cryptographically signed, and withdrawals only
pay out against a verified balance" — P0 through P7 below, plus two follow-on
waves. **Crypto deposit + sweep**
(`docs/plans/2026-07-11-crypto-deposit-sweep-design.md`) turns the
CREATE2-derived shared-address deposit flow into an optional, library-shipped
default, with M3 compensating controls (threshold gate + dual-provider
reconciliation → human review) and a full wallet-side frontend.

### Go module — Removed

- **Breaking**: the standalone service binary is gone, along with `cmd/ledgerd`,
  the Helm chart, the Grafana dashboard, the Dockerfile and the compose
  services that ran them. This is a library; it now ships nothing to deploy.
  `docker-compose.yml` starts PostgreSQL alone for local development, and
  `make docker` is `make db`.

  `server/` stays. It is not a composition root -- it takes already-assembled
  dependencies and returns an `http.Handler` -- so a consumer that wants the
  HTTP surface mounts it in their own binary, as
  [`examples/fullstack`](examples/fullstack/) does. `docs/openapi.yaml` remains
  the wire contract that surface implements and that `@azex/ledger-react`
  generates its types from.

  Why it went: the binary was a second composition root that assembled a
  different set of capabilities than the library facade. It never enabled
  per-journal signing, never scheduled batch attestation, and exposed no way
  to require a verified balance -- so everything under "Security" below was
  inert in the only deployment form this repo shipped, and no test noticed,
  because nothing tested that binary.

### Go module — Fixed (accounting correctness)

Two of these move real money and are worth reading before upgrading.

- **Transfers and fees ran in the wrong direction.** `transfer_out`,
  `transfer_in` and `fee_charge` had their holder leg inverted against
  `main_wallet`'s declared polarity, so a peer-to-peer transfer of 100 left
  the sender 100 richer and the receiver 100 in debt, and charging a fee paid
  the payer. `deposit_confirm` and `checkout_settlement` were always correct;
  these three disagreed with them. **If you have posted through these
  templates, the resulting journals are wrong and need reversing.**

  The preset tests passed throughout: both legs draw on the same amount key,
  so "total debits equal total credits" holds whichever side each
  classification lands on.

- **"Reverse everything remaining" under-reversed when a journal had two
  entries on the same dimension.** Prior reversals are tracked per dimension
  while the remainder was computed per entry, so each entry subtracted the
  whole dimension's prior total. A 60 + 40 journal reversed by half and then
  "the rest" left 40 on the books and returned success. **Check any journal
  you reversed in fractions that carried repeated dimensions.**

- Inbound webhooks failed on every request in any deployment that used the
  role separation the schema installs: the replay-cache prune is a `DELETE`,
  `ledger_app` had none, and the error was returned rather than tolerated.
  Migration 002 grants that one `DELETE` -- which the schema's own comment
  already called "the one sanctioned DELETE" -- and the prune now tolerates a
  refused privilege without failing the request.

- A cached attestation-time `Authorized` verdict no longer lets
  `VerifiedBalance` skip the live check. The verdict answers whether a journal
  was authorized *when attested*; the withdrawal gate needs whether it is
  authorized *now*, and the canonical digest covers every entry's amount, so
  an amount edited after attestation was invisible to the gate. Verification
  cost on that path returns to its pre-optimization level, deliberately.

### Go module — Security (configuration integrity)

- Migration 003 puts a column whitelist on `currencies`, `classifications`,
  `journal_types`, `entry_templates`, `entry_template_lines` and
  `deposit_addresses`. A per-journal signature authenticates what the
  application *read*, not what happened -- so an application credential that
  could rewrite a deposit address's holder, or a template line's direction,
  made the application sign a correct journal about the wrong facts, and the
  result verified. Only `is_active`, `display_label`, `lifecycle` and
  `balance_role`'s one-way upgrade stay mutable; `currencies.exponent`,
  `classifications.normal_side` and `.code`, and every column of a template
  line do not.

- Migration 004 refuses promoting a classification to `balance_role`
  `'available'` once it has journal entries. `available` is the only bucket
  `Reserve` spends from, so promoting one that already holds balances turns
  them into withdrawable funds in a single statement -- the shipped
  `fee_expense` is debited on every withdrawal fee. `'pending'` and
  `'locked'` stay unrestricted; neither is spendable.

### Go module — Security (tamper-evident ledger, P0–P7 + Waves 2–3)

- **P0 — Reconciliation coverage fixed**: the negative (system) holder range
  was never scanned by check #2 (its keyset cursor started at `(0,0)`, and
  `account_holder > cursor` permanently excludes negatives); a scan that hit
  `Check2ScanLimit`, timed out, or covered a check the schema didn't support
  yet was still reported `Passed=true`. `CheckResult` now carries a
  `Complete` flag (zero value = not completed, fail-closed) and
  `ReconcileReport` a `FullCoverage` flag; the `ReconcileCheckResult` metric
  only reports green when a check both ran and finished.
- **P1 — DB role least privilege**: `ledger_owner` (DDL-capable, owns every
  object), `ledger_app` (read/write, no DDL), and `ledger_ro` roles; schema
  ownership transferred to `ledger_owner` and `PUBLIC` access revoked. This
  is the prerequisite every later DB-layer guard below depends on — without
  it, any of those triggers could simply be dropped by the credential they
  exist to constrain. The ownership sweep originally covered tables and
  sequences only, leaving all five guard functions — including the two that
  enforce append-only on `journals` and `journal_entries` — owned by whoever
  ran the migration. A function's owner can `CREATE OR REPLACE` its body, so
  that is a silent way to disable every append-only guarantee while leaving
  the triggers in place and emitting no DDL an audit would flag. The baseline
  sweeps tables, sequences, views and routines, and asks the catalogue what
  exists rather than working from a list; a CI check now fails if any object
  is not owned by `ledger_owner`.
- **P2 — Checkpoints are an untrusted cache**:
  balances can now be recomputed entirely from `journal_entries`
  (`RebuildCheckpoint`), with a resumable scan cursor and an append-only
  `checkpoint_rebuilds` audit trail. `system_rollups`/`balance_snapshots`
  drift detection no longer inherits checkpoint corruption.
- **P3 — Per-journal balance enforced at the DB layer again**: a deferred
  constraint trigger rejects any journal whose entries don't balance
  per-currency, independent of the application. This closes a
  real regression — an equivalent trigger existed early on, was dropped
  during a later refactor and never replaced, so pure-SQL access could insert
  unbalanced entries for several months of the project's history.
- **P4 — Mutation guards on non-journal balance tables**:
  `classifications.normal_side`/`balance_role`, `reservations`, and
  `period_closes` can no longer be mutated outside their one legitimate
  entry point. `journals.event_id` gets the same set-once protection —
  see the breaking schema/type change under Changed below.
- **P5 — Per-journal authorization signing** (plus a follow-up fix): a new
  `core.Attestor` port (distinct from the on-chain
  `core.Signer` — different key, different blast radius) signs every
  journal, reversal, and template batch at write time, including writes
  composed through `RunInTx` (a gap in the initial cut where those weren't
  being signed is now closed).
- **P6 — Batch attestation chain + external anchor**:
  journals are chained into signed, gapless batches with an externally
  anchorable head, so a compromised DB owner can't silently truncate the
  tail without `verify` noticing.
- **P7 — RFC 6962 Merkle tree**: batch digests gained a
  Merkle root plus inclusion proofs, so `verify` can localize *which*
  entries were tampered with, not just detect that a batch changed.
- **Withdrawal-time verified balance** (Wave 2): `ReserveInput.RequireVerifiedBalance`
  gates a reservation on a balance computed only from signed/authorized
  journals — an unsigned or forged journal can no longer fund a withdrawal
  even if it's balanced and passes every other check.
- **Attestation-batched authorization verdicts** (Wave 3): signature
  verification cost is amortized across a batch at attestation time instead
  of being re-derived on every read. A cached verdict may only ever be
  trusted, never re-derived, and is contractually at least as strict as a
  live check.
- **Deposit-review separation of duties** (Wave 3): resolving a deposit
  stuck in `review` now requires a capability that no existing API-key
  scope implies on its own (an `admin` key alone can no longer both create
  the anomaly and clear it); a persistently unreachable second confirmation
  source escalates through alerting instead of leaving the review queue
  stuck indefinitely.
- New invariants **I-19 through I-34** in `docs/INVARIANTS.md` pin all of the
  above; `examples/tamper-evident` is a runnable end-to-end demonstration —
  it forges a row and shows exactly what stops the money from leaving.

### Go module — Added (crypto deposit + sweep)

- CREATE2-derived shared deposit addresses
  (`svc.Onchain().EnsureDepositAddress`), a pull-based EVM watcher + sweeper
  (`chains/evm`, a separate Go module so consumers who don't need on-chain
  support never pull in go-ethereum), and a push-based webhook ingestion
  path — both converge on `svc.Onchain().IngestDeposit`.
- Address registration performs a full historical rescan before returning
  and tracks a durable per-chain scan cursor (`registration_rescans`) that
  survives process restarts and resumes in bounded block windows — no
  fire-and-forget scanning, no unbounded genesis-to-tip replays.
- **M3 compensating controls**: an `AutoCreditCeiling` threshold gate plus
  dual-provider (`DepositConfirmer`) reconciliation routes anomalous or
  unconfirmed deposits into a `review` state with zero ledger effect (I-21)
  instead of auto-crediting; human approve/reject endpoints post the
  compensating journal. Secure by default — an unconfigured ceiling fails
  startup instead of silently defaulting to unlimited auto-credit.
- Ops hardening: stuck sweeps are automatically revived and sweeps are
  serialized per chain to avoid double-submission.
- Developer-mode credit preset (`dev_credit`): credits a holder against no
  custodied asset, gated by `ENV=dev` + `DEV_CREDIT_ENABLED=true`. For local
  development and demos only — not installed by `InstallExtendedPresets`.

### Go module — Changed

- **Breaking**: `NewReserverStore(pool, ledger, verifiedBalance)` gained a
  third parameter (`*postgres.VerifiedBalanceStore`), needed for the
  withdrawal-time verified-balance gate above. Callers constructing a
  `ReserverStore` directly must update the call site.
- **Breaking**: `journals.event_id` is now nullable with a real foreign key
  to `events(id)` — previously `NOT NULL DEFAULT 0`, using
  `0` as an unenforced sentinel for "no event". The generated
  `postgres/sqlcgen.Journal.EventID` field changes from `int64` to
  `pgtype.Int8` accordingly. The public library surface
  (`core.Journal.EventUID string`) is unaffected; only code reading the
  generated struct or the raw column directly needs to switch from
  `== 0` to `.Valid`.
- **Breaking (installation)**: the 53 migrations accumulated since the
  project's first commit are squashed into a single `001_baseline`. A new
  install runs one migration instead of fifty-three, and gets the locked-down
  role configuration in that one step rather than through a staged rollout.
  **The connection that runs it must be able to `CREATE ROLE`** — a superuser
  or any role with `CREATEROLE`, which every managed-Postgres master user
  has. That credential is install-time only and should be rotated or retired
  afterwards. Equivalence with the old chain is enforced by diffing
  `pg_dump --schema-only` between a database built by each, which reports no
  difference. Migration numbers referenced anywhere else in these notes are
  historical: the released artifact contains `001_baseline` plus `002`
  through `004`, the three this release adds on top of it, and nothing else.
- **Breaking (Go API)**: `core.Reserver.Release` changed from
  `Release(ctx, reservationUID string) error` to `Release(ctx, input
  ReleaseInput) error`.
- **Breaking (Go API)**: `core.Reserver.FinalizeSettlement` changed from
  `(ctx, reservationUID string) error` to `(ctx, input
  FinalizeSettlementInput) error`.
- **Breaking (Go API)**: `core.SettleInput` gained a required
  `IdempotencyKey` field — `Validate()` now rejects an empty one.
- **Breaking (Go API)**: `core.TransitionInput` gained a required
  `IdempotencyKey` field (I-3) — `Validate()` now rejects an empty one.
- **Breaking (Go API)**: `core.RollupQueueItem` moved out of `core` into
  `service` — it was never part of the consumer-facing domain surface, only
  the rollup worker's internal queue item shape.
- **Breaking (Go API)**: `core.BalanceCheckpoint`'s `CurrencyID` /
  `ClassificationID int64` fields became `CurrencyUID` / `ClassificationUID
  string`, and `LastEntryID` was removed (superseded by `LastEntryAt`).
  Consistent with the uid-only identity contract (see `[0.4.0]` below); this
  struct had been missed in that pass.

  The six entries above landed with this release but were not called out in
  it at the time — 26-09-02 correction. The full 23-item breaking-change
  list, organized by what a consumer needs to do (not by which commit
  introduced it), is
  [`docs/plans/2026-08-27-release-readiness.md`](docs/plans/2026-08-27-release-readiness.md)
  §3.

### Go module — Fixed

- **`Worker.Subscribe` never delivered anything.** It built its dispatcher
  with no event poller and required a separate `SetLocalPoller` call before
  `Run` that nothing outside the test suite made — so an in-process
  subscription silently received no events, forever, while logging a failure
  on every poll tick. `ledger.Service.Worker` now wires the poller, so
  subscribing is all a consumer has to do. If you were calling
  `SetLocalPoller` yourself, keep doing so; it still works and is still
  required when you build a `service.Worker` directly rather than through the
  facade.
- A forged-but-unsigned journal could report as `VERIFIED` under some code
  paths — closed.
- `PendingStore` dropped an unread cached set of classification IDs.
- Assorted ledger-correctness and event-delivery hardening fixes surfaced
  during the integrity-hardening review pass.
- New `JournalTypeStore.SetHolderKind` and reconcile check
  `untagged_holder_kind` (M-7 follow-up, docs/INVARIANTS.md I-44): a journal
  type visible in a holder's transaction list without a `holder_kind` had no
  way to be discovered other than a user noticing `"other"` where a
  specific label was expected. Pure addition, no behavior change to what
  `kind` resolves to on the wire.

### @azex/ledger-react — Added

- End-user wallet: `WalletDepositAddressCard` (shadcn + heroui), backed by a
  holder-token-scoped `GET /api/v1/holder/deposit-address` endpoint — no
  admin key ever reaches the browser.
- M3 deposit review queue page (shadcn + heroui).
- Sweep collection monitor page (shadcn + heroui).
- Headless contract for crypto deposit (client + hooks + types), for hosts
  building their own UI on top of the deposit/sweep feature instead of the
  shipped components.

### @azex/ledger-react — Fixed

- **Breaking**: `HolderTransaction.kind` (and `TransactionList`'s
  `kindLabels` prop, keyed by it) is now one of a small,
  deployment-stable vocabulary — `"deposit" | "withdrawal" | "transfer" |
  "fee" | "adjustment" | "other"` (`core.HolderTxKind`,
  docs/INVARIANTS.md I-44) — instead of the ledger's internal journal-type
  UUID. If your app passes `kindLabels` keyed by that UUID (or, from before
  this cycle, by the journal-type *code* such as `"deposit_confirm"` — both
  prior shapes silently matched nothing once the server changed under
  them), rewrite the keys to the vocabulary above, e.g.
  `kindLabels={{ deposit: "Top up" }}`. No action needed if you only
  consumed the library's default `kind_label` text. `docs/openapi.yaml`'s
  `HolderTransaction.kind` schema is updated to a matching `enum`.
- `ErrorState` gained a Retry action across both skins (previously a dead
  end once an error rendered).
- `schema.ts` (OpenAPI-generated types) regenerated to match the current
  contract, plus a local `openapi-check` script so this can't drift
  silently again.
- Gaps between `docs/frontend.md` and the package's actual exports closed.
- `qrcode.react` dependency pinned.

### Examples

- Every example that needs only a database was run end to end against a fresh
  one. Two could not complete before: `crypto-deposit` was missing a lifecycle
  on the seeded label-only `deposit` classification (the accounting bundles
  install templates, not lifecycles) and then skipped the `confirming` state
  that `DepositLifecycle` requires; `event-subscribe` was demonstrating the
  broken `Subscribe` path above, and printed a note and exited 0 when no event
  arrived rather than failing.

### Docs

- `docs/plans/2026-08-21-tamper-evident-ledger-design.md` (design) and
  `docs/plans/2026-08-21-integrity-hardening-contracts.md` (the cross-task
  migration-number and resource-allocation contract for P1–P7 and the
  in-flight migration-baseline squash) carry the full rationale behind the
  Security entries above.
- `docs/RUNBOOK.md`, `docs/DR.md`, and `docs/openapi.yaml` updated for the
  new roles, reconcile checks, and endpoints.

## [0.5.1] - 2026-07-09

Holder-scoped wallet surface (2026-07-08,
`docs/plans/2026-07-08-holder-scoped-wallet-surface.md`): the end-user
wallet — balances / translated transactions / holds for ONE holder — as a
library capability, so consumer products stop hand-rolling the projection.

### Go module — Added (wallet surface)

- Holder read projections (`core.HolderReader` on the ledger store):
  `ListHolderBalances` (per-currency BalanceBreakdown + currency code),
  `ListHolderTransactions` ((journal, holder, currency) net aggregation over
  role-bearing classifications, user-language kind/label/direction, cursor at
  journal granularity), `ListHolderHolds`.
- Holder tokens: stateless HMAC (`lht_` prefix), single-holder, read-only,
  TTL-bound; `server.MintHolderToken` for in-process minting,
  `POST /api/v1/holder-tokens` (write scope) over HTTP.
- `server.HolderHandler`: mountable sub-router with exactly the three read
  endpoints (no admin routes) for library hosts; ledgerd exposes the same
  surface behind `HOLDER_TOKEN_SECRET`.
- `display_label` on classifications + journal types (migration 038,
  expand-only) with preset-seeded user-facing defaults; `SetDisplayLabelIfEmpty`
  never clobbers operator overrides.

### npm package — Added (wallet surface)

- `@azex/ledger-react/wallet` (shadcn skin), `/wallet/heroui` (HeroUI v3),
  `/wallet/headless` (client + hooks): `WalletPanel`, `WalletBalances`,
  `WalletBalanceCard`, `TransactionList`; `getToken` callback auth with
  single 401 refresh-retry; rendered-surface tests pin that no double-entry
  vocabulary reaches the DOM. Version 0.5.1 (aligned with the Go module).

Production-hardening batch (2026-07-06): closes the operational gaps between
"code-complete" and "runnable in production" — credential model, dashboard
auth, backup/DR, alerting closure, data lifecycle, deploy hygiene.

### Go module — Added
- **Scoped API keys** (BREAKING): `API_KEYS` is now comma-separated
  `name:scope:secret` triples, scope `read` < `write` < `admin`; every route
  group enforces `requireScope`, insufficient scope returns bizcode `10150`
  (403), and the key *name* is attached to access-log lines for audit.
  Malformed `API_KEYS` fails boot. (`server/middleware_auth.go`, docs/api.md)
- **Active monthly partitioning** (BREAKING migration 037): journal_entries
  rows move from the default partition into named monthly partitions
  (`journal_entries_yYYYYmMM`); a new advisory-locked worker `partition` job
  keeps `PartitionMonthsAhead` (default 3) months pre-created and rebalances
  stranded default rows (fallback gated on SQLSTATE 23514). I-13 is now an
  active process; archival guidance in RUNBOOK §11.
- **OTLP tracing bootstrap**: `OTEL_EXPORTER_OTLP_ENDPOINT` enables an
  OTLP/HTTP batching exporter behind pkg/otel (flushed on shutdown);
  unset = no-op as before. (`cmd/ledgerd/tracing.go`)
- **MIGRATE_MODE** (`auto`|`only`|`off`) decouples schema migrations from
  pod startup; Helm `migrations.job.enabled` runs them from a
  pre-install/pre-upgrade hook Job so serving pods need no DDL privileges.

### Go module — Fixed
- `BenchmarkReserveSettle` seeded funds into a role-less classification, so
  `Reserve` (role=available only) always failed — bench-only, CI never runs
  benchmarks. Now seeds a dedicated available-role wallet.

### Go module — Security
- **Trusted-proxy client-IP resolution** (BREAKING): replaced the
  `TRUST_PROXY_HEADERS` boolean with `TRUSTED_PROXY_CIDRS`, a comma-separated
  list of trusted edge-proxy CIDR ranges. Proxy headers (`X-Forwarded-For`
  walked right-to-left skipping trusted hops, then `X-Real-IP` /
  `True-Client-IP`) are honored **only** when the socket peer is inside a
  configured range, and every candidate is `netip`-validated — so a direct
  caller can no longer spoof its IP past the rate limiter or into access logs,
  and non-IP garbage can no longer create unbounded rate-limiter buckets.
  Migration: deployments that set `TRUST_PROXY_HEADERS=true` must set
  `TRUSTED_PROXY_CIDRS` to their ingress/proxy ranges instead; an invalid value
  fails boot. (`server/middleware_realip.go`, `server/server.go`,
  deploy/helm, docs/RUNBOOK.md, README.md)

### @azex/ledger-react + web — Changed
- **Dashboard credential model** (BREAKING): `NEXT_PUBLIC_API_KEY` /
  `NEXT_PUBLIC_API_URL` are gone. The browser talks to a same-origin BFF
  proxy (`/api/v1/[...path]`) that holds `LEDGER_API_KEY` server-side;
  pages are gated by `DASHBOARD_PASSWORD` login (HMAC session cookie,
  `proxy.ts`), with sign-out in the sidebar (`Sidebar` gains an optional
  `footer` slot — additive).

### Ops & docs
- `docs/DR.md`: PITR strategy, RPO/RTO targets, restore drill with
  invariant-based verification (reconcile full + solvency), quarterly drill.
- `docs/CAPACITY.md`: measured baseline (PostJournal ~2.5ms, GetBalance
  ~0.7ms, Reserve→Settle ~2.7ms serial), sizing rules, suggested SLOs.
- Helm: ServiceMonitor + PrometheusRule (alerts mapped 1:1 to RUNBOOK
  scenarios), PDB (on by default), HPA + NetworkPolicy (opt-in); Grafana
  dashboard in `deploy/grafana/`.
- CI: govulncheck (pinned), Trivy image scan (fails on fixed
  CRITICAL/HIGH), SPDX SBOM artifact, dependabot (gomod/npm/actions).
- RUNBOOK: §10 rewritten for the scoped-key model, §11 partition
  management/archival, §5 dead-letter handling + events retention policy.

## [0.4.1] - 2026-07-03

### Go module — Fixed
- **Migrations 025 and 031 failed on any database that already contained
  rows** — the class of database every upgrading library consumer has. Plain
  CI only ever migrates empty databases, where both bugs are invisible
  (caught by armatrix's upgrade rehearsal):
  - 025's `journal_entries.effective_at` backfill UPDATE was rejected by the
    018 append-only row trigger (0 rows = trigger never fires). The backfill
    now disables/re-enables the trigger around the one-time statement.
  - 031's `ADD COLUMN uid UUID NOT NULL` (no DEFAULT) fails on non-empty
    tables. Every table now uses add → `gen_random_uuid()` backfill →
    `SET NOT NULL`. Pre-existing rows get v4 uids (uniqueness is the only
    property the contract needs; v7 time-ordering remains a Go-side nicety
    for new rows).
- New pin: `TestMigrate_PopulatedDatabase` migrates to v24, seeds rows into
  every entity table, then runs the rest — the populated-database upgrade
  path is now CI-covered. `postgres.NewMigrationSource` and
  `postgrestest.SetupRawDB` are exposed for such stepwise migration tests.

## [0.4.0] - 2026-07-03

API-contract alignment (design: docs/plans/2026-07-03-api-contract-alignment-design.md).
No-legacy premise (Aaron's 2026-07-03 ruling): the ledger is treated as a brand-new
library — single-step breaking migration, no compatibility shims, no backfill.

### Go module — Changed (BREAKING: uid-only identity)
- **uid (UUIDv7) is the only externally visible identifier** (migration 031). Every
  entity table gains `uid UUID NOT NULL` + unique index; uids are generated Go-side
  (UUIDv7) on insert — no DB default, so a write path that forgets one fails loudly.
  Internal BIGSERIAL ids survive only inside storage (PKs/FKs/locks/keyset cursors)
  and appear in **no public contract**, including the library-mode Go API.
- `core` entity structs expose `UID string`; all cross-references are `*UID string`
  (`Journal.EventUID`, `Booking.JournalUID`, `Entry.ClassificationUID`, …; "" = not
  linked). All interface signatures take/return uids (`GetJournal(uid string)`,
  `ReverseJournal(uid string, …)`, `GetBalance(holder, currencyUID, classificationUID)`, …).
- **Pagination**: every list interface returns `(items, nextCursor string, error)`;
  cursors are opaque base64 strings produced by the store (`AuditFilter.Cursor`,
  `BookingFilter.Cursor`, `EventFilter.Cursor` are now strings). HTTP responses carry
  `next_cursor` populated by the store, not recomputed by handlers.
- **HTTP API**: all path params are `{uid}`; query params renamed
  (`currency_id`→`currency_uid`, `classification_id`→`classification_uid`,
  `booking_id`→`booking_uid`); no request or response body carries an internal id.
  Pinned by a mechanical source scan (server contract test, invariant I-18).
- `channel.CallbackPayload.BookingID int64` → `BookingUID string`; EVM adapter
  parses `booking_uid` from webhook JSON.
- `service.ClassificationLister` is now a dimension port
  (`ClassificationDims` / `CurrencyIDByUID` / `CurrencyUIDByID`), implemented by
  `postgres.RollupAdapter`; rollup/reconcile math stays on internal ids and converts
  at the boundary.
- `ledger-cli` flags take uids (`--currency <uid>`, `--uid`, `--booking-uid`); the
  internal-id escape hatch was not kept — use psql for storage-level investigation.
- `postgrestest` seed helpers return uid strings; new `InternalID` helper resolves a
  uid back to the bigint id for raw-SQL test assertions.
- `docs/openapi.yaml` rewritten for the uid contract (info.version 0.4.0).

### @azex/ledger-react — Changed (BREAKING, 0.2.0)
- Regenerated `schema.ts`; all hand-written types use `uid: string` / `*_uid: string`
  ("" = not linked, no more `number | null`); `Entry` and `TemplateLine` no longer
  carry row ids. Client methods, hooks, and admin pages take uid strings; journal
  detail routing matches opaque uids (`/journals/{uid}`).

## [0.3.1] - 2026-07-03

### Fixed
- CI: period-close reopen test compared a nanosecond Go timestamp against its
  microsecond Postgres round-trip (passed on Darwin, failed on Linux runners);
  timestamps are now truncated to microseconds in tests.
- `@azex/ledger-react`: regenerated `src/client/schema.ts` from the v0.3
  `docs/openapi.yaml` (the codegen:check CI gate was failing).

## [0.3.0] - 2026-07-03

### Go module — Added (financial-core hardening, design: docs/plans/2026-07-02-financial-core-hardening-design.md)
- **Effective date** (migration 025): `journals.effective_at` / `journal_entries.effective_at`
  separate business date from posting date. Backdating allowed (future rejected, 5min
  tolerance); real-time balances stay posting-ordered; as-of reads (`ListBalancesAt`,
  trends, snapshots) switch to the effective axis.
- **Accounting period close** (migration 026): append-only `period_closes` line;
  posting before the line fails with `ErrPeriodClosed` (14009); reopen = append an
  earlier line (latest-row-wins, audited). `POST /periods/close`, `GET /periods/closes`.
- **Trial balance**: `GET /reports/trial-balance` + `TrialBalanceReader` +
  `ledger-cli trial-balance`, on the effective axis.
- **Currency exponent & money primitives** (migration 027): `currencies.exponent`
  (JPY=0 … wei=18); every write path rejects over-precise amounts with
  `ErrPrecisionExceeded` (14006) — never silently rounds. `core.Round` (4 modes),
  `core.Allocate` (largest-remainder, sum-preserving), `core.ConvertAt`.
  HTTP currency creation requires an explicit exponent (pointer DTO — 0 is legal).
- **Account policies** (migration 028): per-(holder[,currency[,classification]])
  freeze/close + `min_balance` floor (negative = credit limit, 0 = no overdraft,
  positive = dust floor). Frozen blocks consumption (Reserve + net-negative journals)
  while pending-deposit confirmation still lands; closed blocks both directions.
  `ErrAccountFrozen` (14007) / `ErrAccountClosed` (14008). Enforced inside the
  existing per-dimension advisory locks; policy changes are audit-logged.
- **Partial reversal** (migration 029): `ReverseJournalFraction(num/den)` — multiple
  partial reversals per journal, per-currency balanced via `Allocate`, cumulative
  conservation enforced under the original's row lock; `num==den` reverses the exact
  remainder. `POST /journals/{id}/reverse-partial`.
- **Partial settlement**: `SettlePartial` / `FinalizeSettlement` activate the
  `settling` reservation state; the unsettled remainder stays held against the
  balance (over-commit window closed); expired settling reservations auto-finalize.
  `POST /reservations/{id}/settle-partial`, `POST /reservations/{id}/finalize`.
- Invariants I-14 (effective-date consistency), I-15 (close line is a hard write
  barrier), I-16 (precision bounded by exponent), I-17 (account policy enforcement);
  I-2 revised for cumulative partial reversals; I-11 extended to settling holds.
- **Inbound webhook replay cache** (migration 030): identical callbacks resent
  inside the signature timestamp window are rejected with 409 (previously relied
  solely on downstream transition idempotency). Wired in service mode via
  `Server.SetWebhookNonceRecorder`; optional for library consumers.
- `Lifecycle.Version` field (0/1 equivalent today) — a hook for future
  lifecycle-shape evolution.

### Go module — Breaking (v0.3 cleanups)
- All `Metadata` fields are now `map[string]string` (`Booking`, `TransitionInput`,
  `Event`, `channel.CallbackPayload`) — matching journals/pending. Pre-existing
  JSONB rows with non-string values are read back as their compact JSON text.
- `Reserver.Settle` / `Reserver.SettlePartial` take `SettleInput` /
  `SettlePartialInput` structs (Input + Validate discipline).
- `@azex/ledger-react`: `createCurrency` requires `exponent`; the currencies
  form gains a required decimal-places field (0 is legal — JPY — so the field
  cannot default).

### Go module — Added
- **Audit / platform reads over HTTP** — the read capabilities previously only
  reachable via the library facade and `ledger-cli` are now HTTP endpoints:
  `GET /audit/journals` (by account or time range), `GET /audit/bookings/{id}/trace`,
  `GET /audit/journals/{id}/reversals`, `GET /platform/balances`,
  `GET /platform/solvency`, `GET /balances/trends`. All documented in
  `docs/openapi.yaml`.
- **Full reconciliation is now runnable in service mode** — the 10-check suite is
  wired into the background worker (`FULL_RECONCILE_INTERVAL`, default 1h, leader-
  elected) and exposed as `POST /reconcile/full`. Check #2 (fleet-wide
  checkpoint-vs-entries scan) is now a real keyset-paginated scan with a scan
  limit + timeout guard that reports partial coverage instead of false passes.
- `bizcode.Retryable(code)` + a `retryable` field on the HTTP error envelope —
  machine-readable retry semantics (retry only with the same idempotency key);
  contract documented in `docs/api.md`.
- Per-subscriber webhook delivery health: `webhook_subscribers` gains
  `last_status_code` / `last_error` / `last_attempt_at` (migration 024), written
  after every delivery attempt.
- Delivery / reconcile / rollup observability: new `core.Metrics` methods
  `EventDelivered`, `EventDeliveryFailed`, `EventDead`, `RollupItemFailed`,
  `ReconcileCheckResult`, implemented by `observability.PrometheusMetrics`.
- `journal_entries` primary key `(id, created_at)` (migration 022) and a
  covering index for `ListReservationsByAccount` (migration 023).

### Go module — Fixed
- `JournalInput.Validate` now rejects non-positive `currency_id` /
  `classification_id` at the domain boundary (previously only the DB FK caught it).
- `Settle` rejects non-positive and over-reserved amounts with
  `core.ErrInvalidInput` before hitting the DB constraint.
- `Lifecycle.Validate` rejects states unreachable from `Initial` (island states).
- Worker cleanup paths (`ReleaseRollupClaim`, advisory-lock release) now run on a
  detached 5s context so shutdown no longer strands claims until lease expiry.
- Expiration scans process the earliest-expiring items first
  (`ORDER BY expires_at`); expected multi-replica transition races log at Info.
- Added the missing down migration for 020.

### Go module — Breaking (v0.x)
- `server.New` / `server.NewWithConfig` take five new trailing dependencies
  (audit, platform balances, solvency, balance trends, full reconciler).
- `core.Metrics` has five new methods — implementations written from scratch
  must add them (embedding implementations are unaffected).
- `delivery.NewWebhookDeliverer` takes a `core.Metrics` argument;
  `delivery.SubscriberLister` gains `RecordDeliveryStatus`.

### Documentation
- `docs/RUNBOOK.md`: webhook delivery contract (at-least-once, retries reorder,
  consumers must dedupe on `X-Ledger-Event-ID`), fixed the subscriber-health
  troubleshooting SQL, and a new "unauthenticated reads" deployment-boundary
  section (also in `README.md`).
- `docs/INVARIANTS.md`: idempotency-key lifecycle note (I-3) and partition
  rollout status (I-13).
- `channel.Adapter`: replay-protection responsibility split documented.
- `docs/openapi.yaml` `info.version` now tracks the Go module version.

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

[Unreleased]: https://github.com/azex-ai/ledger/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/azex-ai/ledger/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/azex-ai/ledger/compare/v0.4.1...v0.5.1
[0.4.1]: https://github.com/azex-ai/ledger/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/azex-ai/ledger/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/azex-ai/ledger/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/azex-ai/ledger/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/azex-ai/ledger/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/azex-ai/ledger/releases/tag/v0.1.0
