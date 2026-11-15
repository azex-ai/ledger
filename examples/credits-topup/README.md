# USDC deposits and AI credits

This example imports the Go ledger and composes its deposit, FX, reservation
and atomic `Capture` APIs. **By default, 1 USDC buys 1,000 CREDITS.** It installs only
`DepositBundle`, `FXBundle`, a `credits_spend` template and zero-balance floors.
It provides no withdrawal or credits cash-out operation.

Run against a fresh dedicated example database with the runtime and migration roles
described in the root README:

```sh
export MIGRATE_DATABASE_URL='postgres://migration_user:password@localhost/credits_example_v3_capture?sslmode=disable'
export DATABASE_URL='postgres://ledger_app:password@localhost/credits_example_v3_capture?sslmode=disable'
go run ./examples/credits-topup
go test ./examples/credits-topup -race -count=1
```

The embedded [rates.json](rates.json) configures currencies/units, directed rates,
versions and rounding. To use your own file, set `CREDITS_RATES_FILE=/path/to/rates.json`
before running. Amounts and rates are decimal strings; rounding accepts `half_up`,
`half_even`, `down` or `up`. Duplicate units/pairs, missing references and invalid
rates are rejected. Currency precision is set when the wallet currency is created;
an existing currency with different precision is rejected.

| Source → target | Configured target units per source unit |
|---|---:|
| USDC → CREDITS | 1000 |
| INPUT_TOKEN → CREDITS | 0.002 |
| OUTPUT_TOKEN → CREDITS | 0.005 |
| IMAGE → CREDITS | 25 |
| STREAM_TOKEN → CREDITS | 0.01 |
| ROSE → CREDITS | 10 |
| CREDITS → ROSE | 0.1 |

Rates are directed; reverse conversion is configured separately. Token and gift
quantities use exponent zero. Measured units need no database row or wallet:
10,000 input tokens + 2,425 output tokens price to `20 + 12.125 = 32.125` credits.
For an actual gift balance, create the ROSE currency and call the library's
`svc.Exchange`: the tested 20 CREDITS purchase creates two ROSE through paired FX
journals. The same fixed-rate primitive values usage and prices wallet exchanges.

Tests create isolated PostgreSQL databases via Docker or the supplied
`DATABASE_URL` and exercise the public facade as `ledger_app`. The executable uses
stable `credits-demo-v3-capture:*` fixture event IDs, so running
it again after completion does not create another deposit, purchase or charge. It asserts the
final balances rather than just printing an expected result. Use a separate
database from earlier versions of this example. Configuration changes apply to
new operations. Completed fixture IDs
retain their original quotes: rerunning them with a different rate/version returns
`ErrConflict`, even when rounding produces the same amount. For this demo, retain
the original configuration when replaying the fixed fixture IDs.

The earlier `credits-demo-v2:*` helper already used `event:settle` and
`event:charge`; those suffixes are preserved. The incompatibility is the old
journal payload: `Capture` now adds `capture_mode` and `capture_template_code`
alongside `reservation_uid`. Old journals must not be rewritten or given new
keys to evade a replay conflict. This new demo namespace represents new fixture
events **only in a fresh example database**, not a migration of submitted usage.
`setup` and `scenario` refuse any older `credits-demo*` journal or reservation
before demo configuration or accounting writes; current v3 events remain
replayable. Bootstrap schema migrations still run first to provision the schema
and runtime role, so this refusal does not promise that no schema changes occur.
Keep real submitted events on their original processing version or reconcile
them under an explicit host migration policy, preserving their identities. Do
not run old and new demo versions concurrently against one database.

If the process stops after reserving and the hold expires before its result is
captured, replay rejects that new settlement. Completed events remain replayable;
recovering interrupted expired jobs requires the host's reconciliation policy,
not a fresh reservation or an unguarded debit hidden inside a retry.

| Event | Credits charged | Available classification balance | Remaining hold | Spendable credits |
|---|---:|---:|---:|---:|
| Confirmed 1 USDC deposit and purchase | 0 | 1000 | 0 | 1000 |
| Fixed-price image, budget 25 | 25 | 975 | 0 | 975 |
| Token-metered completion, budget 50 | 32.125 | 942.875 | 0 | 942.875 |
| Failure before billable work / free result, budget 50 | 0 | 942.875 | 0 | 942.875 |
| Streaming event 1, budget 100 | 10 | 932.875 | 90 | 842.875 |
| Streaming event 2 | 20 | 912.875 | 70 | 842.875 |
| Finalize stream | 0 | 912.875 | 0 | 912.875 |

The classification balance is the `main_wallet` book balance returned by
`GetBalance`. Spendable credits are `GetBalanceBreakdown.available`: in this
example, the available classification balance minus reservation holds. A
partial capture reduces both the book balance and the hold by the captured
amount; it does not free that amount for another reservation.

USDC stays at zero in the user's wallet after purchase. The system still holds
the deposited USDC. Consuming credits does not send tokens or record provider
payments. USDC and CREDITS are different accounting units, never summed together.

The executable seeds a confirmed deposit as a local fixture. The existing
[crypto-deposit example](../crypto-deposit) demonstrates deposit orchestration
with simulated sightings; a product must supply real chain reader/scanner
dependencies and a confirmation policy. Only confirmed, accepted deposits may
fund purchases; pending/failed/review-held deposits must not issue usable credits.

## Host composition

Rates enter through one port. `pricing` in [pricing.go](pricing.go) is this
host's `core.RateQuoter`: a JSON file of units and directed rates. A second host
backs the same single method with a database table or a price oracle; the
ledger stores no rates and does not distinguish configured from sourced ones.
The rate is resolved **before** any transaction opens and handed to the ledger
as a value.

