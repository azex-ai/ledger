# Ledger Cookbook

Business recipes for modeling real product scenarios on top of the ledger's
primitives — currencies, classifications, templates, and reserve/settle. Every
recipe shows the T-accounts (double-entry) and a Go code skeleton against the
`ledger` facade.

A runnable end-to-end program lives in [`examples/credits-topup`](../examples/credits-topup);
the current example is deposit-only: **1 USDC buys 1,000 AI credits**. It covers
fixed-price, metered and incremental charges, plus zero-cost/failure release.
See its [integration notes](../examples/credits-topup/README.md) for exact outcomes,
retry rules and host responsibilities. Other recipes below describe optional
modeling choices, not modules installed by this example.

---

## Mental model (read this first)

Four ideas make every recipe below fall out mechanically:

1. **A currency is a modeling dimension, not just "money".** `USDC`, `credits`,
   `points`, a coupon balance, each fiat — all are just rows in `currencies`.
   "Support credits" = "add one currency". Nothing about credits is special.

2. **Every template execution is single-currency.** The template renderer applies one
   `currency_uid` to all lines (`core/template.go`). So anything cross-currency
   (buying credits with USDC) is modeled as **two
   single-currency journals**, each balancing on its own. Post them atomically
   (`ExecuteTemplateBatch`, or two `ExecuteTemplate` calls inside `RunInTx`) so a
   failed write cannot leave one journal without the other. This does not validate
   the rate: the host must derive and validate the quoted amounts.

3. **Classifications are shared across currencies.** The same `main_wallet`,
   `settlement`, `equity` classifications work for every currency. A user's
   credits balance is `main_wallet` in currency `credits`; their USDC balance is
   `main_wallet` in currency `USDC`. Balances are keyed by
   `(holder, currency_uid, classification_uid)`.

4. **Holder sign encodes user vs system.** Positive holder = a user account;
   negative = the system counterpart (`core.SystemAccountHolder(userID)`).
   Templates map `HolderRoleUser`/`HolderRoleSystem` to the sign automatically.

### The system accounts you'll use (all preset)

| Classification | Normal side | Meaning |
|---|---|---|
| `main_wallet` | debit (user) | a user's spendable balance in a given currency |
| `custodial` | credit (system) | assets the platform actually holds (real USDC in custody) |
| `settlement` | credit (system) | per-currency net exposure absorber for FX journals |
| `equity` | credit (system) | platform equity — the funding source for giveaways/bonuses |
| `spread` | credit (system) | price-differential revenue (markup earned on conversion) |
| `fee_revenue` | credit (system) | fee income |

Install them once: `svc.InstallExtendedPresets(ctx)` (idempotent).

### Invariants the ledger enforces for you

- **Per-currency balance**: each journal must balance *within its currency*
  (DB trigger + Go validator). Two journals can each balance at an incorrect
  exchange rate. The host owns quote validation and amount derivation; see
  `postgres.TestFX_LedgerDoesNotCheckTheRate`.
- **Append-only**: journals are never mutated. Corrections are new **reversal**
  journals (`ReverseJournal`). Refunds below use this.
- **Idempotency**: every mutation needs an `idempotency_key`; same key + same
  payload returns the original result, divergent payload raises `ErrConflict`.
  Use `ledger.NewIdempotencyKey("scope")`.

---

## Recipe 1 — Top up and buy credits at 1 USDC : 1,000 credits

**Scenario:** after a confirmed 1 USDC deposit, the user purchases 1,000 credits.
Pending deposits are not spendable. The crypto adapter's confirmed event records
`DR user.main_wallet(USDC) 1 / CR custodial(USDC) 1`; a browser-supplied amount is
not evidence of a deposit.

In one `RunInTx`, reserve and settle 1 USDC, then execute both templates:

```
fx_sell  (USDC, amount = 1)
    CR user.main_wallet(USDC)       1
    DR system.settlement(USDC)     1

fx_buy   (CREDITS, amount = 1000)
    DR user.main_wallet(CREDITS) 1000
    CR system.settlement(CREDITS) 1000
```

