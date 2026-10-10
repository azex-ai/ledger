# Deposit / AI credits decisions left to the host

These are boundaries found while polishing the existing Go + shadcn integration,
not requests to add new ledger modules. Current example policy is deposit-only,
1 USDC → 1,000 credits, with fractional credits and no overdraft.

| Area | Existing mechanism | Missing decision or integration |
|---|---|---|
| Live crypto deposits | EVM adapter, confirmation/review lifecycle, holder deposit-address API | Host network/token allowlist, RPC/scanner, factory/init-code hash, sweep/review policies and session-to-holder mapping; no production values can be inferred safely from fixture data |
| Automatic credit purchase | `svc.Exchange`: Reserve/Settle + both FX legs in one transaction, deposit-compatible lock order, quote and `funding_uid` on both journals; joins the host's `RunInTx` | Host durable deposit-to-purchase job with a deterministic key (e.g. `purchase:<deposit booking uid>`), driven from the confirmed-deposit event (`Worker.Subscribe`); do not mint credits from a browser's claimed transfer |
| Pricing | `core.RateQuoter` port (one method) + `core.FixedRate` value; currency/unit exponent and explicit rounding; `ConversionQuote` snapshot on every FX leg and priced usage line; example JSON adapter covers USDC, input/output tokens, images and gifts | The `RateQuoter` adapter itself (JSON, host table with effective dates, oracle) and which version is active — by decision (2026-09-07) the ledger stores no rates and ships no rate UI; tax/discount policy |
| Business reconciliation | Journal metadata carries `funding_uid` on exchanges and `conversion_quotes` on charges; per-currency and accounting-equation checks | "Confirmed but never converted": join confirmed deposit bookings' `journal_uid` against FX journals whose `funding_uid` matches (`Exchange` only accepts a journal uid carrying the holder's source-currency entry). "Completed but never charged": needs the host's job table. Neither is visible to the ledger's internal checks, which only prove what was written balances |
| Usage events | SettlePartial and idempotent journals | Persist each provider event's immutable amount and operation kind before delivery; deduplicate across charge/release/finalize routes, normalize cumulative counters into durable deltas and reconcile final usage. Different ledger operation keys do not authenticate a shared provider event |
| Budget overruns / late usage | Hard reservation ceiling and expiry checks | Stop/extend work before exceeding budget, late-billing policy, retry queue; a failed charge is not evidence that provider work was free |
| Promotions / expiring credits | Custom currencies/classifications/templates | Paid-vs-bonus lots, spend order, expiry and refund restrictions; one fungible balance does not encode provenance |
| Refunds | Full reversal and `ReverseJournalFraction`, including per-entry cumulative caps and concurrent/replay protection | Refund eligibility, approval and business limits, consumed-credit treatment and coordinated purchase reversal; refunding a charge is distinct from a withdrawal |
| Revenue / provider costs | Append-only accounting and host-defined templates | Fiat revenue-recognition policy, supplier invoices and margin reconciliation; credits consumed are not automatically USDC revenue |
| Withdrawal | Generic library capabilities predate this integration | Outside current product scope; no payout workflow or cash-out example is added |

The generic admin API is privileged bookkeeping infrastructure, not a customer
payments API. Removing a navigation item or omitting a preset is not an HTTP
authorization boundary. A deposit-only customer integration should expose the
holder read surface and authenticated host-owned deposit/purchase routes, keeping
ledger write credentials server-side.
