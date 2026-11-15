# Rate quoter port, library exchange primitive, explainable holder rows

## Decision (Aaron, 2026-09-07)

Rate storage ownership is not a ledger question. Rates come from outside the
ledger — an external market rate for real currencies, a business configuration
for credits / gifts / points — and every business unit is already a `Currency`.
The ledger therefore:

- defines **one port** (`core.RateQuoter`, single method) that any host adapter
  implements: JSON file, host database, price oracle. The same shape serves
  external and configured rates; only `FixedRate.Version` differs in meaning;
- **consumes a resolved quote value**, never the port, inside money movement:
  the quote is resolved outside the transaction (financial.md: no external calls
  inside a DB transaction) and passed in;
- **stores no rate configuration**: no rates table, no version table, no admin
  page in `@azex/ledger-react`;
- **keeps the applied quote on the journal** (`conversion_quotes` metadata).
  That is the operation's evidence, not configuration: it drives replay-conflict
  detection and the holder-facing explanation of a charge.

"Complete a task, receive N tokens" is not a rate: it has no source unit. It is
a grant template from the system counterpart with a host reason code.

## Scope

1. `core`: `RateQuoter` port; `ConversionQuote` value with a canonical JSON
   encoding under `core.ConversionQuotesMetadataKey`; `FixedRate.Quote`;
   `RoundingMode` text names; `JournalInput.Validate` rejects a malformed
   `conversion_quotes` value at write time; `FundingUIDMetadataKey`.
2. Root package: `(*Service).Exchange` — the reserve → settle → paired FX
   journals composition the credits example hand-rolled, with the lock order
   `LockForTemplates` fixes, quote snapshot on both legs, and the optional
   funding reference a host reconciles confirmed deposits against. Works on the
   top-level Service (opens `RunInTx`) and on the transaction-bound clone.
3. Example `credits-topup`: its JSON pricing becomes a `core.RateQuoter`
   adapter; purchases go through `svc.Exchange` and carry the deposit's uid.
4. Holder surface: `GET /holder/transactions` rows carry `quotes` — source unit,
   quantity, rate, target unit, amount. Version, rounding and exponents stay
   off the wire. `@azex/ledger-react` renders the line in both skins with a
   `unitLabels` presenter map (same pattern as `kindLabels`).
5. Docs: gaps, cookbook, api, frontend, changelog, API surface snapshot.

Out of scope, by decision: rate persistence, rate versioning UI, provider
usage-event persistence, lot/expiry modeling.

## Acceptance

- [x] A host adapter implementing `RateQuoter` is the only thing a second
      consumer needs; the library has zero rate-storage code.
      `examples/credits-topup/pricing.go` is the JSON adapter (`var _
      core.RateQuoter = pricing{}`); no migration, no table, no UI added.
- [x] `Exchange` replay with the same key and quote is a no-op; a changed quote
      on the same key is `ErrConflict`; a zero-rounded output writes nothing.
      `exchange_test.go`: `TestExchange_ReplayIsNoOpAndChangedQuoteConflicts`,
      `TestExchange_RefusesBeforeWritingAnything`.
- [x] Concurrent `Exchange` and deposit on the same holder do not deadlock —
      `examples/credits-topup`'s `TestPurchaseConcurrentDepositLockOrder` now
      drives `svc.Exchange` through `purchaseCredits`.
- [x] A journal whose `conversion_quotes` does not decode is rejected at post
      time (`core.TestJournalInput_Validate_RejectsMalformedConversionQuotes`);
      `postgres/holder_store.go` errors on a decode failure instead of
      dropping the explanation.
- [x] Holder wire carries `quotes` with only user-explainable facts;
      `server.TestHolderHandlerWireShape` pins the empty array and that
      `version` / `rounding` / `exponent` never appear.
- [x] Both React skins render the quote line from the same `describeQuotes`
      presenter; `test/wallet/components.test.tsx` asserts the sentence on
      both skins and that no unit code leaks once `unitLabels` names it. A
      quote-less row (and a pre-`quotes` server) renders as before.
- [x] `docs/api-surface.txt` regenerated (43 added symbols, nothing removed
      or re-signed); no BREAKING entry needed.

## Verification (2026-09-07 22:20 SGT)

- `go test . ./core/ ./server/ ./examples/credits-topup/ -race -count=1` — ok
  (root 42s incl. 5 Exchange PostgreSQL tests; credits example 13s).
- `go test ./postgres/ -run Holder -race` — ok. `go vet ./...`, `gofmt -l`,
  `golangci-lint run` on core/server/postgres/root/example — 0 issues.
  `sqlc diff` — clean.
- README gates (`TestREADMEDocumentsEveryExportedServiceMethod`,
  `TestREADMEOpenAPICountsMatchSpec`) — ok after adding the `Exchange` row and
  the 61 paths / 103 schemas count.
- `@azex/ledger-react`: `tsc --noEmit` ok; `npm run codegen` regenerated
  `schema.ts` from the OpenAPI change (deterministic on re-run); `npm run
  build` + `vitest run` — 43 files, 245 tests passed (skin-parity gate
  included).
- Independent money-path review: `.team/reviews/rate-quoter-exchange-money.md`
  (adversarial; see the changelog entry for disposition).
