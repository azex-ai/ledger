// Example: tamper-evident ledger — signing, attestation, and a withdrawal gate.
//
// Answers, in runnable form, the question the integrity-hardening work exists
// for: an attacker holds a database write credential and inserts a perfectly
// balanced journal that credits themselves. Every other invariant this ledger
// enforces is satisfied by construction. What stops the money from leaving?
//
// The short answer is that the database is untrusted and the application is
// not: the signing key is reachable by this process and not by whoever holds
// DATABASE_URL. This example wires that up and then actually forges a row to
// show the difference it makes.
//
// Demonstrates:
//   - ledger.New(pool, ledger.WithAttestor(...))  — per-journal signing
//   - svc.AttestationService(anchor)              — batch chain + external anchor
//   - svc.VerifiedBalanceReader()                 — balance that ignores unsigned journals
//   - ReserveInput.RequireVerifiedBalance         — the gate money passes through
//   - svc.VerifyLedger(ctx, anchor, cfg)          — the five-step audit
//   - svc.Authorize + tx.JournalWriter().PostAuthorized() inside RunInTx — the
//     ONLY way to keep a journal signed when it must be composed atomically
//     with other writes, contrasted with PostJournal/ExecuteTemplate called
//     directly inside RunInTx, which always produces auth_status=unsigned_tx_mode
//     (see the appendix at the end of run())
//
// Both dev implementations used here are shipped for exactly this purpose and
// nothing more:
//
//   - authdev.NewLocalAttestor keeps an ed25519 key in memory. For a real
//     deployment the only hard requirement is that the key not live in the same
//     failure domain as DATABASE_URL — a leak that hands over the database must
//     not also hand over the key, or signatures prove nothing. Which carrier
//     satisfies that is your deployment's call.
//   - anchordev.NewLocalFileAnchor writes the chain head to a local file, which
//     is emphatically NOT tamper-evident storage — the same machine, the same
//     user. A production anchor has to be somewhere the database credentials
//     cannot reach, immutable once written, and independently readable.
//
// Expects an EMPTY ledger database. It forges a row on purpose and leaves it
// there, so a second run against the same database starts from a ledger that
// already contains one. The signing key is also freshly generated each run,
// which means journals signed by a previous run cannot verify under this
// run's key -- realistic (losing a key does make history unverifiable) but
// confusing to read as output. Point DATABASE_URL at a scratch database.
//
// Run:
//
//	createdb ledger_example
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_example?sslmode=disable"
//	# optional: migrations on their own credential (see docs/RUNBOOK.md "Database roles")
//	export MIGRATE_DATABASE_URL="postgres://ledger_owner:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/tamper-evident
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

