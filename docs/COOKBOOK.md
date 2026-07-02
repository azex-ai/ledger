# Ledger Cookbook

Business recipes for modeling real product scenarios on top of the ledger's
primitives — currencies, classifications, templates, and reserve/settle. Every
recipe shows the T-accounts (double-entry) and a Go code skeleton against the
`ledger` facade.

A runnable end-to-end program lives in [`examples/credits-topup`](../examples/credits-topup);
this document explains the *why* behind it and covers the variants it doesn't run.

---

## Mental model (read this first)

Four ideas make every recipe below fall out mechanically:

1. **A currency is a modeling dimension, not just "money".** `USDT`, `credits`,
   `points`, a coupon balance, each fiat — all are just rows in `currencies`.
   "Support credits" = "add one currency". Nothing about credits is special.

2. **Every journal is single-currency.** The template renderer applies one
   `currency_id` to all lines (`core/template.go`). So anything cross-currency
   (buying credits with USDT, cashing credits back out) is modeled as **two
   single-currency legs**, each balancing on its own. Post them atomically
   (`ExecuteTemplateBatch`, or two `ExecuteTemplate` calls inside `RunInTx`) so a
   bad rate quote can never leave one leg without the other.

3. **Classifications are shared across currencies.** The same `main_wallet`,
   `settlement`, `equity` classifications work for every currency. A user's
   credits balance is `main_wallet` in currency `credits`; their USDT balance is
   `main_wallet` in currency `USDT`. Balances are keyed by
   `(holder, currency_id, classification_id)`.

4. **Holder sign encodes user vs system.** Positive holder = a user account;
   negative = the system counterpart (`core.SystemAccountHolder(userID)`).
   Templates map `HolderRoleUser`/`HolderRoleSystem` to the sign automatically.

### The system accounts you'll use (all preset)

| Classification | Normal side | Meaning |
|---|---|---|
| `main_wallet` | debit (user) | a user's spendable balance in a given currency |
| `custodial` | credit (system) | assets the platform actually holds (real USDT in custody) |
| `settlement` | credit (system) | per-currency net exposure absorber for FX legs |
| `equity` | credit (system) | platform equity — the funding source for giveaways/bonuses |
| `spread` | credit (system) | price-differential revenue (markup earned on conversion) |
| `fee_revenue` | credit (system) | fee income |

Install them once: `svc.InstallExtendedPresets(ctx)` (idempotent).

### Invariants the ledger enforces for you

- **Per-currency balance**: each journal must balance *within its currency*
  (DB trigger + Go validator). A mis-quoted FX rate makes one leg unbalanced and
  is rejected — it can never silently pass.
- **Append-only**: journals are never mutated. Corrections are new **reversal**
  journals (`ReverseJournal`). Refunds below use this.
- **Idempotency**: every mutation needs an `idempotency_key`; same key + same
  payload returns the original result, divergent payload raises `ErrConflict`.
  Use `ledger.NewIdempotencyKey("scope")`.

---

## Recipe 1 — Top up and buy credits at 1 USDT : 100 credits

**Scenario:** a user pays 1 USDT and receives 100 credits.

Two currencies (`USDT`, `credits`), one cross-currency conversion → FX two-leg.
Assuming the user already holds USDT in `main_wallet` (from a prior deposit),
the purchase is:

```
fx_sell  (currency = USDT,    amount = 1)
    CR user.main_wallet(USDT)   1        ← user gives up 1 USDT
    DR settlement(USDT)         1

fx_buy   (currency = credits, amount = 100)
    DR user.main_wallet(credits) 100     ← user receives 100 credits
    CR settlement(credits)       100
```

- Each leg balances **inside its own currency**. Rate `100` (and a shared
  `quote_id`) go in both legs' metadata so audit can stitch them back together.
- Post both legs **atomically** — one bad amount and the whole purchase rolls back.
- **`settlement(credits)` credit balance = total credits the platform has
  issued**, and is your reconciliation anchor for outstanding credit liability.
  `settlement(USDT)` debit balance = USDT taken in against those credits (sweep
  it into `custodial` to reflect real custody).

