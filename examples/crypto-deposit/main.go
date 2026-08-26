// Example: end-to-end EVM deposit booking, driven through the production
// crypto-deposit orchestration (docs/plans/2026-07-11-crypto-deposit-sweep-design.md).
//
// Two calls, mirroring the real production flow:
//
//  1. svc.EnableOnchain(...).EnsureDepositAddress -- derive (CREATE2) + durably
//     register a holder's custody address. Idempotent, safe to call repeatedly.
//  2. onchain.IngestDeposit -- normalize an observed on-chain transfer into a
//     core.DepositSighting and drive it through create-or-advance booking +
//     accounting, including the M3 AutoCreditCeiling review gate (design doc
//     §9.2). This is the single entry point BOTH the chains/evm watcher
//     (pull, polling eth_getLogs) and the onchain webhook (push, see
//     server/handler_webhooks.go) call in production -- here it's driven by
//     a simulated sighting instead of a real chain, since this example only
//     needs to demonstrate the shape.
//
// EnableOnchain's reader/scanner/sweeper are all nil here: this example only
// exercises the two calls above, which work standalone (e.g. from an HTTP
// handler) without the background watch/sweep loops ever running -- see
// service.OnchainDeps.validateCore's doc comment. A real deployment supplies
// a chains/evm.Reader/Scanner/Sweeper for those loops; nothing about
// EnsureDepositAddress/IngestDeposit changes when it does.
//
// The address registry and the AutoCreditCeiling gate below are NOT
// hand-rolled: EnableOnchain wires postgres.NewDepositAddressStore (durable,
// survives restarts) and IngestDeposit enforces AutoCreditCeiling itself. An
// earlier version of this example reimplemented both by hand -- an in-memory
// registry that forgot every address on restart, and an ingest path with no
// ceiling at all -- while its own comment said the production API "isn't
// part of this branch yet." It was: this file now calls it.
//
// The older hand-rolled 3-step flow (CreateBooking -> Transition(confirming)
// -> Transition(confirmed)+journal, calling Booker/JournalWriter directly)
// still works unchanged and is exactly what IngestDeposit does internally --
// see examples/event-subscribe for that shape used standalone.
//
// Run order:
//
//  1. Start Postgres and set DATABASE_URL.
//  2. go run ./examples/crypto-deposit
//
// Installs everything it needs: the deposit accounting bundle
// (presets.DepositBundle) and presets.DepositLifecycle on the "deposit"
// classification.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
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
	if err := postgres.Migrate(dbURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	// One-line ledger wiring via the top-level facade.
	svc, err := ledger.New(pool)
	if err != nil {
		return fmt.Errorf("ledger facade: %w", err)
	}

	if _, err := ensureCurrency(ctx, svc, "USDT", "Tether USD", 6); err != nil {
		return err
	}

	// Presets need the concrete store handles. ClassificationStore satisfies
	// both ClassificationStore and JournalTypeStore.
	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	if err := presets.InstallTemplateBundle(ctx, classStore, classStore, tmplStore, presets.DepositBundle()); err != nil {
		return fmt.Errorf("install deposit bundle: %w", err)
	}

	// The accounting bundle installs templates, not lifecycles -- and the
	// schema ships a label-only "deposit" classification, so the bundle finds
	// one already there and leaves it alone. Without a lifecycle, no booking
	// can be opened against it. SetLifecycleIfEmpty exists for exactly this:
	// it seeds one only when the classification has none, so an operator's
	// customised lifecycle is never clobbered.
	if err := installDepositLifecycle(ctx, classStore); err != nil {
		return err
	}

	// -- Wire the onchain subsystem --------------------------------------
	//
	// AutoCreditCeiling MUST be set on every CreditTokens entry (M3.1
	// secure-by-default, design doc §9.2 addendum) -- EnableOnchain refuses
	// to hand back an *Onchain otherwise. 300 USDT is this example's
	// deliberately low ceiling so it can demonstrate both sides of the gate
	// below with round numbers; a real deployment sets this to its own risk
	// tolerance for a single-RPC-source auto-credit.
	const ceiling = "300"
	chain := core.ChainConfig{
		ChainID:       1,
		Confirmations: 12,
		Factory:       "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323",
		InitHash:      "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece",
		CreditTokens: map[string]core.TokenConfig{
			"0xusdt": {
				TokenAddress:      "0xusdt",
				CurrencyCode:      "USDT",
				Decimals:          6,
				AutoCreditCeiling: decimal.RequireFromString(ceiling),
			},
		},
	}
	// reader/scanner/sweeper are nil: this example drives EnsureDepositAddress
	// and IngestDeposit directly, the same way a webhook handler would,
	// without ever calling onchain.Run (which starts the watch/sweep loops).
	onchain, err := svc.EnableOnchain(core.ChainSet{chain.ChainID: chain}, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("enable onchain: %w", err)
	}

	// -- 1. Address issuance -----------------------------------------------
	holder := int64(1001)
	depositAddr, err := onchain.EnsureDepositAddress(ctx, holder)
	if err != nil {
		return fmt.Errorf("ensure deposit address: %w", err)
	}
	fmt.Printf("holder %d deposit address: %s (persisted in postgres, survives a restart)\n", holder, depositAddr.Address)

	// -- 2. Ingestion: a deposit within the ceiling auto-credits -----------
	withinCeiling := core.DepositSighting{
		ChainID:       chain.ChainID,
		TxHash:        "0xabc123",
		TxLogSeq:      0,
		Token:         "0xusdt",
		From:          "0xsender",
		To:            depositAddr.Address,
		Amount:        decimal.RequireFromString("200.00"), // <= ceiling
		Confirmations: chain.Confirmations,                 // already past threshold -> confirms immediately
		BlockNumber:   1_000_000,
	}
	booking, err := onchain.IngestDeposit(ctx, withinCeiling)
	if err != nil {
		return fmt.Errorf("ingest deposit (within ceiling): %w", err)
	}
	fmt.Printf("within-ceiling deposit: booking uid=%s status=%s journal_uid=%s (auto-credited)\n",
		booking.UID, booking.Status, booking.JournalUID)

	// -- 3. Ingestion: a deposit OVER the ceiling is parked, not credited --
	//
	// This is the M3 compensating control the earlier version of this
	// example bypassed entirely by hand-rolling its own ingest path: a
	// single-RPC-source sighting above the ceiling does not auto-credit --
	// it stops at "review" with no journal, until ApproveReview or
	// RejectReview (both on *service.Onchain) is called by a human.
	overCeiling := core.DepositSighting{
		ChainID:       chain.ChainID,
		TxHash:        "0xdef456",
		TxLogSeq:      0,
		Token:         "0xusdt",
		From:          "0xsender",
		To:            depositAddr.Address,
		Amount:        decimal.RequireFromString("5000.00"), // > ceiling
		Confirmations: chain.Confirmations,
		BlockNumber:   1_000_001,
	}
	reviewBooking, err := onchain.IngestDeposit(ctx, overCeiling)
	if err != nil {
		return fmt.Errorf("ingest deposit (over ceiling): %w", err)
	}
	if reviewBooking.Status != "review" || reviewBooking.JournalUID != "" {
		return fmt.Errorf("over-ceiling deposit was not parked for review: status=%s journal_uid=%s -- the AutoCreditCeiling gate did not hold",
			reviewBooking.Status, reviewBooking.JournalUID)
	}
	fmt.Printf("over-ceiling deposit: booking uid=%s status=%s journal_uid=%q (parked for human review, NOT auto-credited)\n",
		reviewBooking.UID, reviewBooking.Status, reviewBooking.JournalUID)

	return nil
}

// ensureCurrency creates currency code/name if it doesn't already exist
// (idempotent), and errors if a currency with that code exists at a
// different exponent -- precision is a property of the money, not something
// a second caller gets to silently redefine.
func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string, exponent int32) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
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
		return "", fmt.Errorf("create currency %s: %w", code, err)
	}
	return created.UID, nil
}

// installDepositLifecycle attaches presets.DepositLifecycle to the "deposit"
// classification the schema seeds as label-only.
func installDepositLifecycle(ctx context.Context, classStore *postgres.ClassificationStore) error {
	class, err := classStore.GetByCode(ctx, presets.DepositClassificationCode)
	if err != nil {
		return fmt.Errorf("get deposit classification: %w", err)
	}
	if err := classStore.SetLifecycleIfEmpty(ctx, class.UID, presets.DepositLifecycle); err != nil {
		return fmt.Errorf("install deposit lifecycle: %w", err)
	}
	return nil
}
