// Package main demonstrates the full ledger library workflow for an EVM CREATE2
// crypto deposit, including metadata setup, deposit lifecycle, reserve/settle,
// balance queries, and reconciliation.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

func main() {
	ctx := context.Background()

	// ---------------------------------------------------------------
	// 1. Connect to PostgreSQL
	// ---------------------------------------------------------------
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect to postgres: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping postgres: %v", err)
	}
	fmt.Println("Connected to PostgreSQL")

	// ---------------------------------------------------------------
	// 2. Run migrations
	// ---------------------------------------------------------------
	migrateURL := databaseURL
	// golang-migrate uses pgx5:// scheme for pgx v5 driver
	if strings.HasPrefix(migrateURL, "postgres://") {
		migrateURL = "pgx5" + migrateURL[len("postgres"):]
	} else if strings.HasPrefix(migrateURL, "postgresql://") {
		migrateURL = "pgx5" + migrateURL[len("postgresql"):]
	}

	if err := postgres.Migrate(migrateURL); err != nil {
		log.Fatalf("run migrations: %v", err)
	}
	fmt.Println("Migrations applied")

	// ---------------------------------------------------------------
	// 3. Initialize stores
	// ---------------------------------------------------------------
	engine := core.NewEngine()

	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	templateStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)
	depositStore := postgres.NewDepositStore(pool)
	reserverStore := postgres.NewReserverStore(pool, ledgerStore)

	_ = engine // engine is used for service-layer wiring; stores work standalone

	// ---------------------------------------------------------------
	// 4. Create currency: USDT
	// ---------------------------------------------------------------
	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT",
		Name: "Tether USD",
	})
	if err != nil {
		log.Fatalf("create currency: %v", err)
	}
	fmt.Printf("Currency created: %s (id=%d)\n", usdt.Code, usdt.ID)

	// ---------------------------------------------------------------
	// 5. Create classifications
	// ---------------------------------------------------------------
	type classSpec struct {
		code       string
		name       string
		normalSide core.NormalSide
		isSystem   bool
	}

	specs := []classSpec{
		{"main_wallet", "Main Wallet", core.NormalSideDebit, false},
		{"custodial", "Custodial", core.NormalSideCredit, true},
		{"suspense", "Suspense", core.NormalSideDebit, true},
		{"pending", "Pending Deposit", core.NormalSideCredit, false},
		{"fees", "Fees Collected", core.NormalSideCredit, true},
	}

	classIDs := make(map[string]int64, len(specs))
	for _, s := range specs {
		cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
			Code:       s.code,
			Name:       s.name,
			NormalSide: s.normalSide,
			IsSystem:   s.isSystem,
		})
		if err != nil {
			log.Fatalf("create classification %s: %v", s.code, err)
		}
		classIDs[s.code] = cls.ID
		fmt.Printf("Classification created: %s (id=%d, side=%s)\n", cls.Code, cls.ID, cls.NormalSide)
	}

	// ---------------------------------------------------------------
	// 6. Create journal type and entry template for deposit confirmation
	// ---------------------------------------------------------------
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit",
		Name: "Deposit Confirmation",
	})
	if err != nil {
		log.Fatalf("create journal type: %v", err)
	}
	fmt.Printf("Journal type created: %s (id=%d)\n", jt.Code, jt.ID)

	// Template: deposit_confirm
	//   DR MainWallet(user)   amount
	//   CR Custodial(system)  amount
	tmpl, err := templateStore.CreateTemplate(ctx, core.TemplateInput{
		Code:          "deposit_confirm",
		Name:          "Confirm Deposit",
		JournalTypeID: jt.ID,
		Lines: []core.TemplateLineInput{
			{
				ClassificationID: classIDs["main_wallet"],
				EntryType:        core.EntryTypeDebit,
				HolderRole:       core.HolderRoleUser,
				AmountKey:        "amount",
				SortOrder:        1,
			},
			{
				ClassificationID: classIDs["custodial"],
				EntryType:        core.EntryTypeCredit,
				HolderRole:       core.HolderRoleSystem,
				AmountKey:        "amount",
				SortOrder:        2,
			},
		},
	})
	if err != nil {
		log.Fatalf("create template: %v", err)
	}
	fmt.Printf("Template created: %s (id=%d, lines=%d)\n", tmpl.Code, tmpl.ID, len(tmpl.Lines))

	// ---------------------------------------------------------------
	// 7. Simulate EVM CREATE2 deposit flow
	// ---------------------------------------------------------------
	const userID int64 = 1001
	depositAmount := decimal.NewFromFloat(500.00)
	txHash := "0xabc123def456789abcdef0123456789abcdef0123456789abcdef0123456789a"

	fmt.Println("\n--- Deposit Flow ---")

	// 7a. InitDeposit (pending)
	dep, err := depositStore.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  userID,
		CurrencyID:     usdt.ID,
		ExpectedAmount: depositAmount,
		ChannelName:    "evm_create2",
		IdempotencyKey: "deposit:user1001:1",
		Metadata: map[string]string{
			"chain":   "base",
			"address": "0x742d35Cc6634C0532925a3b844Bc9e7595f2bD18",
		},
	})
	if err != nil {
		log.Fatalf("init deposit: %v", err)
	}
	fmt.Printf("Deposit created: id=%d status=%s expected=%s\n",
		dep.ID, dep.Status, dep.ExpectedAmount)

	// 7b. ConfirmingDeposit (chain detected tx, waiting for confirmations)
	if err := depositStore.ConfirmingDeposit(ctx, dep.ID, txHash); err != nil {
		log.Fatalf("confirming deposit: %v", err)
	}
	fmt.Printf("Deposit confirming: channel_ref=%s\n", txHash)

	// 7c. ConfirmDeposit (12 confirmations reached, record actual amount)
	if err := depositStore.ConfirmDeposit(ctx, core.ConfirmDepositInput{
		DepositID:    dep.ID,
		ActualAmount: depositAmount,
		ChannelRef:   txHash,
	}); err != nil {
		log.Fatalf("confirm deposit: %v", err)
	}
	fmt.Println("Deposit confirmed")

	// 7d. Post the journal using the template to credit user's MainWallet
	journal, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     usdt.ID,
		IdempotencyKey: fmt.Sprintf("deposit_journal:%s", txHash),
		Amounts: map[string]decimal.Decimal{
			"amount": depositAmount,
		},
		Source:   "deposit_confirm",
		Metadata: map[string]string{"tx_hash": txHash},
	})
	if err != nil {
		log.Fatalf("execute template: %v", err)
	}
	fmt.Printf("Journal posted: id=%d total_debit=%s total_credit=%s\n",
		journal.ID, journal.TotalDebit, journal.TotalCredit)

	// ---------------------------------------------------------------
	// 8. Query balance -- MainWallet should show 500 USDT
	// ---------------------------------------------------------------
	fmt.Println("\n--- Balance Query ---")

	balances, err := ledgerStore.GetBalances(ctx, userID, usdt.ID)
	if err != nil {
		log.Fatalf("get balances: %v", err)
	}
	for _, b := range balances {
		fmt.Printf("  holder=%d currency=%d classification=%d balance=%s\n",
			b.AccountHolder, b.CurrencyID, b.ClassificationID, b.Balance)
	}

	mainBalance, err := ledgerStore.GetBalance(ctx, userID, usdt.ID, classIDs["main_wallet"])
	if err != nil {
		log.Fatalf("get main wallet balance: %v", err)
	}
	fmt.Printf("MainWallet balance: %s USDT\n", mainBalance)

	// ---------------------------------------------------------------
	// 9. Reserve -> Settle (simulate a spend of 100 USDT)
	// ---------------------------------------------------------------
	fmt.Println("\n--- Reserve/Settle Flow ---")

	spendAmount := decimal.NewFromFloat(100.00)

	reservation, err := reserverStore.Reserve(ctx, core.ReserveInput{
		AccountHolder:  userID,
		CurrencyID:     usdt.ID,
		Amount:         spendAmount,
		IdempotencyKey: "spend:user1001:order42",
		ExpiresIn:      15 * time.Minute,
	})
	if err != nil {
		log.Fatalf("reserve: %v", err)
	}
	fmt.Printf("Reserved: id=%d amount=%s status=%s\n",
		reservation.ID, reservation.ReservedAmount, reservation.Status)

	// Settle with actual amount (could differ from reserved)
	settleAmount := decimal.NewFromFloat(95.50) // actual cost was 95.50
	if err := reserverStore.Settle(ctx, reservation.ID, settleAmount); err != nil {
		log.Fatalf("settle: %v", err)
	}
	fmt.Printf("Settled: amount=%s\n", settleAmount)

	// Post a spend journal for the settled amount
	spendJT, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "spend",
		Name: "User Spend",
	})
	if err != nil {
		log.Fatalf("create spend journal type: %v", err)
	}

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  spendJT.ID,
		IdempotencyKey: "spend_journal:user1001:order42",
		Entries: []core.EntryInput{
			{
				AccountHolder:    userID,
				CurrencyID:       usdt.ID,
				ClassificationID: classIDs["main_wallet"],
				EntryType:        core.EntryTypeCredit, // reduce wallet
				Amount:           settleAmount,
			},
			{
				AccountHolder:    core.SystemAccountHolder(userID),
				CurrencyID:       usdt.ID,
				ClassificationID: classIDs["fees"],
				EntryType:        core.EntryTypeDebit, // fees earned
				Amount:           settleAmount,
			},
		},
		Source:   "spend",
		Metadata: map[string]string{"order_id": "42"},
	})
	if err != nil {
		log.Fatalf("post spend journal: %v", err)
	}
	fmt.Println("Spend journal posted")

	// ---------------------------------------------------------------
	// 10. Print final balances
	// ---------------------------------------------------------------
	fmt.Println("\n--- Final Balances ---")

	finalBalance, err := ledgerStore.GetBalance(ctx, userID, usdt.ID, classIDs["main_wallet"])
	if err != nil {
		log.Fatalf("get final balance: %v", err)
	}
	fmt.Printf("MainWallet: %s USDT (expected: 404.50)\n", finalBalance)

	allBalances, err := ledgerStore.GetBalances(ctx, userID, usdt.ID)
	if err != nil {
		log.Fatalf("get all balances: %v", err)
	}
	for _, b := range allBalances {
		classCode := "unknown"
		for code, id := range classIDs {
			if id == b.ClassificationID {
				classCode = code
				break
			}
		}
		fmt.Printf("  %s (id=%d): %s\n", classCode, b.ClassificationID, b.Balance)
	}

	// System side balances
	sysBalances, err := ledgerStore.GetBalances(ctx, core.SystemAccountHolder(userID), usdt.ID)
	if err != nil {
		log.Fatalf("get system balances: %v", err)
	}
	fmt.Println("\nSystem counterpart balances:")
	for _, b := range sysBalances {
		classCode := "unknown"
		for code, id := range classIDs {
			if id == b.ClassificationID {
				classCode = code
				break
			}
		}
		fmt.Printf("  %s (id=%d): %s\n", classCode, b.ClassificationID, b.Balance)
	}

	// ---------------------------------------------------------------
	// 11. Run reconciliation check
	// ---------------------------------------------------------------
	fmt.Println("\n--- Reconciliation ---")

	// Use the reconciliation service
	reconcileSvc := newSimpleReconciler(pool, classStore, engine)
	result, err := reconcileSvc.CheckAccountingEquation(ctx)
	if err != nil {
		log.Fatalf("reconcile: %v", err)
	}
	fmt.Printf("Accounting equation balanced: %v (gap: %s)\n", result.Balanced, result.Gap)

	fmt.Println("\nDone.")
}