The purchase itself is the library's `svc.Exchange`: it resolves the two stored
currencies, quotes the amount with `core.FixedRate.Quote`, then atomically
reserves and settles the source and posts both FX journals, taking the union of
every lock up front so a concurrent deposit follows the same order. Both
journals carry the applied `core.ConversionQuote` under `conversion_quotes`,
and the deposit journal's uid under `funding_uid` — the link a host's business
reconciliation joins on to find a deposit that was confirmed and never
converted. `Exchange` refuses a `FundingUID` that is not a journal with an
entry for this holder in the source currency, so the link always points at the
holder's own deposit. A production host that models deposits as bookings passes
the confirmed booking's `JournalUID` there (a journal uid, not the booking uid). Called on
the `*Service` inside a `RunInTx` callback, `Exchange` joins that transaction, so
the host's own "deposit converted" write commits with the purchase.

Each positive usage journal stores the same `conversion_quotes` shape for its
priced lines: source/target unit and exponent, original quantity, rate, version,
rounding and converted amount. Persist this snapshot with the host event before
processing it; a retry reuses the snapshot and every operation key. The holder
statement (`GET /holder/transactions`, `@azex/ledger-react`'s wallet) renders
the quotes as "10,000 input tokens × 0.002 + 2,425 output tokens × 0.005";
version, rounding and exponents stay on the admin journal surface.

For other composed flows, call `tx.LockForTemplates` before Reserve with every
template the transaction will post and the reservation key; retry
`ErrTransient` by replaying the entire transaction with the same keys and
payloads.

Persist each usage event's amount and operation kind before delivery. A retry
must not switch the same provider event from a charged increment to a zero-cost
release: those are different ledger operations with separate receipts. The host's
durable event record owns cross-operation deduplication; these helpers assume it.

For positive usage, `captureUsage` calls `svc.Capture` with the original event
key and `TemplateCode: "credits_spend"`. Capture atomically pairs
Settle/SettlePartial with the charge journal and validates the holder/currency's
available classification net decrease. Settle alone does not debit the wallet.
The helper copies business quote metadata and adds `usage_event_id`; Capture
owns the reserved `reservation_uid`, `capture_mode`, and `capture_template_code`
fields, which the caller must not supply. The reservation passed to this
helper is the trusted result of Reserve; a production handler resolves ownership
from its authenticated job record rather than accepting holder/currency fields
from a browser. All competing consumption must use reservations: a raw journal
can bypass a hold even when a zero balance floor is configured.

Capture opens a transaction on a top-level Service and joins a caller's
`RunInTx` clone without a savepoint. Return its error from the callback so
settlement and journal writes roll back together. If that transaction also
performs other money operations, pre-acquire their full lock union using
`LockForTemplates` before the first write. Replay the same key and full payload;
changing full versus partial mode may return `ErrInvalidTransition` before the
journal's `ErrConflict` check. Both reject the changed operation.

| Business shape | Existing mechanism | Host decision |
|---|---|---|
| Fixed-price image/tool call | Reserve exact price; capture on billable completion | What counts as completion |
| Token/time metering | FixedRate maps quantities to credits; reserve budget and capture actual amount | Configure input/output/cache units, rates, precision and rounding |
| Stream or multi-step agent | `Capture` with `Partial: true` per stable usage-event ID; `FinalizeSettlement` at end | Persist deltas, ordering and deduplication of provider events |
| Cancel after partial work | Release/Finalize remaining hold; prior charge journals stay | Whether completed work remains billable |
| Zero-cost/cache hit/failure before usage | Release only; do not post zero-amount journals | Free vs discounted cache policy |
| Retry or response lost | Replay the same event ID and payload | Do not rerun provider work just because ledger delivery retried |
| Cost exceeds budget | Reject excess capture; preserve reservation | Stop generation or obtain another authorized budget before further work |
| Job outlives hold | Expired hold cannot accept new settlement | Set realistic TTL and reconcile late provider usage; no silent overdraft |
| Full consumption correction | Reverse the original charge journal | Authorization/reason; no onchain payout |

Provider calls happen outside database transactions. Store the usage event/job
result durably; the host can compose its own DB writes with the ledger using
`RunInTx`/`DBTX` as in `examples/tx-compose`. A cancelled request context cannot
release a hold: use `context.WithTimeout(context.WithoutCancel(ctx), ...)` for
bounded cleanup and retain a durable retry when cleanup fails.

Default wallet currencies use exponent 6, permitting fractional credits. Conversion
happens server-side with decimal arithmetic and configured rounding. This demo
rounds each priced usage line/delta separately, then sums the results. Its stream
must have positive rounded increments; zero-rounded purchase amounts, budgets or
stream increments fail before any deposit/reservation/journal. A host using
cumulative pricing must persist the difference between rounded cumulative totals
instead of treating independently rounded deltas as equivalent.

Zero-cost final usage only releases its reservation; it writes no zero journal.
Consequently journal metadata cannot compare quotes for zero-cost release events:
their immutable payload and cross-operation deduplication remain host duties.
No fiat revenue, subscription, promo-lot or provider-billing module is introduced;
unresolved policies are tracked in
[deposit-credits gaps](../../docs/gaps/deposit-credits.md).

This Capture path produces unsigned journals and discharge claims, even with
`WithAttestor`. For verified journals, authorize before opening a transaction
and compose `PostAuthorized` with settlement inside it. An unsigned discharge
retains the verified-balance gate's conservative hold until expiry; journal
authorization alone does not sign the discharge. See `examples/tamper-evident`
and `core.ReserveInput` for the separate authorization and hold contracts.
