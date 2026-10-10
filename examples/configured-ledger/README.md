# Configured accounts: deposits, fees, points and gifts

This runnable example uses existing `CurrencyInput`, `TemplateBundle`,
`TemplateParams` and `FixedRate` values. `ExecuteTemplate` uses the existing
`EntryTemplate.Render` path; `Exchange` composes the existing FX templates in an
atomic transaction. There is no additional configuration language, rate table,
market feed or external trade.

Configuration lives in [main.go](main.go): `currencies`, `pointsBundle`,
`purchaseRate` and `giftRate`. Deposit, fee and FX definitions come directly from
`presets.DepositBundle()`, `FeeBundle()` and `FXBundle()`. POINTS adds one
credit-normal system classification and a two-line issuance template while
reusing the debit-normal user `main_wallet` classification. Currency UIDs,
resolved from these exact codes during setup, identify all journal dimensions.

## Run

Use a **fresh, dedicated example database**, separate from other examples and
production. Provision the migration credential and `ledger_app` runtime login
as described in the root [library prerequisites](../../README.md#quick-start----as-a-library)
and [database roles runbook](../../docs/RUNBOOK.md#database-roles-ledger_owner--ledger_app--ledger_ro).
The database must already exist. First-time schema bootstrap requires a migration
credential that can create roles; the runtime login must have its password set
through your local database provisioning. The example runs migrations with the
migration connection and checks `AssertRuntimeRole` on the runtime connection.

```sh
export MIGRATE_DATABASE_URL='postgres://migration_user:password@localhost/configured_example?sslmode=disable'
export DATABASE_URL='postgres://ledger_app:password@localhost/configured_example?sslmode=disable'
go run ./examples/configured-ledger
```

Tests use the existing PostgreSQL fixture, creating and deleting an isolated
database per test. Supply a separate **test administrator** connection able to
create databases and run migrations; the tested application calls still use
`ledger_app`. With `DATABASE_URL` unset, the fixture uses Docker instead.

```sh
DATABASE_URL='postgres://test_admin:password@localhost/postgres?sslmode=disable' \
  go test -race ./examples/configured-ledger/... -count=1
```

## Concrete accounting effects

Amounts below are native quantities. Each row describes a confirmed local
fixture event. `system` means the counterpart for holder `2010`, resolved by
`core.SystemAccountHolder`. A `+` or `−` is a change in the classification's
normal-side balance, not a debit/credit sign.

| Event | User effect | System effect |
|---|---|---|
| Confirm deposit of 100 USDC | `main_wallet +100 USDC` | `custodial +100 USDC` |
| Charge fee of 25 USDC | `main_wallet −25 USDC`; `fee_expense +25 USDC` | `custodial −25 USDC`; `fees +25 USDC` |
| Convert 1 USDC to 1,000 CREDITS | `main_wallet −1 USDC`; `main_wallet +1000 CREDITS` | `settlement −1 USDC`; `settlement +1000 CREDITS` |
| Issue 100 POINTS | `main_wallet +100 POINTS` | `points_issued +100 POINTS` |
| Convert 20 CREDITS to 2 ROSE | `main_wallet −20 CREDITS`; `main_wallet +2 ROSE` | `settlement −20 CREDITS`; `settlement +2 ROSE` |

The fee uses the preset's four lines: DR user `fee_expense`, DR system
`custodial`, CR user `main_wallet`, CR system `fees`. `main_wallet` and
`fee_expense` are debit-normal; `custodial`, `fees`, `settlement` and
`points_issued` are credit-normal. The fee expense has the `memo` balance role,
so its increase does not replenish the spendable wallet. This books fee revenue;
it is different from returning consumed credits to their issuance counterpart.

Final user wallet quantities are **74 USDC, 980 CREDITS, 100 POINTS and 2 ROSE**.
The user also has a **25 USDC memo expense**. Final system balances are
`custodial = 75 USDC`, `fees = 25 USDC`, `settlement = −1 USDC / 980 CREDITS /
2 ROSE`, and `points_issued = 100 POINTS`. These classifications are checked
separately. They must not be added across currencies or treated as physical
custody evidence.

`main_wallet` has the `available` role. Both exchanges finish their holds inside
their own transactions, so no holds remain and `GetBalanceBreakdown.available`
equals each final wallet quantity. In general that API subtracts active holds
from available-classification book balances; the two values need not be equal.

The executable verifies every emitted journal's **DR = CR for each currency**,
then checks these literal business expectations independently of the configured
rates and rendered entries. The seven journals include two journals per FX
operation: each currency balances against its own system settlement account.

## What acceptance rejects

[main_test.go](main_test.go) exercises the real database and runtime role:

- Reversing all four fee directions still balances, but incorrectly increases
  the wallet to 125 USDC and makes the expense and revenue negative. The
  economic check rejects that candidate; its test transaction rolls back.
- A valid configured rate of `0.5 ROSE/CREDITS` produces 10 ROSE for 20 CREDITS.
  Both journals still balance and `Exchange` correctly follows that supplied
  quote. The independent business requirement is **2 ROSE**, so acceptance
  rejects it and the test transaction rolls back both FX journals and the hold.
- Both rejection tests verify that persistent journal/reservation counts and
  user/system balances return to their initial values. Passing balance checks
  alone never certifies either candidate as economically correct.

These checks are configuration acceptance tests. The ordinary executable checks
its final balances after the events commit; it does not provide a generic
production approval mechanism or undo an already committed wrong configuration.
Validate changed configurations against independent business expectations before
deploying them. In a caller-owned transaction, propagate errors to `RunInTx`;
there is no implicit savepoint around `Exchange`.

## Event identity and boundaries

The fixture event keys use `configured-demo-v1` with `:deposit`, `:fee`,
`:purchase`, `:reward` and `:gift`. Exchanges derive their own reserve, settle,
pay and issue keys. Running the same example again with the original
configuration returns the same seven journal UIDs and changes no balances or
holds. A changed rate on a completed exchange key is rejected as `ErrConflict`.
Persist business event IDs and their original submitted payloads in a real host;
never change an event ID to make a retry bypass a conflict. New configuration
versions apply to new events. The whole demonstration is a sequence of events,
not one atomic transaction; a restart replays the committed prefix.

The deposit and reward are trusted local fixtures. This is not a deposit
confirmation service, authorization layer or general-purpose spending endpoint.
The fixed fee demonstrates a configured journal; raw `ExecuteTemplate` does not
reserve funds. A production host must supply authorization and an atomic
reservation/fee composition for concurrent spending. Exchange uses its own
reservation workflow and zero-balance account policies installed by this example.

The directed rates are host catalog values (`catalog-v1`, decimal arithmetic,
round down to the target exponent), not USD market prices. USDC/CREDITS use
six decimal places; POINTS/ROSE are whole units. A count, configured exchange
path or `available` classification does not grant withdrawal, cash redemption,
point redemption or gift-transfer rights. No such operations are installed here.
USD valuation is a separate read model; changing a display price must not
rewrite these native quantities. External venue execution and custody
reconciliation remain host responsibilities, outside this example.
