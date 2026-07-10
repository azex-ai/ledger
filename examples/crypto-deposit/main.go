// Example: end-to-end EVM deposit booking, driven through the orchestrated
// crypto-deposit shape (docs/plans/2026-07-11-crypto-deposit-sweep-design.md).
//
// Two steps, mirroring the real production flow:
//
//  1. ensureDepositAddress: derive (CREATE2) + register a holder's custody
//     address -- idempotent, safe to call repeatedly.
//  2. ingestDeposit: normalize an observed on-chain transfer into a
//     core.DepositSighting and drive it through create-or-advance booking +
//     accounting. This is the single entry point BOTH the chains/evm
//     watcher (pull, polling eth_getLogs) and the onchain webhook (push, see
//     server/handler_webhooks.go) call in production -- here it's driven by
//     a simulated sighting instead of a real chain, since this example only
//     needs to demonstrate the shape.
//
// The AddressRegistry used below is an in-memory stand-in for
// postgres.NewDepositAddressStore (S1 #2, not part of this branch yet) --
// swap it for the real Postgres-backed core.AddressRegistry once available;
// nothing about ensureDepositAddress/ingestDeposit changes. Once
// service.OnchainService lands, these two local helpers are replaced
// one-for-one by svc.Onchain().EnsureDepositAddress(ctx, holder) and
// svc.Onchain().IngestDeposit(ctx, sighting) -- same signatures, same
// idempotency contract.
//
// The older hand-rolled 3-step flow (CreateBooking -> Transition(confirming)
// -> Transition(confirmed)+journal, calling Booker/JournalWriter directly)
// still works unchanged and is exactly what ingestDeposit does internally --
// see examples/event-subscribe for that shape used standalone.
//
// Run order:
//
//  1. Start Postgres and set DATABASE_URL.
//  2. go run ./examples/crypto-deposit
//
// Assumes a "deposit" classification (with a lifecycle -- see
// presets.DepositLifecycle) and the deposit accounting bundle
// (presets.DepositBundle) have been installed.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

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

	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}

	// Presets need the concrete store handles. ClassificationStore satisfies
	// both ClassificationStore and JournalTypeStore.
	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	if err := presets.InstallTemplateBundle(ctx, classStore, classStore, tmplStore, presets.DepositBundle()); err != nil {
		return fmt.Errorf("install deposit bundle: %w", err)
	}

	// -- 1. Address issuance -------------------------------------------
	registry := newInMemoryAddressRegistry()
	chain := core.ChainConfig{
		ChainID:       1,
		Confirmations: 12,
		Factory:       "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323",
		InitHash:      "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece",
		CreditTokens: map[string]core.TokenConfig{
			"0xusdt": {TokenAddress: "0xusdt", CurrencyCode: "USDT", Decimals: 6},
		},
	}

	holder := int64(1001)
	depositAddr, err := ensureDepositAddress(ctx, registry, chain, holder)
	if err != nil {
		return fmt.Errorf("ensure deposit address: %w", err)
	}
	fmt.Printf("holder %d deposit address: %s\n", holder, depositAddr.Address)

	// -- 2. Ingestion (simulating a watcher/webhook sighting) -----------
	sighting := core.DepositSighting{
		ChainID:       chain.ChainID,
		TxHash:        "0xabc123",
		TxLogSeq:      0,
		Token:         "0xusdt",
		From:          "0xsender",
		To:            depositAddr.Address,
		Amount:        decimal.RequireFromString("500.00"),
		Confirmations: chain.Confirmations, // already past threshold -> confirms immediately
	}
	booking, err := ingestDeposit(ctx, svc, registry, currencyUID, sighting, chain.Confirmations)
	if err != nil {
		return fmt.Errorf("ingest deposit: %w", err)
	}
	fmt.Printf("booking uid=%s status=%s journal_uid=%s\n", booking.UID, booking.Status, booking.JournalUID)
	return nil
}

// ensureDepositAddress is a minimal stand-in for
// svc.Onchain().EnsureDepositAddress(ctx, holder) (design doc §2) -- derive
// the CREATE2 address, then register it (idempotent upsert). Once
// service.OnchainService lands (S1 #2) this local helper goes away entirely.
func ensureDepositAddress(ctx context.Context, registry core.AddressRegistry, chain core.ChainConfig, holder int64) (*core.DepositAddress, error) {
	address, err := core.DeriveDepositAddress(chain.Factory, chain.InitHash, holder)
	if err != nil {
		return nil, fmt.Errorf("derive deposit address: %w", err)
	}
	return registry.EnsureAddress(ctx, core.AddressRegistrationInput{
		AccountHolder: holder,
		Address:       address,
		Factory:       chain.Factory,
		InitHash:      chain.InitHash,
	})
}