const (
	userID       int64 = 9101
	honestAmount       = "500.00"
	forgedAmount       = "1000000.00"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	// Migrations run on their own credential. Migrate switches its own
	// connection to ledger_owner rather than granting the credential
	// ledger_owner's privileges, so this pool inherits nothing while a
	// migration run is in flight -- but a credential that can reach
	// ledger_owner at all is still not one to serve traffic on: any session
	// holding it can SET ROLE to that role deliberately. See docs/RUNBOOK.md
	// "Database roles".
	migrateURL := os.Getenv("MIGRATE_DATABASE_URL")
	if migrateURL == "" {
		migrateURL = dbURL
		log.Printf("warning: MIGRATE_DATABASE_URL is unset, so migrations run on DATABASE_URL. " +
			"That credential can act as ledger_owner -- able to drop the append-only guards -- which is not something " +
			"a serving pool should be able to do; Migrate refuses outright if any other session is already connected " +
			"as it. Acceptable for a local example, which migrates before it opens a pool. Not for production.")
	}
	if err := postgres.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	// ---------------------------------------------------------------------
	// 1. Wire the key and the anchor.
	//
	// WithAttestor is what makes journals signed at all. Leave it out and
	// every journal records auth_status=unsigned_no_attestor -- honestly
	// labelled, but nothing to verify later.
	// ---------------------------------------------------------------------
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("generate key seed: %w", err)
	}
	attestor, verifier, err := authdev.NewLocalAttestor(seed, "example-key-1")
	if err != nil {
		return fmt.Errorf("attestor: %w", err)
	}

	anchorDir, err := os.MkdirTemp("", "ledger-anchor-*")
	if err != nil {
		return fmt.Errorf("anchor dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(anchorDir) }()
	anchor := anchordev.NewLocalFileAnchorForDevelopment(filepath.Join(anchorDir, "head.json"))

	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	if err != nil {
		return fmt.Errorf("ledger.New: %w", err)
	}

	currencyUID, jt, wallet, custody, err := setup(ctx, svc)
	if err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// 2. Post an honest journal. It goes out through the normal write path,
	//    so it is signed before the transaction even opens.
	// ---------------------------------------------------------------------
	honest := decimal.RequireFromString(honestAmount)
	journal, err := svc.JournalWriter().PostJournal(ctx, journalInput(
		jt.UID, currencyUID, wallet.UID, custody.UID, honest,
		ledger.NewIdempotencyKey("tamper-evident-honest"),
	))
	if err != nil {
		return fmt.Errorf("post honest journal: %w", err)
	}
	fmt.Printf("1. honest journal posted   uid=%s amount=%s\n", journal.UID, journal.TotalDebit)

	// ---------------------------------------------------------------------
	// 3. Attest the batch. This signs a digest over the batch, chains it to
	//    the previous one, records a per-journal authorization verdict, and
	//    publishes the head to the anchor.
	// ---------------------------------------------------------------------
	attestSvc, err := svc.AttestationService(anchor)
	if err != nil {
		return fmt.Errorf("attestation service: %w", err)
	}
	attested, seq, err := attestSvc.RunAttestBatch(ctx, 500)
	if err != nil {
		return fmt.Errorf("attest: %w", err)
	}
	fmt.Printf("2. attested                seq=%d entries=%d (head published to anchor)\n", seq, attested)

	// ---------------------------------------------------------------------
	// 4. Both balance reads agree, because nothing is wrong yet.
	// ---------------------------------------------------------------------
	if err := reportBalances(ctx, svc, currencyUID, wallet.UID, "3. before the forgery   "); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// 5. Forge a journal the way an attacker with DATABASE_URL would: direct
	//    SQL, perfectly balanced, both entries in one transaction so the
	//    per-journal balance trigger is satisfied. It has no signature,
	//    which is the only thing separating it from a genuine credit.
	// ---------------------------------------------------------------------
	if err := forgeJournal(ctx, pool, jt.UID, currencyUID, wallet.UID, custody.UID); err != nil {
		return fmt.Errorf("forge: %w", err)
	}
	fmt.Printf("4. forged journal inserted straight into the database, amount=%s\n", forgedAmount)

	// ---------------------------------------------------------------------
	// 6. The ordinary balance read counts it. That is not a bug: GetBalance
	//    answers "what do the entries say", and the entries say the money is
	//    there. VerifiedBalance answers a different question -- "is every
	//    journal behind this balance authorized" -- and refuses to return a
	//    number at all when one is not.
	//
	//    It refuses rather than returning a smaller figure on purpose. A
	//    reversal reduces a balance, so silently dropping unauthorized
	//    journals could report MORE money than really exists. Excluding must
	//    never inflate, and per-journal exclusion cannot promise that.
	// ---------------------------------------------------------------------
	if err := reportBalances(ctx, svc, currencyUID, wallet.UID, "5. after the forgery    "); err != nil {
		return err
	}

	// ---------------------------------------------------------------------
	// 7. The gate. Without RequireVerifiedBalance the reservation succeeds --
	//    this is the behaviour every consumer gets by default, and the reason
	//    the flag exists. With it, the withdrawal is refused before any money
	//    moves.
	// ---------------------------------------------------------------------
	withdrawal := decimal.RequireFromString("100.00")

	res, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:  userID,
		CurrencyUID:    currencyUID,
		Amount:         withdrawal,
		IdempotencyKey: ledger.NewIdempotencyKey("tamper-evident-ungated"),
		ExpiresIn:      time.Minute,
	})
	if err != nil {
		return fmt.Errorf("ungated reserve was expected to succeed: %w", err)
	}
	fmt.Printf("6. ungated reserve         SUCCEEDED uid=%s  <- the forged credit paid out\n", res.UID)
	if err := svc.Reserver().Release(ctx, core.ReleaseInput{
		ReservationUID: res.UID,
		IdempotencyKey: ledger.NewIdempotencyKey("tamper-evident-ungated-release"),
	}); err != nil {
		return fmt.Errorf("release: %w", err)
	}

	_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          userID,
		CurrencyUID:            currencyUID,
		Amount:                 withdrawal,
		IdempotencyKey:         ledger.NewIdempotencyKey("tamper-evident-gated"),
		ExpiresIn:              time.Minute,
		RequireVerifiedBalance: true,
	})
	switch {
	case err == nil:
		return fmt.Errorf("gated reserve should have been refused, but it succeeded")
	case errors.Is(err, core.ErrUnauthorizedJournal):
		fmt.Printf("7. gated reserve           REFUSED (%v)\n", core.ErrUnauthorizedJournal)
	default:
		return fmt.Errorf("gated reserve failed for an unexpected reason: %w", err)
	}

	// ---------------------------------------------------------------------
	// 8. The audit, run twice, because the answer changes and the change is
	//    the point.
	//
	//    The forged journal sits in the un-attested tail, so the chain itself
	//    is intact -- but the journal carries no signature while signing is
	//    configured, and that alone is a finding. Verification does not get to
	//    report VERIFIED with a forged credit live in the ledger.
	// ---------------------------------------------------------------------
	report := svc.VerifyLedger(ctx, anchor, service.VerifyConfig{})
	printVerify("8. verify (tail unattested)", report)

	// ---------------------------------------------------------------------
	// 9. Attest again. The forged journal is now inside a batch, and the
	//    UNAUTHORIZED verdict recorded for it becomes part of the signed,
	//    anchored content -- which is both what lets a withdrawal trust a
	//    cached verdict instead of re-verifying every journal on every read,
	//    and now a finding verification reports in its own right. A negative
	//    verdict that stays negative is consistent; consistency is not health.
	// ---------------------------------------------------------------------
	attested, seq, err = attestSvc.RunAttestBatch(ctx, 500)
	if err != nil {
		return fmt.Errorf("second attest: %w", err)
	}
	fmt.Printf("9. attested again          seq=%d entries=%d (the forged journal is now covered)\n", seq, attested)

	report = svc.VerifyLedger(ctx, anchor, service.VerifyConfig{})
	printVerify("10. verify (tail covered) ", report)

	// The gate's answer does not depend on any of this -- it refused before
	// the forgery was ever attested, and it still refuses now. Attestation
	// changes how expensive the answer is to compute, not what it is.
	_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          userID,
		CurrencyUID:            currencyUID,
		Amount:                 withdrawal,
		IdempotencyKey:         ledger.NewIdempotencyKey("tamper-evident-gated-2"),
		ExpiresIn:              time.Minute,
		RequireVerifiedBalance: true,
	})
	if !errors.Is(err, core.ErrUnauthorizedJournal) {
		return fmt.Errorf("gated reserve should still be refused after attestation, got: %v", err)
	}
	fmt.Printf("11. gated reserve          still REFUSED\n")

	// ---------------------------------------------------------------------
	// Appendix — Authorize + PostAuthorized inside RunInTx, and its unsigned
	// contrast. Uses a second currency so it neither reads nor perturbs the
	// wallet/USDT dimension the forgery narrative above depends on; both
	// halves live in one dimension (userID, usdcUID, wallet) so the second
	// journal's effect on VerifiedBalance is visible.
	//
	// This is the pattern service/onchain.go's postDepositConfirmedJournal
	// uses for real money movement composed with a Booker.Transition: call
	// Authorize/AuthorizeTemplate BEFORE RunInTx opens (an Attestor may be
	// remote — calling it from inside an open DB transaction is the
	// "external call inside a transaction" financial.md forbids), then post
	// the result with PostAuthorized *inside* the callback.
	// ---------------------------------------------------------------------
	if err := runInTxAuthorizationDemo(ctx, svc, jt.UID, wallet.UID, custody.UID); err != nil {
		return err
	}

	return nil
}

