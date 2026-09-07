# Configurable fixed conversions

## Input and intended result

The host can model credits, token usage and gifts as currencies or measurable
units and configure a fixed, directed conversion rate. Keep the default example
at 1 USDC to 1,000 CREDITS, while removing that rate and metered usage prices from
the calculation logic. Reuse Currency, ConvertAt, FX templates and the existing
reservation/settlement composition; no separate AI billing or gift subsystem.

Input: current money helpers, currency metadata, FX and credits example →
Output: a configurable conversion contract and exercised host integration →
Handoff: importable Go API/configuration examples plus explicit semantics →
Gate/Review: pure conversion tests, real PostgreSQL purchase/consumption tests,
external consumer/API checks and independent financial review →
Memory: only record a newly observed reusable failure mode.

## Contract and execution plan

1. Define fixed rates by source and target unit, with explicit direction: target
   quantity = source quantity × rate, rounded at the target currency exponent.
   Currency precision remains currency metadata; rates are relations between
   units. Different input/output/cache token prices use separate configured
   source units, and gifts can use exponent zero when indivisible.
2. Reuse decimal arithmetic and magnitude validation. Reject invalid rates,
   mismatched pairs, invalid precision/rounding and negative quantities before
   bookkeeping. Preserve the existing general-purpose ConvertAt contract.
3. Supply configuration through Go objects / a host configuration file. The
   optional preference question received no answer during implementation, so
   this follows the stated default. Configuration storage/editor selection stays
   in the host; the shared value does not require a new table or API module.
4. Exercise configuration in the credits example's purchase and metered charges.
   Preserve Reserve → Settle/SettlePartial + journal atomicity and the sorted
   locks. Save rate, version, original quantity and units as immutable operation
   metadata; retries use the original snapshot, never a newly selected price.
5. Cover at least USDC→credits, input/output-token→credits and gift-unit→credits
   valuation. If an operation moves two stored wallet balances, use paired FX
   journals; valuing measured usage only determines the existing credits debit.
6. Review each lane against tests and actual examples, update cookbook/gaps so
   fixed-rate configuration is supported rather than labeled missing pricing,
   and keep provider event collection and business authorization explicit.

## Acceptance

- [x] Host configuration changes the result without editing calculation code.
- [x] The same conversion primitive handles money, token quantities and gifts.
- [x] Direction, quantity precision, target rounding and invalid inputs are tested.
- [x] Purchase and consumption metadata preserve the configured quote.
- [x] Unchanged retries do not move money again; changed quote/payload conflicts.
- [x] Failed conversions cannot leave holds or journals; successful money flows
      preserve per-currency accounting and zero unused holds after finalization.
- [x] Existing deposit-only and credits regression tests remain green.
- [x] Public consumption and independent review pass; scope and configuration
      instructions are documented.

## Implementation and individual acceptance

`core.FixedRate` is a configuration/validation value only. Its `Convert` method
uses the existing `ConvertAt` arithmetic and the supplied Currency exponents.
No service accessor, interface mutation, database migration or UI dependency was
added. `rates.json` and `CREDITS_RATES_FILE` are the example host's configuration
boundary, not an implicit configuration-storage service inside the ledger.

- Core lane (#21): TDD red/green, seven FixedRate test groups pass with race
  detection (1.349s), including source precision, all rounding modes, extreme
  magnitudes, overflow after rounding and zero quantity at fine rate precision.
- Host lane (#22): all 17 credits test groups pass with race detection (11.297s).
  Defaults retain 912.875 credits and seven journals. Changing USDC→credits to
  2000 and INPUT_TOKEN→credits to 0.003 yields 1902.875, with unchanged replay.
  Gift exchange creates two ROSE from 20 CREDITS; both FX legs contain the same
  complete quote. Different rate/version/quantity/rounding can produce identical
  rounded debits yet still fail conflicting replay through journal metadata.
- Financial review (#23): independently approved actual code, PostgreSQL
  assertions and output. It caught positive stream budgets with zero-rounded
  deltas; the example now rejects zero purchase/budget/stream outputs before
  the deposit fixture, with zero journal/reservation/receipt assertions.
- External consumer: fresh host `/tmp/fixed-rate-consumer-yusqjf7c`, workspace off
  and local replace, tidy/build/run passes four valuation scenarios and error
  assertions. Existing transaction APIs compile; 281 production dependencies
  exclude Docker/testcontainers. Updated `make test-consumer` repeats the new
  import/configuration/run checks in CI.
- Build/vet and affected-package golangci-lint pass. Full generic-gate Go tests
  exposed a test-classifier bug: `source_code` matched the prefix for `source`.
  JSON tags are now parsed for the exact field name, with positive/negative
  classification tests; actual free-form field limits remain enforced. Root and
  full core race rerun pass (38.937s / 10.695s), including the additive API
  snapshot. All other root-module packages passed the preceding full run.

The generic shell scanner still reports its previously reviewed ignored terminal
HTTP/FNV writes and environment-reader boundaries, plus warnings about deliberate
model names and a float64-rejection message. These are not relabeled green.
Current evidence: `/tmp/ledger-fixed-rates-*`; independent reviews:
`.team/reviews/fixed-conversion-{consumer,money}.md`. Push-time CI is recorded in
`~/.claude/session-summaries/2026-09-07-19-00.md`.

Quote retention for zero-cost Release still belongs to the host because Release
has no journal metadata. This example rounds each usage line/delta separately;
a cumulative-pricing host must persist cumulative rounded totals and their deltas.
The [gaps document](../gaps/deposit-credits.md) now distinguishes supported fixed
conversion from configuration storage, provider-event and business-policy choices.

Memory: updated the existing project credits note and transaction-composition
learning with quote identity and preflight rounding evidence; no new global rule.
`hive doctor --strict` passes.