// ingestDeposit is a minimal stand-in for svc.Onchain().IngestDeposit(ctx,
// sighting) (design doc §3). Idempotency key =
// deposit-{chain_id}-{tx_hash}-{txlog_seq} -- deliberately not the chain's
// block-level log_index, which is reassigned across a reorg and would mint a
// fresh key for an already-recorded transfer.
func ingestDeposit(ctx context.Context, svc *ledger.Service, registry core.AddressRegistry, currencyUID string, sighting core.DepositSighting, requiredConfirmations int32) (*core.Booking, error) {
	if err := sighting.Validate(); err != nil {
		return nil, err
	}

	owner, err := registry.GetByAddress(ctx, sighting.To)
	if err != nil {
		return nil, fmt.Errorf("resolve holder for address %s: %w", sighting.To, err)
	}

	idemKey := fmt.Sprintf("deposit-%d-%s-%d", sighting.ChainID, sighting.TxHash, sighting.TxLogSeq)
	booking, err := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      owner.AccountHolder,
		CurrencyUID:        currencyUID,
		Amount:             sighting.Amount,
		IdempotencyKey:     idemKey,
		ChannelName:        "evm",
		Metadata: map[string]string{
			"chain_id": fmt.Sprintf("%d", sighting.ChainID),
			"tx_hash":  sighting.TxHash,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create booking: %w", err)
	}

	if sighting.Confirmations < requiredConfirmations {
		return booking, nil // still pending; caller re-checks on the next sighting
	}

	var confirmedEvent *core.Event
	err = svc.RunInTx(ctx, func(txSvc *ledger.Service) error {
		evt, err := txSvc.Booker().Transition(ctx, core.TransitionInput{
			BookingUID: booking.UID,
			ToStatus:   "confirmed",
			ChannelRef: sighting.TxHash,
			Amount:     sighting.Amount,
			Source:     "example.crypto_deposit",
		})
		if err != nil {
			return err
		}

		if _, err := txSvc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
			HolderID:       booking.AccountHolder,
			CurrencyUID:    booking.CurrencyUID,
			IdempotencyKey: fmt.Sprintf("deposit-confirm-journal:%s", booking.UID),
			EventUID:       evt.UID,
			Amounts:        map[string]decimal.Decimal{"amount": sighting.Amount},
			Source:         "example.crypto_deposit",
		}); err != nil {
			return err
		}
		confirmedEvent = evt
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("transition confirmed + journal: %w", err)
	}
	_ = confirmedEvent

	return svc.BookingReader().GetBooking(ctx, booking.UID)
}

// inMemoryAddressRegistry is a minimal stand-in for
// postgres.NewDepositAddressStore(pool) (S1 #2, not part of this branch
// yet) -- swap it for the real Postgres-backed core.AddressRegistry in
// production; ensureDepositAddress/ingestDeposit don't change.
type inMemoryAddressRegistry struct {
	byHolder map[int64]*core.DepositAddress
}

func newInMemoryAddressRegistry() *inMemoryAddressRegistry {
	return &inMemoryAddressRegistry{byHolder: make(map[int64]*core.DepositAddress)}
}

func (r *inMemoryAddressRegistry) EnsureAddress(ctx context.Context, input core.AddressRegistrationInput) (*core.DepositAddress, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	if existing, ok := r.byHolder[input.AccountHolder]; ok {
		return existing, nil
	}
	addr := &core.DepositAddress{
		UID:           fmt.Sprintf("addr-%d", input.AccountHolder),
		AccountHolder: input.AccountHolder,
		Address:       input.Address,
		Factory:       input.Factory,
		InitHash:      input.InitHash,
		CreatedAt:     time.Now(),
	}
	r.byHolder[input.AccountHolder] = addr
	return addr, nil
}

func (r *inMemoryAddressRegistry) GetByAddress(ctx context.Context, address string) (*core.DepositAddress, error) {
	for _, a := range r.byHolder {
		if a.Address == address {
			return a, nil
		}
	}
	return nil, core.ErrNotFound
}

func (r *inMemoryAddressRegistry) ListAddresses(ctx context.Context) ([]core.DepositAddress, error) {
	out := make([]core.DepositAddress, 0, len(r.byHolder))
	for _, a := range r.byHolder {
		out = append(out, *a)
	}
	return out, nil
}

func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
	for _, c := range list {
		if c.Code == code {
			return c.UID, nil
		}
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: 18})
	if err != nil {
		return "", fmt.Errorf("create currency: %w", err)
	}
	return created.UID, nil
}