// runInTxAuthorizationDemo shows the two ways a journal composed inside
// RunInTx ends up with an auth_status, and what each does to
// VerifiedBalance for the dimension it touches.
func runInTxAuthorizationDemo(ctx context.Context, svc *ledger.Service, jtUID, walletUID, custodyUID string) error {
	usdcUID, err := ensureCurrency(ctx, svc, "USDC", "USD Coin")
	if err != nil {
		return err
	}
	signedAmount := decimal.RequireFromString("10.00")
	unsignedAmount := decimal.RequireFromString("5.00")

	// A. Authorize the journal BEFORE opening the transaction -- the only
	//    point at which calling the (possibly remote) Attestor is safe.
	authorized, err := svc.Authorize(ctx, journalInput(
		jtUID, usdcUID, walletUID, custodyUID, signedAmount,
		ledger.NewIdempotencyKey("tamper-evident-authorized-in-tx"),
	))
	if err != nil {
		return fmt.Errorf("appendix: authorize: %w", err)
	}
	var signedJournal *core.Journal
	if err := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		// PostAuthorized never calls the Attestor -- it is the write path
		// that is safe to call from inside an open transaction.
		signedJournal, err = tx.JournalWriter().PostAuthorized(ctx, authorized)
		return err
	}); err != nil {
		return fmt.Errorf("appendix: post authorized journal: %w", err)
	}
	if signedJournal.AuthStatus != core.AuthStatusSigned {
		return fmt.Errorf("appendix: expected auth_status=%s, got %s", core.AuthStatusSigned, signedJournal.AuthStatus)
	}
	verified, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, userID, usdcUID, walletUID)
	if err != nil {
		return fmt.Errorf("appendix: Authorize+PostAuthorized should have kept this dimension verifiable: %w", err)
	}
	fmt.Printf("12. Authorize+PostAuthorized in RunInTx  auth_status=%s  VerifiedBalance=%s\n", signedJournal.AuthStatus, verified)

	// B. The contrast: PostJournal (and ExecuteTemplate/ExecuteTemplateBatch,
	//    which reduce to the same write path) called *directly* inside
	//    RunInTx. There is no point inside an already-open transaction where
	//    calling out to the Attestor would not itself be the "external call
	//    inside a DB transaction" financial.md forbids -- so this journal is
	//    posted, correctly, as unsigned_tx_mode. It moves real money; it is
	//    simply not verifiable.
	var unsignedJournal *core.Journal
	if err := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		unsignedJournal, err = tx.JournalWriter().PostJournal(ctx, journalInput(
			jtUID, usdcUID, walletUID, custodyUID, unsignedAmount,
			ledger.NewIdempotencyKey("tamper-evident-unsigned-in-tx"),
		))
		return err
	}); err != nil {
		return fmt.Errorf("appendix: post journal in tx: %w", err)
	}
	if unsignedJournal.AuthStatus != core.AuthStatusUnsignedTxMode {
		return fmt.Errorf("appendix: expected auth_status=%s, got %s", core.AuthStatusUnsignedTxMode, unsignedJournal.AuthStatus)
	}
	plain, err := svc.BalanceReader().GetBalance(ctx, userID, usdcUID, walletUID)
	if err != nil {
		return fmt.Errorf("appendix: get balance: %w", err)
	}
	_, verifyErr := svc.VerifiedBalanceReader().VerifiedBalance(ctx, userID, usdcUID, walletUID)
	if !errors.Is(verifyErr, core.ErrUnauthorizedJournal) {
		return fmt.Errorf("appendix: expected the unsigned_tx_mode journal to make this dimension UNDEFINED, got: %v", verifyErr)
	}
	fmt.Printf("13. PostJournal in RunInTx (no Authorize) auth_status=%s  GetBalance=%s  VerifiedBalance=UNDEFINED (this legitimate journal is unsigned)\n",
		unsignedJournal.AuthStatus, plain)
	return nil
}

