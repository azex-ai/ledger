# Deposit / AI credits decisions left to the host

These are boundaries found while polishing the existing Go + shadcn integration,
not requests to add new ledger modules. Current example policy is deposit-only,
1 USDC → 1,000 credits, with fractional credits and no overdraft.

| Area | Existing mechanism | Missing decision or integration |
|---|---|---|
| Live crypto deposits | EVM adapter, confirmation/review lifecycle, holder deposit-address API | Host network/token allowlist, RPC/scanner, factory/init-code hash, sweep/review policies and session-to-holder mapping; no production values can be inferred safely from fixture data |
| Automatic credit purchase | Atomic FX pair + Reserve/Settle + `RunInTx` | Host durable deposit-to-purchase event/job with deterministic keys; do not mint credits from a browser's claimed transfer |
| Pricing | Decimal amounts, currency exponent, journal metadata | Persisted price version, input/output/cache/tool rates, tax/discount treatment and rounding policy |
| Usage events | SettlePartial and idempotent journals | Normalize cumulative counters into durable deltas; deduplicate provider retries and reconcile final usage; a journal balance check does not verify provider usage |
| Budget overruns / late usage | Hard reservation ceiling and expiry checks | Stop/extend work before exceeding budget, late-billing policy, retry queue; a failed charge is not evidence that provider work was free |
| Promotions / expiring credits | Custom currencies/classifications/templates | Paid-vs-bonus lots, spend order, expiry and refund restrictions; one fungible balance does not encode provenance |
| Refunds | Full journal reversal | Partial refund limits, dispute authorization, consumed-credit treatment and coordinated purchase reversal; refunding a charge is distinct from a withdrawal |
| Revenue / provider costs | Append-only accounting and host-defined templates | Fiat revenue-recognition policy, supplier invoices and margin reconciliation; credits consumed are not automatically USDC revenue |
| Withdrawal | Generic library capabilities predate this integration | Outside current product scope; no payout workflow or cash-out example is added |

The generic admin API is privileged bookkeeping infrastructure, not a customer
payments API. Removing a navigation item or omitting a preset is not an HTTP
authorization boundary. A deposit-only customer integration should expose the
holder read surface and authenticated host-owned deposit/purchase routes, keeping
ledger write credentials server-side.
