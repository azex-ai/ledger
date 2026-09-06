# USDC deposits and AI credits

This example imports the Go ledger and composes its existing deposit, FX,
reservation and journal APIs. **1 USDC buys 1,000 CREDITS.** It installs only
`DepositBundle`, `FXBundle`, a `credits_spend` template and zero-balance floors.
It provides no withdrawal or credits cash-out operation.

Run against a dedicated example database with the runtime and migration roles
described in the root README:

```sh
export MIGRATE_DATABASE_URL='postgres://migration_user:password@localhost/credits_example?sslmode=disable'
export DATABASE_URL='postgres://ledger_app:password@localhost/credits_example?sslmode=disable'
go run ./examples/credits-topup
go test ./examples/credits-topup -race -count=1
```

Tests create isolated PostgreSQL databases via Docker and exercise the public
facade as `ledger_app`. The executable uses stable fixture event IDs, so running
it again after completion does not create another deposit, purchase or charge. It asserts the
final balances rather than just printing an expected result. Use a separate
database from the previous USDT/bonus/cash-out version of this example.

If the process stops after reserving and the hold expires before its result is
captured, replay rejects that new settlement. Completed events remain replayable;
recovering interrupted expired jobs requires the host's reconciliation policy,
not a fresh reservation or an unguarded debit hidden inside a retry.

| Event | Credits charged | Credits balance | Remaining hold |
|---|---:|---:|---:|
| Confirmed 1 USDC deposit and purchase | 0 | 1000 | 0 |
| Fixed-price image, budget 25 | 25 | 975 | 0 |
| Token-metered completion, budget 50 | 32.125 | 942.875 | 0 |
| Failure before billable work / free result, budget 40 | 0 | 942.875 | 0 |
| Streaming event 1, budget 100 | 10 | 932.875 | 90 |
| Streaming event 2 | 20 | 912.875 | 70 |
| Finalize stream | 0 | 912.875 | 0 |

USDC stays at zero in the user's wallet after purchase. The system still holds
the deposited USDC. Consuming credits does not send tokens or record provider
payments. USDC and CREDITS are different accounting units, never summed together.

The executable seeds a confirmed deposit as a local fixture. In a product, use
the existing [crypto-deposit example](../crypto-deposit) to wire a real chain
reader/scanner and confirmation policy. Only confirmed, accepted deposits may
fund purchases; pending/failed/review-held deposits must not issue usable credits.

## Host composition

`purchaseCredits` reserves USDC and atomically settles the reservation plus both
FX journals. Rate derivation is host policy: per-currency balancing cannot detect
a wrong exchange rate. Persist the purchase ID, quote/pricing version and quoted
amount before processing it. A callback retry reuses every operation key.

`captureCredits` atomically pairs Settle/SettlePartial with a `credits_spend`
journal. Settle alone does not debit the wallet. The reservation passed to this
helper is the trusted result of Reserve; a production handler resolves ownership
from its authenticated job record rather than accepting holder/currency fields
from a browser. All competing consumption must use reservations: a raw journal
can bypass a hold even when a zero balance floor is configured.

| Business shape | Existing mechanism | Host decision |
|---|---|---|
| Fixed-price image/tool call | Reserve exact price; capture on billable completion | What counts as completion |
| Token/time metering | Reserve budget; capture actual amount; unused budget releases | Input/output/cached token rates, precision, rounding |
| Stream or multi-step agent | SettlePartial + journal per stable usage-event ID; Finalize at end | Persist deltas, ordering and deduplication of provider events |
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

Both currencies use exponent 6 here, permitting fractional credits. Provider
pricing and rounding happen server-side using decimal arithmetic. Do not round
each streaming increment independently without defining how it reconciles to
the final aggregate. The library validates precision; it does not select a
rounding rule. No fiat revenue, subscription, promo-lot or provider-billing module
is introduced; unresolved policies are tracked in
[deposit-credits gaps](../../docs/gaps/deposit-credits.md).

When using `WithAttestor`, authorize journals before opening a transaction and
post them with `PostAuthorized` inside it. Direct `ExecuteTemplate` in `RunInTx`
is unsigned; see `examples/tamper-evident` and `core.ReserveInput` for the separate
verified-balance and signed-discharge contracts.