```go
// user already has USDT in main_wallet (e.g. via deposit_confirm).
// buy 100 credits for 1 USDT, atomically:
key := ledger.NewIdempotencyKey("buy-credits")
meta := map[string]string{"quote_id": "q-123", "fx_rate": "100"}

_, err := svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
    {TemplateCode: "fx_sell", Params: core.TemplateParams{
        HolderID: userID, CurrencyID: usdtID, IdempotencyKey: key + "-sell",
        Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("1")},
        Metadata: meta,
    }},
    {TemplateCode: "fx_buy", Params: core.TemplateParams{
        HolderID: userID, CurrencyID: creditsID, IdempotencyKey: key + "-buy",
        Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("100")},
        Metadata: meta,
    }},
})
```

> **Direct purchase (no USDT balance first):** if the user pays externally and
> you never hold their USDT as a spendable balance, skip the `fx_sell` leg and
> issue credits with a single journal `DR user.main_wallet(credits) / CR
> settlement(credits)` for 100. The external USDT receipt is recorded separately
> against `custodial`.

---

## Recipe 2 — Discounts (three shapes, pick per business need)

### (a) Price discount — "20% off: 0.8 USDT buys 100 credits"

Nothing structural changes. The discount **is** the rate: charge less USDT for
the same credits. Record the promo in metadata for reporting.

```
fx_sell  (USDT,    amount = 0.8)     CR user.main_wallet(USDT) 0.8  / DR settlement(USDT) 0.8
fx_buy   (credits, amount = 100)     DR user.main_wallet(credits) 100 / CR settlement(credits) 100
metadata: {promo_code: "SAVE20", list_price_usdt: "1.0", charged_usdt: "0.8"}
```

The platform simply realizes less USDT per issued credit. No extra accounts.

### (b) Bonus credits — "top up 100, get 20 free" (platform-funded)

The user pays for 100 credits; the platform *gives* 20 more out of its own
equity. This needs a credits journal with **two amount keys** and `equity` as
the funding counterparty. Register the template once (this is also the general
pattern for "extend the ledger with a new business template"):

```
credits_topup  (currency = credits)
    DR user.main_wallet(credits) 100   ← purchased  (CR settlement — the paid leg)
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
    Code: "credits_topup", Name: "Credits Top-up with Bonus", JournalTypeID: jt.ID,
    Lines: []core.TemplateLineInput{
        {ClassificationID: mw.ID, EntryType: core.EntryTypeDebit,  HolderRole: core.HolderRoleUser,   AmountKey: "purchased", SortOrder: 1},
        {ClassificationID: st.ID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "purchased", SortOrder: 2},
        {ClassificationID: mw.ID, EntryType: core.EntryTypeDebit,  HolderRole: core.HolderRoleUser,   AmountKey: "bonus",     SortOrder: 3},
        {ClassificationID: eq.ID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "bonus",     SortOrder: 4},
    },
})

// per top-up (the paid USDT leg is a separate fx_sell as in Recipe 1):
_, err := svc.JournalWriter().ExecuteTemplate(ctx, "credits_topup", core.TemplateParams{
    HolderID: userID, CurrencyID: creditsID, IdempotencyKey: ledger.NewIdempotencyKey("topup"),
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
NormalSide: credit, IsSystem: true)` and swap `eq.ID` for `promo.ID` above.

**Choosing:** (a) when the discount is just a lower price; (b) when you literally
hand out extra units and want it in equity; (c) when finance wants promo spend
on its own line.

---

## Recipe 3 — Adding currencies for new business scenarios

Every new value type is one `CreateCurrency` call; it reuses all the
classifications and templates above.

```go
points,  _ := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "POINTS", Name: "Loyalty Points"})
coupon,  _ := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "COUPON", Name: "Coupon Balance"})
eur,     _ := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "EUR",    Name: "Euro"})
```

- **Loyalty points**: issue with `DR main_wallet(POINTS) / CR equity(POINTS)`
  (points are platform-funded); redeem with the reverse.
- **Coupons**: same issuance pattern; "spend a coupon toward credits" is an FX
  two-leg `COUPON → credits`.
- **Multi-fiat**: `EUR`, `USD`, … each balance independently; cross-fiat is an
  FX two-leg with `spread` capturing your markup.