// forgeJournal writes a balanced journal and its two entries with no signature,
// the way a holder of DATABASE_URL would. Both entries go in one transaction
// because migration 044's deferred trigger evaluates per-journal balance at
// commit -- a lone entry on its own connection is refused as unbalanced, so a
// real attacker would use a transaction too. event_id is omitted rather than
// written as 0: migration 045 turned it into a real nullable foreign key.
func forgeJournal(ctx context.Context, pool *pgxpool.Pool, jtUID, currencyUID, walletUID, custodyUID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var journalID int64
	var effectiveAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit,
		                      metadata, actor_id, source, effective_at,
		                      auth_digest, auth_signature, auth_key_id)
		SELECT gen_random_uuid(), jt.id, $1, $2::numeric, $2::numeric,
		       '{}'::jsonb, 0, 'forged-direct-sql', now(),
		       ''::bytea, ''::bytea, ''
		FROM journal_types jt WHERE jt.uid = $3::uuid
		RETURNING id, effective_at
	`, "forged-"+time.Now().Format(time.RFC3339Nano), forgedAmount, jtUID).Scan(&journalID, &effectiveAt); err != nil {
		return fmt.Errorf("insert journal: %w", err)
	}

	insertLeg := func(holder int64, classUID, entryType string) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id,
			                             entry_type, amount, effective_at)
			SELECT $1, $2, c.id, cl.id, $3, $4::numeric, $5
			FROM currencies c, classifications cl
			WHERE c.uid = $6::uuid AND cl.uid = $7::uuid
		`, journalID, holder, entryType, forgedAmount, effectiveAt, currencyUID, classUID)
		return err
	}
	if err := insertLeg(userID, walletUID, "debit"); err != nil {
		return fmt.Errorf("insert debit entry: %w", err)
	}
	if err := insertLeg(core.SystemAccountHolder(userID), custodyUID, "credit"); err != nil {
		return fmt.Errorf("insert credit entry: %w", err)
	}
	return tx.Commit(ctx)
}

