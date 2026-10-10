# Signed charge and settlement composition

This example imports the existing Go ledger APIs. It credits a local fixture
with 100 SCREDITS, reserves 60, and charges 20 while settling the reservation
in one PostgreSQL transaction. No chain, provider, payment or RPC is contacted.
SCREDITS is an independent currency with exponent 2; the fixture models credits
issuance/redemption, not fiat revenue recognition or provider payment.

## Run

From the repository root, the tests create and remove isolated PostgreSQL
databases. They use PostgreSQL 17 through Docker when `DATABASE_URL` is unset:

```sh
go test -race -timeout 5m ./examples/signed-capture/... -count=1
go vet ./examples/signed-capture/...
```

To use an existing **test-only** PostgreSQL server, provide a credential that
can create test databases and run the ledger migrations, including role setup:

```sh
DATABASE_URL='postgres://test_admin:password@localhost:5432/postgres?sslmode=disable' \
  go test -race -timeout 5m ./examples/signed-capture/... -count=1
```

For the executable, provision a **fresh example database** and separate migration
and runtime credentials as described in the root [README](../../README.md) and
[database role runbook](../../docs/RUNBOOK.md). The runtime must
assume `ledger_app`; `AssertRuntimeRole` checks this before any fixture writes.

```sh
export MIGRATE_DATABASE_URL='postgres://migration_user:password@localhost:5432/signed_capture_example?sslmode=disable'
export DATABASE_URL='postgres://ledger_app:password@localhost:5432/signed_capture_example?sslmode=disable'
go run ./examples/signed-capture
```

The executable generates an in-memory development key using `authdev`, and
requires a new database for each run. It refuses duplicate fixture configuration.
It exercises a replay **within the same run**, then asserts and prints:

```text
signed charge=<journal UID>; replay returned the same journal
verified balance=80; ordinary hold=0
gated reserve 21 refused: 80 balance - 60 unexpired unsigned hold = 20
```

Production hosts supply their signing key from a failure domain independent of
the database credential and retain historical public keys for verification.
The example does not implement key custody, rotation or attestation anchoring.

## Sequence and responsibilities

1. On the top-level service, call `Reserve` with the host's chosen
   `RequireVerifiedBalance` policy and a realistic operation lifetime. This
   example enables the gate. It may consult a remote verifier before its own
   database transaction. A verified balance read alone is not a reservation.
2. Complete external work outside any database transaction. The host durably
   records the submitted usage event's identity, reservation association, amount,
   operation kind and effective time. These fields remain fixed on delivery
   retries. The host authorizes the job and resolves its reservation; it must
   not trust client-supplied holder/currency fields.
3. `capture` derives both operation keys from that event, calls
   `AuthorizeTemplate`, then verifies the authorization **before** `RunInTx`.
   New authorizations use `core.VerifyJournalAuth`. For an existing key,
   `AuthorizeTemplate` reuses its stored status without returning new signature
   material. Only a `signed` result with digest, signature and key ID all empty
   takes this replay path; partially missing material fails normal verification.
   This example verifies the persisted history of both template
   dimensions using `VerifiedBalanceReader`; it never treats missing replay
   signature material as a new authorization. That conservative replay policy
   also refuses a receipt replay if later unsigned or invalidly signed history
   touches a dimension. This is a whole-history check, not a lookup/verification
   API for that single journal.
   Signature validation is not a check of current balance, policy or reservation
   state. The prepared authorization stays local to this attempt.
4. Inside `RunInTx`, `LockForTemplates` acquires the combined keys and balance
   locks first. `PostAuthorized` performs the journal write with normal ledger
   input, precision and policy checks; `Settle` or `SettlePartial` validates and
   discharges the hold. The same amount feeds both operations. Every error is
   returned from the callback so both writes roll back together. No external
   signer, verifier or provider runs in the callback.
5. Publish the journal receipt only after commit. A lost response retries the
   whole composition with the same event, `EffectiveAt`, journal key and
   settlement key. For `ErrTransient`, retry the entire attempt. Partial usage
   increments each need a distinct event ID; replays must keep `Partial`
   unchanged. Cross-operation deduplication belongs to the host's durable event
   record, not to this example helper.

`Settle` alone does not debit a wallet. A partial capture leaves the remainder
held; use the existing `FinalizeSettlement`/`Release` policy when the job ends.
Expired holds reject new settlement; reconcile late usage instead of silently
replacing the hold or charging directly. All competing consumption must go
through reservations: raw journals can bypass holds even with a zero floor.

The template and reservation are trusted host configuration/input. This helper
is not a general handler for arbitrary templates or arbitrary reservation UIDs.
`PostAuthorized` stores the prepared authorization; it does not invoke a verifier
in the transaction. The pre-transaction verification in this example is explicit.
Journal signatures do not authenticate arbitrary metadata, external delivery or
the host's job-to-reservation association.

This workflow retains submitted results and committed receipts for delivery and
deduplication. It does **not** save or restore financial form drafts or an old
authorization for future submission. New financial operations require current
inputs/authorization; retrying a submitted event must not rerun external work.

## Signature and spendability boundary

| Item | Result in this example |
| --- | --- |
| Charge journal | Signed outside the transaction and written with `PostAuthorized` |
| `Settle` / `SettlePartial` discharge | **Unsigned** because it is written inside the transaction |
| `VerifiedBalance` | Entries-only signed balance: 80 after a charge of 20 |
| Ordinary hold after full / partial capture | 0 / 40 for an original reservation of 60 |
| Hold counted by a new verified reserve | The full original 60 until that reservation expires |
| Maximum new verified reserve, before any other writes | 80 − 60 = 20 |

The verified reserve gate credits only verifiable discharge claims. It cannot
trust an unsigned transaction-mode settlement, so this composition retains the
conservative hold until expiry even though ordinary settlement succeeded.
`VerifiedBalance` remains defined: the unsigned **discharge** is not an unsigned
journal. Direct `ExecuteTemplate`/`PostJournal` inside `RunInTx` would create an
unsigned journal and make its contributing balance dimension undefined instead.
The higher-level ordinary `Capture` helper is therefore not used here.

Top-level settlement can sign a discharge outside its own transaction, but
moving it out of this callback would lose atomicity with the charge. This example
does not claim a fully signed funds lifecycle or atomicity across PostgreSQL and
an external provider. See [I-49 and I-65](../../docs/INVARIANTS.md) for the gate's
conservative hold and signed-discharge contracts.

Tests run writes as `ledger_app` and assert full/partial charge balances,
reservation state, same-key replay, signer errors and invalid signatures,
rollback when settlement rejects an already-posted charge, and the exact
verified-reserve boundary. They also inspect real PostgreSQL transaction state
at every signer/verifier call to enforce the placement rule.