Each currency balances on its own — a bug in POINTS accounting can never
corrupt USDT. This isolation is why "just add a currency" is safe.

---

## Recipe 4 — Spending credits: reserve → settle

**Scenario:** a job may cost up to N credits; you hold a budget, run the job,
then capture the actual cost and release the remainder. This is the safe pattern
for metered consumption (an AI generation run, an API call quota, etc.).

`available = balance − SUM(active reservations)`. `Reserve` takes a per-(holder,
currency) advisory lock and checks availability (TOCTOU-safe). `Settle` captures
the actual amount and **auto-releases the remainder** atomically.

```go
// hold up to 50 credits
rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
    AccountHolder: userID, CurrencyID: creditsID,
    Amount:        decimal.RequireFromString("50"),
    IdempotencyKey: ledger.NewIdempotencyKey("run-budget"),
    ExpiresIn:      time.Hour,
})

// run finishes; actual cost was 32 credits → 18 released automatically
err = svc.Reserver().Settle(ctx, rsv.ID, decimal.RequireFromString("32"))
```

- Reserve does **not** move the balance — it's a soft lock reducing *available*.
  Post the actual debit journal (credits leaving `main_wallet` to a `fee_revenue`
  or consumption account) as part of your settle flow if you need the spend on
  the books, composed in the same `RunInTx` as the `Settle`.
- To abandon a hold explicitly (job never ran), call `Release(rsv.ID)`.

---

## Recipe 5 — Refunds and cashing credits back to USDT

### Cashing out (credits → USDT), the reverse FX two-leg

```
fx_sell  (credits, amount = 100)   CR user.main_wallet(credits) 100 / DR settlement(credits) 100
fx_buy   (USDT,    amount = 1)     DR user.main_wallet(USDT)     1  / CR settlement(USDT)     1
```

Same atomic two-leg as Recipe 1, currencies swapped. The user's credits balance
drops by 100, USDT rises by 1. `settlement(credits)` debit reduces outstanding
credit liability. (Real USDT payout to an external wallet is a separate
withdrawal against `custodial`.)

### Refunding a specific charge — use a reversal, never a hand-written "undo"

If you need to void a prior journal (bad charge, disputed purchase), post a
**reversal**. It creates a mirror-image journal linked to the original via
`reversal_of`; the original row is never touched (append-only invariant).

```go
rev, err := svc.JournalWriter().ReverseJournal(ctx, originalJournalID, "customer refund #4821")
```

This is the *only* correct correction mechanism — do not `UPDATE`/`DELETE`
journal rows, and do not synthesize an inverse by hand (you'll drift from the
original's amounts/rounding).

---

## Recipe 6 — Expiry and insufficient funds (the safe edges)

### Insufficient balance is an error, not a silent zero

`Reserve` (and any debit that would overdraw) returns
`core.ErrInsufficientBalance`. Handle it explicitly — surface it to the caller;
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
w := svc.Worker(service.WorkerConfig{}) // sensible defaults incl. ExpirationInterval
go w.Run(ctx)                            // ledgerd does this for you in service mode
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

## Recipe 7 — Retroactive posting and period close

### Backdating a journal to its real business date

`JournalInput.EffectiveAt` (and `TemplateParams.EffectiveAt`) lets you attribute
a journal to a date other than "now" — a delayed on-chain confirmation, an
invoice for last month's usage. Leave it zero for the common case (attribute
to now); set it explicitly for retroactive posting:

```go
_, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{
    JournalTypeID:  depositJT,
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
_, err := svc.JournalWriter().ReverseJournal(ctx, originalJournalID, "March closed, correcting in April")
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
report, err := svc.TrialBalanceReader().TrialBalance(ctx, currencyID, monthEnd)
if !report.Balanced {
    return fmt.Errorf("trial balance off by %s, do not close", report.TotalDebit.Sub(report.TotalCredit))
}
```

---

## Running the example

```bash
export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
go run ./examples/credits-topup
```

It walks Recipes 1, 2b, 4, and 5 end-to-end against a real PostgreSQL, printing
balances at each step. See [`examples/credits-topup/main.go`](../examples/credits-topup).
