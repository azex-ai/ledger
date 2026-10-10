# USD valuation read model

This runnable Go example values fiat, crypto, gifts and credits without changing
their original quantities. It is a **host-owned read model**, not a new ledger API
or a promise of redemption, withdrawal rights, market liquidity or solvency.
All prices and timestamps are local fixtures; it makes no network or database
calls and needs no environment variables.

From the repository root:

```bash
go run ./examples/valuation
go test -race ./examples/valuation/... -count=1
go vet ./examples/valuation/...
```

The example prints USD, BTC, ROSE and CREDITS quantities. Three have usable prices;
ROSE deliberately has none. The result has `complete: false`, `valued_count: 3`
and `subtotal_usd: "629.63"`. It omits `total_usd` and the ROSE row's `value_usd`.
The missing price never becomes a zero-priced asset. Decimal amounts are JSON
strings, and timestamps are UTC.

## Host integration

`main.go` supplies a currency catalog, holdings and price observations to the pure
`valueUSD` function in `valuation.go`. Copy/adapt this small example into the host;
the command package is not a published valuation library. A real host reads its
ledger balances and obtains prices outside any ledger write transaction, then
passes the values in. No rates table or pricing configuration is added to core.

- Join by **Currency UID**, not the display code or token symbol. The adapter owns
  chain/contract-to-currency mapping. Two UIDs with the same code remain distinct.
- Supply one aggregate holding per UID. The host must choose which balance roles
  to value and avoid counting available, total and locked balances twice. Duplicate
  UIDs fail rather than silently double-counting.
- Each price explicitly supplies USD per original currency unit, a source and its
  observation time. `asOf` and `maxAge` are function inputs, so the host owns the
  freshness policy and tests need no global clock. This snapshot example is not a
  historical-price lookup service.
- A price is `valid` when its timestamp is in `[asOf - maxAge, asOf]`, inclusive.
  Earlier observations are `stale`; later ones are `future`. Both retain their
  source, timestamp and unit price for inspection but contribute no USD value.
  An absent price is `missing`. Even a zero holding with a missing price marks
  coverage incomplete; an empty portfolio is a complete zero total.
- `subtotal_usd` sums only valid rows. `total_usd` is present only when every row
  has a usable price. Consumers must check `complete` before presenting a portfolio
  total; a zero subtotal with no coverage is not a zero-value portfolio.
- An explicitly supplied zero price is valid, distinct from missing. Negative
  prices, blank sources and absent timestamps are malformed and return an error.
  Negative holdings are outside this long-only example; debt/net-exposure models
  need their own signed-position semantics. Unknown currencies, mismatched UIDs,
  invalid exponents and quantities finer than their currency exponent also fail.

Quantity and unit price both pass `core.ValidateAmountMagnitude` **before**
formatting, rescaling or multiplication: at most 12 integer and 18 fractional
digits. Currency exponents are separately bounded to 0–18. `core.ConvertAt`
preserves the product at 36 working decimal places, so `0.00000001 ×
0.000000000000000001` remains exactly `0.00000000000000000000000001`.
No per-row cent rounding occurs. Products and the subtotal must remain within
core's 12-integer-digit working bound; overflow is an error, not a truncated total.
Display rounding belongs downstream and never feeds back into quantities.

The example keeps the original numeric quantity, including for missing prices;
JSON may normalize insignificant trailing zeros. It performs no swap, reserve,
posting or authorization. A configured USD value for a gift or credit does not
make that asset redeemable for USD.