The host computes `usdcAmount.Mul(decimal.NewFromInt(1000))`, supplies the shared
purchase ID and pricing version as metadata, and derives stable `:reserve`,
`:settle`, `:pay` and `:issue` keys from the persisted purchase ID. The
[example's `purchaseCredits`](../examples/credits-topup/main.go) is the executable
composition. Both journals, the reservation and settlement roll back together.
All competing purchases must use reservations too; a raw journal can bypass a
hold. Configure a zero balance floor as an additional overdraft control.

Each currency balances independently. After purchase, the user has **0 USDC and
1,000 credits**. Custodial USDC remains 1: conversion has not sent it anywhere.
`settlement(USDC)` is -1 and `settlement(CREDITS)` is +1000 (credit-normal).
Consumption debits the credits settlement account and reduces the user's credits.
These are operational ledger positions, not an automatic fiat revenue-recognition
or provider-cost system. Do not sum USDC and credits or call credits custodial
onchain assets.

---

## Recipe 2 — Discounts (three shapes, pick per business need)

### (a) Price discount — "20% off: 0.8 USDC buys 100 credits"

Nothing structural changes. The discount **is** the rate: charge less USDC for
the same credits. Record the promo in metadata for reporting.

```
fx_sell  (USDC,    amount = 0.8)     CR user.main_wallet(USDC) 0.8  / DR settlement(USDC) 0.8
fx_buy   (credits, amount = 100)     DR user.main_wallet(credits) 100 / CR settlement(credits) 100
metadata: {promo_code: "SAVE20", list_price_usdc: "1.0", charged_usdc: "0.8"}
```

The platform simply realizes less USDC per issued credit. No extra accounts.

### (b) Bonus credits — "top up 100, get 20 free" (platform-funded)

The user pays for 100 credits; the platform *gives* 20 more out of its own
equity. This needs a credits journal with **two amount keys** and `equity` as
the funding counterparty. Register the template once (this is also the general
pattern for "extend the ledger with a new business template"):

```
credits_topup  (currency = credits)
    DR user.main_wallet(credits) 100   ← purchased  (CR settlement — the paid entries)
    DR user.main_wallet(credits)  20   ← bonus      (CR equity — platform-funded)
    CR settlement(credits)       100
    CR equity(credits)            20
user ends with +120 credits; equity(credits) shows the 20 promo cost.
```

```go
// one-time setup: register a journal type + template with purchased & bonus keys
jt, _ := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
    Code: "credits_topup", Name: "Credits Top-up with Bonus"})
mw, _ := svc.Classifications().GetByCode(ctx, "main_wallet")
st, _ := svc.Classifications().GetByCode(ctx, "settlement")
eq, _ := svc.Classifications().GetByCode(ctx, "equity")
_, _ = svc.Templates().CreateTemplate(ctx, core.TemplateInput{
    Code: "credits_topup", Name: "Credits Top-up with Bonus", JournalTypeUID: jt.UID,
    Lines: []core.TemplateLineInput{
        {ClassificationUID: mw.UID, EntryType: core.EntryTypeDebit,  HolderRole: core.HolderRoleUser,   AmountKey: "purchased", SortOrder: 1},
        {ClassificationUID: st.UID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "purchased", SortOrder: 2},
        {ClassificationUID: mw.UID, EntryType: core.EntryTypeDebit,  HolderRole: core.HolderRoleUser,   AmountKey: "bonus",     SortOrder: 3},
        {ClassificationUID: eq.UID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "bonus",     SortOrder: 4},
    },
})

// per top-up (the paid USDC journal is a separate fx_sell as in Recipe 1):
_, err := svc.JournalWriter().ExecuteTemplate(ctx, "credits_topup", core.TemplateParams{
    HolderID: userID, CurrencyUID: creditsUID, IdempotencyKey: ledger.NewIdempotencyKey("topup"),
    Amounts: map[string]decimal.Decimal{
        "purchased": decimal.RequireFromString("100"),
        "bonus":     decimal.RequireFromString("20"),
    },
})
```

### (c) Dedicated promo account

Same as (b) but point the bonus counterparty at a purpose-built `promo` system
classification instead of `equity`, so promo cost is tracked separately from
general equity. Create it with `CreateClassification(code:"promo",
NormalSide: credit, IsSystem: true)` and swap `eq.UID` for `promo.UID` above.

**Choosing:** (a) when the discount is just a lower price; (b) when you literally
hand out extra units and want it in equity; (c) when finance wants promo spend
on its own line.

---

## Recipe 3 — Adding currencies for new business scenarios

Every new value type is one `CreateCurrency` call; it reuses all the
classifications and templates above.

```go
points,  _ := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "POINTS", Name: "Loyalty Points", Exponent: 0})
coupon,  _ := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "COUPON", Name: "Coupon Balance", Exponent: 2})
eur,     _ := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "EUR",    Name: "Euro", Exponent: 2})
```

`Exponent` is required and is not "money-shaped" — it's a small integer
declaring how many decimal places entries in that currency may carry (see
Recipe 7 below). Points are whole numbers (0), coupon/fiat use the
conventional 2. Getting this wrong up front means every future entry in that
currency is bounded by it: the ledger rejects (never rounds) an over-precise
amount.

- **Loyalty points**: issue with `DR main_wallet(POINTS) / CR equity(POINTS)`
  (points are platform-funded); redeem with the reverse.
- **Coupons**: same issuance pattern; "spend a coupon toward credits" is a
  two-journal FX `COUPON → credits`.
- **Multi-fiat**: `EUR`, `USD`, … each balance independently; cross-fiat is a
  two-journal FX with `spread` capturing your markup.

Each currency balances on its own — a bug in POINTS accounting can never
corrupt USDC. This isolation is why "just add a currency" is safe.

---

## Recipe 4 — Spending credits: reserve → settle

**Scenario:** a job may cost up to N credits; you hold a budget, run the job,
then capture the actual cost and release the remainder. This is the safe pattern
for metered consumption (an AI generation run, an API call quota, etc.).

`available = balance − remaining holds of active/settling reservations`. `Reserve` takes a per-(holder,
currency) advisory lock and checks availability (TOCTOU-safe). `Settle` closes
the hold at the actual amount and **auto-releases the unused remainder back
into `available`** — both of those are reservation bookkeeping, atomic within
the reservation row. Neither writes a journal entry: **`Settle` moves no
money.** If the spend needs to hit the books, that is a separate journal you
post yourself (next block).

```go
// hold up to 50 credits
rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
    AccountHolder: userID, CurrencyUID: creditsUID,
    Amount:        decimal.RequireFromString("50"),
    IdempotencyKey: ledger.NewIdempotencyKey("run-budget"),
    ExpiresIn:      time.Hour,
})

// run finishes; actual cost was 32 credits → 18 released back into available.
// The debit journal below is what actually charges the user -- Settle alone
// would close the hold and charge nobody (examples/billing used to make
// exactly this mistake; see its history if you want the full account).
// Settle and the journal run in one RunInTx: a crash between them would
// otherwise release the hold without the charge landing, and the ledger
// would report success because from its side nothing failed.
// Create these once per logical usage event and persist/reuse on retries.
settleKey := ledger.NewIdempotencyKey("run-settle")
spendKey := ledger.NewIdempotencyKey("run-spend")
err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
    if err := tx.Reserver().Settle(ctx, core.SettleInput{
        ReservationUID: rsv.UID, Amount: decimal.RequireFromString("32"),
        IdempotencyKey: settleKey,
    }); err != nil {
        return err
    }
    _, err := tx.JournalWriter().ExecuteTemplate(ctx, "credits_spend", core.TemplateParams{
        HolderID: userID, CurrencyUID: creditsUID,
        IdempotencyKey: spendKey,
        Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("32")},
    })
    return err
})
```

- Reserve does **not** move the balance — it's a soft lock reducing *available*.
  `Settle` does not move it either. Post the actual debit journal (credits
  leaving debit-normal `main_wallet` via a credit entry, with a matching debit
  to `settlement`, as the example's `credits_spend` template defines) in the same
  `RunInTx` as the `Settle` call — see `examples/credits-topup` for the
  runnable version of the block above.
- `ExecuteTemplate` called directly inside `RunInTx` (as above) always posts
  `auth_status=unsigned_tx_mode` — there is no point inside an already-open
  transaction where calling out to a configured `Attestor` would not itself
  be the "external call inside a DB transaction" `financial.md` forbids. If
  this service was constructed `WithAttestor` and something downstream calls
  `RequireVerifiedBalance` on this dimension, that gate refuses to pay it
  out. The fix is `svc.AuthorizeTemplate` **before** `RunInTx` opens, then
  `tx.JournalWriter().PostAuthorized(...)` inside it instead of
  `ExecuteTemplate` — see `examples/tamper-evident`'s appendix for a
  runnable, asserted demonstration of both paths side by side.
- To abandon a hold explicitly (job never ran):
  ```go
  svc.Reserver().Release(ctx, core.ReleaseInput{
      ReservationUID: rsv.UID,
      IdempotencyKey: ledger.NewIdempotencyKey("run-abandon"),
  })
  ```

---

## Recipe 5 — Correcting a credits charge

Credits in the current integration are consumed for services. Cash-out and
onchain withdrawals are outside this example. Existing generic FX/withdrawal
library capabilities are not enabled by this recipe.

### Refunding a specific charge — use a reversal, never a hand-written "undo"

If you need to void a prior journal (bad charge, disputed purchase), post a
**reversal**. It creates a mirror-image journal linked to the original via
`reversal_of`; the original row is never touched (append-only invariant).

```go
rev, err := svc.JournalWriter().ReverseJournal(ctx, originalJournalUID, "customer refund #4821")
```

For a fully voided consumption charge, a reversal restores the original credits
and leaves USDC unchanged. For part of a charge, use the existing fractional API:

```go
rev, err := svc.JournalWriter().ReverseJournalFraction(ctx, originalJournalUID,
    1, 4, "quarter refund #4821", "refund:4821")
```

Each fraction applies to the original journal. The ledger serializes concurrent
reversals, enforces cumulative per-entry caps and detects changed-payload replays.
Use `1, 1` to reverse exactly the remaining amount after earlier partial refunds,
including rounding residue; `ReverseJournal` rejects a journal with prior refunds.
The host still decides eligibility, approval and business refund limits.
Refunding a purchase is different: previously consumed credits and any bonus must
be accounted for before undoing its paired journals.

This is the correction mechanism — do not `UPDATE`/`DELETE`
journal rows, and do not synthesize an inverse by hand (you'll drift from the
original's amounts/rounding).

---

## Recipe 6 — Expiry and insufficient funds (the safe edges)

### Insufficient balance is an error, not a silent zero

`Reserve` returns `core.ErrInsufficientBalance` when available funds do not
cover the budget. Direct journal debits only have an overdraft floor when an
account policy with `EnforceMinBalance` is configured; they do not respect
reservation holds. Route all competing consumption through Reserve → Settle. Handle it explicitly — surface it to the caller;
never swallow it into a default/zero.

```go
_, err := svc.Reserver().Reserve(ctx, in)
if errors.Is(err, core.ErrInsufficientBalance) {
    return fmt.Errorf("not enough credits for this run: %w", err)
}
```

### Expiry is handled by the ledger's worker — don't reimplement it

Reservations carry `expires_at` (default 15m, set via `ExpiresIn`). The ledger's
`Worker` runs an expiration loop (default every 30s) that finds
`status='active' AND expires_at < now()` reservations and calls the **proper
`Release`** — restoring the user's available balance immediately. Expired
bookings transition to `expired` only when their classification lifecycle allows
it (a JOIN + state-machine check), so an in-flight run is never force-closed.

**You must actually run the worker** for this to happen:

```go
w, err := svc.Worker(service.WorkerConfig{}) // sensible defaults incl. ExpirationInterval
if err != nil {
    log.Fatal(err)
}
go func() {
    // Run only errors for a misconfiguration -- most commonly a Service
    // built without ledger.WithLogger (see README's "Background worker"
    // Quick Start section): every signal this worker produces, this error
    // included, travels over core.Logger and nowhere else, so a worker
    // that "started" under the silent default is indistinguishable from
    // one that never started -- which is exactly the failure mode this
    // recipe exists to warn against, just for the expiration job instead
    // of the reservation itself.
    if err := w.Run(ctx); err != nil {
        log.Fatal(err)
    }
}() // your composition root owns the worker's lifetime
```

> **Anti-pattern (do not do this in a consumer):** sweeping stale holds by
> directly flipping your own mapping table to `closed` on a wall-clock timer,
> *assuming* the ledger already released the reservation. If the worker isn't
> running (or your timer is shorter than the reservation TTL), the ledger
> reservation stays `active` and, once your mapping row is closed, nothing ever
> calls `Release` — the funds are **locked forever**. Always drive release
> through the ledger's `Release` (idempotent, state-machine-guarded), *then*
> update your mapping.

---

## Recipe 7 — Precision, rounding, and who eats the remainder

### The rule: the ledger rejects, it never rounds

`currencies.exponent` bounds how many decimal places an entry may carry
(JPY=0, USD=2, USDC=6, wei=18 — see `docs/INVARIANTS.md` I-16). Every write
path (`PostJournal`, `ExecuteTemplate`, `Reserve`, `AddPending`, ...) checks
every amount against its currency's exponent and returns
`core.ErrPrecisionExceeded` if it's over-precise. **It never silently rounds
or truncates for you.** If your business logic produces a number with more
decimal places than the target currency allows, that's a bug in the caller —
round explicitly, before you call the ledger, using `core/money.go`.

### Rounding decision table

| Scenario | Mode | Why |
|---|---|---|
| Displaying a price, computing a one-off fee, most user-facing totals | `RoundHalfUp` | Conventional "5 rounds up" behavior users expect. |
| Aggregating many small roundings over time (e.g. per-transaction fee accrual) | `RoundHalfEven` | Ties resolve toward even digits, so repeated rounding doesn't drift the sum in one direction. |
| Positive debit must not exceed the computed charge | `RoundDown` | Truncating toward zero reduces the charged amount; it can under-charge. |
| Positive credit must not be smaller than the computed entitlement | `RoundUp` | Rounding away from zero increases the credited amount. For a positive fee it instead increases the charge. |

```go
fee, err := core.Round(rawFee, feeCurrency.Exponent, core.RoundHalfEven)
if err != nil {
    return err // the amount is outside what NUMERIC(30,18) can hold (I-70)
}
```

### Who eats the remainder: FX and splits

**FX (currency conversion)** — call `core.ConvertAt` yourself before posting;
the `fx_sell`/`fx_buy` template pair (`presets/fx.go`) does **not** convert
for you, it just posts whatever amount you give it on each journal:

```go
// Converting 100 USDC -> CNY at a quoted rate, rounding to CNY's own exponent.
cnyAmount, err := core.ConvertAt(decimal.RequireFromString("100"), rate, cnyCurrency.Exponent, core.RoundHalfUp)
if err != nil {
    return err // amount or rate is outside what NUMERIC(30,18) can hold (I-70)
}

key := ledger.NewIdempotencyKey("fx-convert")
_, err = svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
    {TemplateCode: "fx_sell", Params: core.TemplateParams{
        HolderID: userID, CurrencyUID: usdcUID, IdempotencyKey: key + "-sell",
        Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("100")},
    }},
    {TemplateCode: "fx_buy", Params: core.TemplateParams{
        HolderID: userID, CurrencyUID: cnyUID, IdempotencyKey: key + "-buy",
        Amounts: map[string]decimal.Decimal{"amount": cnyAmount},
    }},
})
if err != nil { return err }
```

This fragment demonstrates rounding and atomic posting. For spendable user
funds, compose the USDC Reserve/Settle with this batch in one `RunInTx`, as
Recipe 1 does, and retain the operation keys across retries.

Any residue between the "ideal" rate-implied amount and what the two journals
actually post is the platform's, by construction: `settlement` absorbs the
net exposure (see `presets/fx.go`'s "Net effect on system books" comment). The
caller decides the rounding mode; the ledger never adjusts a journal to make it
"come out even" on your behalf.

**Splitting one total across several accounts** (e.g. a fee split across
revenue-share partners) — use `core.Allocate`, not manual division. It
guarantees the shares sum to exactly the total (no cent lost or manufactured)
via the largest-remainder method:

```go
shares, err := core.Allocate(totalFee, []decimal.Decimal{partnerAWeight, partnerBWeight}, feeCurrency.Exponent)
// sum(shares) == totalFee is guaranteed; the odd cent goes to whichever
// share had the largest truncated remainder (deterministic, not random).
```

`Allocate` requires `total` to already be exact at the target exponent —
round it first with `core.Round` if it might not be (e.g. it came from a
`ConvertAt` at a different exponent).

---

## Recipe 8 — Retroactive posting and period close

### Backdating a journal to its real business date

`JournalInput.EffectiveAt` (and `TemplateParams.EffectiveAt`) lets you attribute
a journal to a date other than "now" — a delayed on-chain confirmation, an
invoice for last month's usage. Leave it zero for the common case (attribute
to now); set it explicitly for retroactive posting:

```go
_, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{
    JournalTypeUID: depositJTUID,
    IdempotencyKey: idemKey,
    EffectiveAt:    lastMonthEnd, // business date — write time (created_at) is still "now"
    Entries:        entries,
})
```

`effective_at` never affects real-time balances (`checkpoint + delta` still
rolls by insertion order) — it only changes which "as of" bucket a reporting
query (trial balance, balance trends, daily snapshots) attributes the entry
to. It's rejected if more than 5 minutes in the future — this is backdating,
not scheduled posting.

### Closing a period, and correcting a closed period

`svc.PeriodCloser().ClosePeriod` appends a close line: any `PostJournal` (or
template, or reversal) whose `EffectiveAt` predates the active line is
rejected with `core.ErrPeriodClosed`.

```go
_, err := svc.PeriodCloser().ClosePeriod(ctx, core.ClosePeriodInput{
    CloseBefore: monthEnd,
    Note:        "March 2026 close",
    ActorID:     opsUserID,
})
```

**You cannot fix a closed period by rewriting history** (I-2 forbids UPDATE/
DELETE on journals). The correction pattern is: reverse the original journal
— the reversal's `EffectiveAt` always defaults to *now* (it never inherits
the original's), so it lands in the currently open period — then, if needed,
post a fresh corrected journal, also dated in the open period:

```go
_, err := svc.JournalWriter().ReverseJournal(ctx, originalJournalUID, "March closed, correcting in April")
// then re-post the correct entries with today's date
```

Reopening a period (e.g. an auditor found something after close) is done by
closing again with an earlier `CloseBefore` — this is a normal, audited
append (the full close-line history is kept, nothing is overwritten):

```go
_, err := svc.PeriodCloser().ClosePeriod(ctx, core.ClosePeriodInput{
    CloseBefore: earlierDate,
    Note:        "reopened for audit correction",
    ActorID:     opsUserID,
})
```

### Trial balance as the close-readiness check

Before closing a period, run the trial balance for the cutoff you're about to
close — `balanced: true` and `total_debit == total_credit` is the signal the
books are internally consistent as of that date:

```go
report, err := svc.TrialBalanceReader().TrialBalance(ctx, currencyUID, monthEnd)
if !report.Balanced {
    return fmt.Errorf("trial balance off by %s, do not close", report.TotalDebit.Sub(report.TotalCredit))
}
```

---

## Recipe 9 — Crypto deposit + sweep (CREATE2 shared-address custody)

**Scenario:** users deposit USDC on an EVM chain to a per-holder
custody address you control, without asking them to pick a memo/tag; you
periodically sweep collected funds to a treasury address. Full design:
`docs/plans/2026-07-11-crypto-deposit-sweep-design.md`.

This is an **optional add-on** — install it only if you want ledger to own
address issuance + ingestion + sweep orchestration. It composes cleanly on
top of the deposit accounting bundle (Recipe 1's `deposit_confirm` journal is
exactly what a confirmed crypto deposit posts).

### 1. Install the bundle

```go
classStore := postgres.NewClassificationStore(pool)
tmplStore := postgres.NewTemplateStore(pool)
// The journal-type port must be the adapter, not the ClassificationStore
// itself: both types have a SetDisplayLabelIfEmpty with the same signature,
// and the installer calls it on the journal-type side (presets/templates.go).
// Passing classStore directly compiles and silently labels classifications
// instead. `ledger.New` wires it the same way (see ledger.go's JournalTypes()).
jtStore := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
if err := presets.InstallCryptoDepositBundle(ctx, classStore, jtStore, tmplStore); err != nil {
    return err
}
```

This installs the standard deposit accounting classifications/journal types
(same as `presets.DepositBundle()`) **plus** the `sweep` booking lifecycle
(`pending -> sent -> confirmed | failed(-> retry -> pending)`). Sweep gets no
journal type/template — it never touches the accounting equation (see
INVARIANTS I-19).

### 2. Configure your `ChainSet`

One `core.ChainConfig` per EVM chain you accept deposits on, keyed by chain
ID. `Factory`/`InitHash` must be the same CREATE2 deployment fingerprint on
every chain in the set — that's what makes a holder's address identical
across all of them:

```go
chainSet := core.ChainSet{
    1: { // Ethereum mainnet
        ChainID: 1, Confirmations: 12,
        ScanStartBlock: 19_000_000, // DepositFactory deployment block
        Factory: "0x...", InitHash: "0x...", // your azex-contracts DepositFactory deployment
        CreditTokens: map[string]core.TokenConfig{
            "0xusdc...": {TokenAddress: "0xusdc...", CurrencyCode: "USDC", Decimals: 6},
        },
        SweepTokens: map[string]core.TokenConfig{
            "0xusdc...":         {TokenAddress: "0xusdc...", CurrencyCode: "USDC", Decimals: 6},
            core.SweepNativeToken: {TokenAddress: core.SweepNativeToken, CurrencyCode: "ETH", Decimals: 18},
        },
    },
}
```

Set `ScanStartBlock` to the `DepositFactory` deployment block. The durable
address-registration rescan starts there; leaving it at zero is valid but may
cause an unnecessarily expensive genesis-to-tip scan.

`CreditTokens` is the deposit-side allowlist (what credits a holder's
balance); `SweepTokens` is independent and may include native assets that are
collected to treasury but never credited to any holder (unattributed —
handled as its own reconciliation category, design doc §5-4).

### 3. Issue a deposit address per holder

```
POST /api/v1/holders/1001/deposit-address
-> {"code":200,"message":null,"data":{"uid":"...","account_holder":1001,"address":"0xB3e7...","created_at":"..."}}
```

Idempotent — call it again any time and you get the same address back. In
library mode this is `svc.Onchain().EnsureDepositAddress(ctx, holder)`.

### 4. Wire the ingestion paths

Both the watcher (pull — polls `eth_getLogs` for Transfers into registered
addresses) and the onchain webhook (push — `POST
/api/v1/webhooks/evm`) converge on the same `IngestDeposit` orchestration.
You do not choose one or the other; run the watcher for completeness and the
webhook as a low-latency accelerant if your indexer provider offers one.

Booking idempotency key = `deposit-{chain_id}-{tx_hash}-{txlog_seq}` — safe
to observe the same transfer any number of times across both paths and
across a reorg (INVARIANTS I-20).

### 5. Sweep job

Configure a `core.SweepPolicy` per `(chain_id, token)` — minimum threshold
(well above per-address gas cost, or dust becomes a standing drain), gas
ceiling, batch limit, and interval. The sweep job batches registered
addresses' balances to the factory's treasury; each batch is a `sweep`
booking (no journal — see Recipe intro), `channel_ref` = the sweep tx hash.

### 6. Reconciliation stays honest about unattributed funds

Treasury's on-chain balance will legitimately exceed the sum of holder
liabilities by the unattributed amount (native deposits, non-allowlisted
tokens swept but never credited). `ledger-cli reconcile` / solvency checks
must carve this out as its own line — don't let it silently mask a real
shortfall. See design doc §5-4.

### 7. M3 compensating controls — threshold gate + reconciliation → human review

> ⚠️ **`AutoCreditCeiling` is REQUIRED, not opt-in.** For every
> `(chain, token)` you configure in `CreditTokens`, you must set
> `AutoCreditCeiling` — either to a positive cap, or to
> `core.UnboundedAutoCredit` if you deliberately accept unbounded
> single-source-RPC trust. Leave it unset and `svc.Onchain().Run(ctx)`
> refuses to start: this is a startup error, not a silently-disabled gate.
> `ReconcileCeiling`, below, is genuinely optional — zero disables only the
> reconciliation gate, not the mint-exposure cap.

RPC is the deposit path's single trusted oracle by default (the watcher's
own `ChainReader`) — the M3 add-on layers two independent compensating
controls on top before a deposit is ever auto-credited, and a
human-review surface for whatever they catch. Design:
`docs/plans/2026-07-11-crypto-deposit-sweep-design.md` §9.

**Threshold gate** — set `AutoCreditCeiling` per `(chain, token)` in
`core.TokenConfig`. Any single deposit above it is parked in `review`
instead of auto-confirmed, no matter how many confirmations it has:

```go
CreditTokens: map[string]core.TokenConfig{
    "0xusdc...": {
        TokenAddress: "0xusdc...", CurrencyCode: "USDC", Decimals: 6,
        AutoCreditCeiling: decimal.NewFromInt(10_000), // > 10k USDC -> review
    },
},
```

**Reconciliation gate** — wire a second, independent confirmation source
(a different RPC provider, a block explorer API, your own indexer — anything
that answers "was this tx included, and for how much?" without sharing a
failure mode with your primary watcher) via `service.WithDepositConfirmer`,
and set `ReconcileCeiling` to the amount above which it's worth the extra RPC
round trip:

```go
type explorerConfirmer struct{ client *explorerClient } // your own adapter

func (c *explorerConfirmer) ConfirmDeposit(ctx context.Context, chainID int64, txHash string, txLogSeq int32) (amount decimal.Decimal, included bool, err error) {
    // query a *different* RPC/indexer than your primary watcher's Reader
}

onchain := service.NewOnchain(deps, chainSet,
    service.WithDepositConfirmer(&explorerConfirmer{client: secondProviderClient}),
)
```

```go
CreditTokens: map[string]core.TokenConfig{
    "0xusdc...": {
        TokenAddress: "0xusdc...", CurrencyCode: "USDC", Decimals: 6,
        ReconcileCeiling:      decimal.NewFromInt(1_000), // > 1k USDC -> double-check
        ReconcileFailureLimit: 3,                         // 3 consecutive second-source errors -> review
    },
},
```

A deposit at or below `ReconcileCeiling` never calls the second source at
all — small deposits aren't worth the extra RPC cost. Above it, a mismatch
(source disagrees on amount, or doesn't see the tx included) routes to
`review` instead of confirming, same as the threshold gate.

`ReconcileCeiling` defaults to zero (reconciliation gate disabled) — that
part of M3 is genuinely opt-in. `AutoCreditCeiling` is NOT: it has no zero
default (see the warning at the top of this section) — every `CreditTokens`
entry must set it to either a positive cap or `core.UnboundedAutoCredit`,
or `Run(ctx)` fails to start.

`ReconcileFailureLimit` (W3-A, mi5) follows `AutoCreditCeiling`'s pattern,
not `ReconcileCeiling`'s: whenever `ReconcileCeiling` IS positive AND
`WithDepositConfirmer` IS wired (the gate is actually active for that
token), `ReconcileFailureLimit` must be a positive integer too, or `Run(ctx)`
fails to start. It counts consecutive `ConfirmDeposit` *errors* (unreachable,
timeout, 5xx) — not amount mismatches, which already route to `review`
immediately — and escalates to `review` (reason `reconcile_unavailable`)
once the limit is hit, instead of retrying a persistently down second
source forever. If your token never activates the reconciliation gate at
all, this field is irrelevant and can be left at zero.

**Human review surface** — a deposit parked in `review` has zero ledger
effect (I-21: `journal_uid` stays empty) until a human resolves it. Wire the
review endpoints in alongside the address/ingestion services:

```go
srv.SetDepositReviewer(onchain) // *service.Onchain satisfies server.DepositReviewer
```

```
GET  /api/v1/deposits/reviews?limit=50            -- list the queue, oldest first
POST /api/v1/deposits/{uid}/review/approve         -- confirm + post the deposit_confirm journal
POST /api/v1/deposits/{uid}/review/reject
     { "reason": "..." }                           -- fail, no journal ever posted
```

See `docs/RUNBOOK.md` §13 for the on-call triage process. Both resolution
endpoints are idempotent (repeat calls on an already-resolved booking are a
no-op) and return a 409 conflict on any booking not currently in `review`.

Both resolution endpoints require an API key holding
`server.CapabilityDepositReview` (W3-A, mi2) — grant it via a `+`-joined
suffix on the key's scope (`reviewer:read+deposit_review:secret` in
`API_KEYS`). This is deliberately independent of scope: the key your
ingestion path uses (`write` scope, to call `POST /bookings` and
`POST /bookings/{uid}/transition`) does not get it automatically, so that
key alone cannot forge a deposit and then approve its own review. See
`docs/api.md`'s Authentication section for the full `API_KEYS` format.

---

## Running the example

```bash
export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
# Optional but recommended: migrations on their own credential — without it the
# example logs a warning and migrates on DATABASE_URL (README's Quick Start
# Prerequisite, docs/RUNBOOK.md §9 "Database roles").
export MIGRATE_DATABASE_URL="postgres://ledger_owner:pass@localhost:5432/ledger_dev?sslmode=disable"
go run ./examples/credits-topup
```

It walks Recipes 1, 2b, 4, and 5 end-to-end against a real PostgreSQL, printing
balances at each step. See [`examples/credits-topup/main.go`](../examples/credits-topup).