// simpleReconciler wraps the global debit/credit sum for the example.
// In production, use service.ReconciliationService with proper store injection.
type simpleReconciler struct {
	pool        *pgxpool.Pool
	classStore  core.ClassificationStore
	engine      *core.Engine
}

func newSimpleReconciler(pool *pgxpool.Pool, cs core.ClassificationStore, e *core.Engine) *simpleReconciler {
	return &simpleReconciler{pool: pool, classStore: cs, engine: e}
}

func (r *simpleReconciler) CheckAccountingEquation(ctx context.Context) (*core.ReconcileResult, error) {
	var debitTotal, creditTotal decimal.Decimal

	rows, err := r.pool.Query(ctx, `
		SELECT entry_type, COALESCE(SUM(amount), 0) AS total
		FROM journal_entries
		GROUP BY entry_type
	`)
	if err != nil {
		return nil, fmt.Errorf("reconcile: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entryType string
		var total decimal.Decimal
		if err := rows.Scan(&entryType, &total); err != nil {
			return nil, fmt.Errorf("reconcile: scan: %w", err)
		}
		switch core.EntryType(entryType) {
		case core.EntryTypeDebit:
			debitTotal = total
		case core.EntryTypeCredit:
			creditTotal = total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile: rows: %w", err)
	}

	gap := debitTotal.Sub(creditTotal)
	return &core.ReconcileResult{
		Balanced:  gap.IsZero(),
		Gap:       gap,
		CheckedAt: time.Now(),
	}, nil
}
