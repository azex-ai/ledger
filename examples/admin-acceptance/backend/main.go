// A minimal authenticated library host. This example is staged into its own
// GOWORK=off consumer module by ../stage.py; it does not expose demo wallets.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/slogadapter"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := server.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.Env != "production" {
		return fmt.Errorf("this acceptance host requires ENV=production")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	seedBytes, err := base64.StdEncoding.DecodeString(os.Getenv("LEDGER_SIGNING_SEED"))
	if err != nil {
		return fmt.Errorf("decode signing seed: %w", err)
	}
	attestor, verifier, err := authdev.NewLocalAttestor(seedBytes, "admin-acceptance-local")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	migrateURL := os.Getenv("MIGRATE_DATABASE_URL")
	if migrateURL == "" {
		migrateURL = dbURL
	} // isolated local acceptance database only
	if err := ledger.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier), ledger.WithLogger(slogadapter.New(slog.Default())))
	if err != nil {
		return err
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, time.Minute)
	defer setupCancel()
	if err := seed(setupCtx, svc); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	api, err := newLedgerAPI(svc, cfg)
	if err != nil {
		return err
	}
	gcStop := make(chan struct{})
	api.StartRateLimiterGC(gcStop)
	defer close(gcStop)
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		return fmt.Errorf("HTTP_PORT is required")
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: api, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("authenticated ledger API on %s", srv.Addr)
	err = srv.ListenAndServe()
	cancel()
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func seed(ctx context.Context, svc *ledger.Service) error {
	if err := presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), presets.DepositBundle()); err != nil {
		return err
	}
	if _, err := presets.InstallDepositClassification(ctx, svc.Classifications()); err != nil {
		return err
	}
	currencies, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return err
	}
	uids := make(map[string]string)
	for _, spec := range []struct {
		code, name, amount string
		exponent           int32
	}{
		{"USD", "US Dollar", "1234.56", 2},
		{"USDC", "USD Coin", "1.234567", 6},
		{"BTC", "Bitcoin", "0.00000001", 8},
		{"POINTS", "Reward points", "42", 0},
		{"ATOM", "Precision units", "999999999999.123456789012345678", 18},
	} {
		uid := ""
		for _, existing := range currencies {
			if existing.Code == spec.code {
				if existing.Exponent != spec.exponent {
					return fmt.Errorf("%s exponent mismatch", spec.code)
				}
				uid = existing.UID
			}
		}
		if uid == "" {
			c, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: spec.code, Name: spec.name, Exponent: spec.exponent})
			if err != nil {
				return err
			}
			uid = c.UID
		}
		uids[spec.code] = uid
		_, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
			HolderID: 1001, CurrencyUID: uid, IdempotencyKey: "admin-acceptance-v1:fund:" + spec.code,
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString(spec.amount)}, Source: "admin-acceptance",
		})
		if err != nil {
			return err
		}
		if spec.code == "USD" {
			_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{AccountHolder: 1001, CurrencyUID: uid, Amount: decimal.RequireFromString("34.56"), IdempotencyKey: "admin-acceptance-v1:hold", ExpiresIn: 24 * time.Hour, RequireVerifiedBalance: true})
			if err != nil {
				return err
			}
		}
		if spec.code == "USDC" {
			_, err = svc.Booker().CreateBooking(ctx, core.CreateBookingInput{ClassificationCode: "deposit", AccountHolder: 1001, CurrencyUID: uid, Amount: decimal.RequireFromString("5.000001"), IdempotencyKey: "admin-acceptance-v1:pending", ChannelName: "acceptance", Metadata: map[string]string{"purpose": "pending is not credited"}})
			if err != nil {
				return err
			}
		}
	}
	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: 1002, CurrencyUID: uids["ATOM"], IdempotencyKey: "admin-acceptance-v1:tiny",
		Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("0.000000000000000001")}, Source: "admin-acceptance",
	})
	if err != nil {
		return err
	}
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return err
	}
	custodial, err := svc.Classifications().GetByCode(ctx, "custodial")
	if err != nil {
		return err
	}
	kind, err := svc.JournalTypes().GetJournalTypeByCode(ctx, "deposit_confirm")
	if err != nil {
		return err
	}
	input := core.JournalInput{JournalTypeUID: kind.UID, IdempotencyKey: "admin-acceptance-v1:multi-currency", Source: "admin-acceptance"}
	for _, leg := range []struct{ code, amount string }{{"USD", "1.23"}, {"BTC", "0.00000002"}} {
		amount := decimal.RequireFromString(leg.amount)
		input.Entries = append(input.Entries,
			core.EntryInput{AccountHolder: 1003, CurrencyUID: uids[leg.code], ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amount},
			core.EntryInput{AccountHolder: -1, CurrencyUID: uids[leg.code], ClassificationUID: custodial.UID, EntryType: core.EntryTypeCredit, Amount: amount},
		)
	}
	if _, err := svc.JournalWriter().PostJournal(ctx, input); err != nil {
		return err
	}

	return nil
}

func newLedgerAPI(svc *ledger.Service, cfg *server.Config) (*server.Server, error) {

	// The HTTP layer is assembled entirely from svc.* accessors -- no direct
	// postgres/service dependency (MJ-5, 2026-08-29 review). The snapshot and
	// system-rollup services the server used to take are Worker concerns, not
	// HTTP ones, and are wired into the Worker separately.
	//
	// NewFromDeps over NewWithConfig: naming each dependency by struct field
	// instead of position removes the "two swapped same-shaped interface
	// arguments compile clean and fail at runtime" trap NewWithConfig has,
	// and it returns an error instead of panicking on an invalid Config.
	srv, err := server.NewFromDeps(cfg, server.Deps{
		Journals:         svc.JournalWriter(),
		Balances:         svc.BalanceReader(),
		Reserver:         svc.Reserver(),
		Booker:           svc.Booker(),
		BookingReader:    svc.BookingReader(),
		EventReader:      svc.EventReader(),
		Classifications:  svc.Classifications(),
		JournalTypes:     svc.JournalTypes(),
		Templates:        svc.Templates(),
		Currencies:       svc.Currencies(),
		Channels:         svc.Channels(),
		Reconciler:       svc.Reconciler(),
		Queries:          svc.Queries(),
		Audit:            svc.Audit(),
		PlatformBalances: svc.PlatformBalanceReader(),
		Solvency:         svc.SolvencyChecker(),
		BalanceTrends:    svc.BalanceTrends(),
		FullReconciler:   svc.FullReconciler(service.FullReconciliationConfig{}),
		AccountPolicies:  svc.AccountPolicies(),
		PeriodCloser:     svc.PeriodCloser(),
		TrialBalance:     svc.TrialBalanceReader(),
	})
	if err != nil {
		return nil, fmt.Errorf("server.NewFromDeps: %w", err)
	}
	srv.SetReady(true)
	return srv, nil
}
