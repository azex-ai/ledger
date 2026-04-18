# Crypto Deposit Example

Demonstrates the full ledger library workflow for an EVM CREATE2 crypto deposit.

## What it does

1. Connects to PostgreSQL and runs schema migrations
2. Creates currency (USDT), classifications (MainWallet, Custodial, Suspense, Pending, Fees), and a journal type
3. Creates an entry template (`deposit_confirm`) defining the double-entry recipe
4. Simulates an EVM CREATE2 deposit lifecycle:
   - `InitDeposit` (pending) -- user's CREATE2 address detected
   - `ConfirmingDeposit` -- chain transaction found, waiting for confirmations
   - `ConfirmDeposit` -- 12 confirmations reached, record actual amount
   - `ExecuteTemplate` -- post the balanced journal (DR MainWallet / CR Custodial)
5. Queries the user's balance to verify MainWallet was credited
6. Demonstrates Reserve/Settle flow (simulates a 95.50 USDT spend against 100 USDT reserve)
7. Posts a spend journal and prints final balances for both user and system accounts
8. Runs a reconciliation check to verify `SUM(debits) == SUM(credits)`

## Prerequisites

- Go 1.25+
- PostgreSQL 15+ running and accessible

## Run

```bash
# Start a local PostgreSQL (if not already running)
docker run -d --name ledger-pg \
  -e POSTGRES_DB=ledger \
  -e POSTGRES_USER=ledger \
  -e POSTGRES_PASSWORD=ledger \
  -p 5432:5432 \
  postgres:17

# Run the example
cd examples/crypto-deposit
DATABASE_URL="postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable" go run main.go
```

## Expected output

```
Connected to PostgreSQL
Migrations applied
Currency created: USDT (id=1)
Classification created: main_wallet (id=1, side=debit)
Classification created: custodial (id=2, side=credit)
Classification created: suspense (id=3, side=debit)
Classification created: pending (id=4, side=credit)
Classification created: fees (id=5, side=credit)
Journal type created: deposit (id=1)
Template created: deposit_confirm (id=1, lines=2)

--- Deposit Flow ---
Deposit created: id=1 status=pending expected=500
Deposit confirming: channel_ref=0xabc123...
Deposit confirmed
Journal posted: id=1 total_debit=500 total_credit=500

--- Balance Query ---
  holder=1001 currency=1 classification=1 balance=500
MainWallet balance: 500 USDT

--- Reserve/Settle Flow ---
Reserved: id=1 amount=100 status=active
Settled: amount=95.50
Spend journal posted

--- Final Balances ---
MainWallet: 404.50 USDT (expected: 404.50)
  main_wallet (id=1): 404.50
System counterpart balances:
  custodial (id=2): 500
  fees (id=5): 95.50

--- Reconciliation ---
Accounting equation balanced: true (gap: 0)

Done.
```