func printVerify(label string, report service.VerifyReport) {
	fmt.Printf("%s status=%s chain_seqs=%d entries_rechecked=%d journals_sampled=%d\n",
		label, report.Status, report.ChainSeqsChecked, report.EntriesRechecked, report.JournalsSampled)
	for _, reason := range report.Reasons {
		fmt.Printf("                           - %s\n", reason)
	}
	for seq, ids := range report.MismatchedEntryIDs {
		fmt.Printf("                           - seq %d mismatched entries: %v\n", seq, ids)
	}
}

func reportBalances(ctx context.Context, svc *ledger.Service, currencyUID, walletUID, label string) error {
	plain, err := svc.BalanceReader().GetBalance(ctx, userID, currencyUID, walletUID)
	if err != nil {
		return fmt.Errorf("get balance: %w", err)
	}
	verified, verifyErr := svc.VerifiedBalanceReader().VerifiedBalance(ctx, userID, currencyUID, walletUID)
	switch {
	case verifyErr == nil:
		fmt.Printf("%s GetBalance=%s  VerifiedBalance=%s\n", label, plain, verified)
	case errors.Is(verifyErr, core.ErrUnauthorizedJournal):
		fmt.Printf("%s GetBalance=%s  VerifiedBalance=UNDEFINED (an unauthorized journal contributed)\n", label, plain)
	default:
		return fmt.Errorf("verified balance: %w", verifyErr)
	}
	return nil
}

func journalInput(jtUID, currencyUID, walletUID, custodyUID string, amount decimal.Decimal, key string) core.JournalInput {
	return core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: key,
		Source:         "tamper-evident-example",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: currencyUID, ClassificationUID: walletUID,
				EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: currencyUID, ClassificationUID: custodyUID,
				EntryType: core.EntryTypeCredit, Amount: amount},
		},
	}
}

func setup(ctx context.Context, svc *ledger.Service) (currencyUID string, jt *core.JournalType, wallet, custody *core.Classification, err error) {
	currencyUID, err = ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return "", nil, nil, nil, err
	}
	if jt, err = ensureJournalType(ctx, svc, "te_credit", "Tamper-Evident Credit"); err != nil {
		return "", nil, nil, nil, err
	}
	// balance_role=available is what makes this classification visible to
	// Reserve; without it the withdrawal path sees no funds at all.
	if wallet, err = ensureClassification(ctx, svc, "te_wallet", "TE Wallet",
		core.NormalSideDebit, false, core.BalanceRoleAvailable); err != nil {
		return "", nil, nil, nil, err
	}
	if custody, err = ensureClassification(ctx, svc, "te_custodial", "TE Custodial",
		core.NormalSideCredit, true, core.BalanceRoleNone); err != nil {
		return "", nil, nil, nil, err
	}
	return currencyUID, jt, wallet, custody, nil
}

func ensureJournalType(ctx context.Context, svc *ledger.Service, code, name string) (*core.JournalType, error) {
	jt, err := svc.JournalTypes().GetJournalTypeByCode(ctx, code)
	if err == nil {
		return jt, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("get journal type %s: %w", code, err)
	}
	return svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: name})
}

func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
	const exponent = int32(6)
	for _, c := range list {
		if c.Code != code {
			continue
		}
		if c.Exponent != exponent {
			return "", fmt.Errorf("currency %s already exists with exponent %d, this example expects %d", code, c.Exponent, exponent)
		}
		return c.UID, nil
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: exponent})
	if err != nil {
		return "", fmt.Errorf("create currency: %w", err)
	}
	return created.UID, nil
}

func ensureClassification(ctx context.Context, svc *ledger.Service, code, name string,
	side core.NormalSide, system bool, role core.BalanceRole) (*core.Classification, error) {
	c, err := svc.Classifications().GetByCode(ctx, code)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("get classification %s: %w", code, err)
	}
	return svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: code, Name: name, NormalSide: side, IsSystem: system, BalanceRole: role,
	})
}
