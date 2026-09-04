package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit pins a Major from
// the 2026-08-25 financial-engineering audit
// (postgres/sql/queries/platform_balances.sql:60-91, GetTotalUserSideBalance;
// financial-correctness.md): SolvencyCheck's Liability figure summed EVERY
// user-side classification, including role-less debit-normal cost accounts
// like fee_expense. fee_expense is booked to the user's own holder id purely
// for per-user fee reporting -- it is not money the platform owes back. Under
// the old query, every dollar of cumulative withdrawal fees showed up as a
// dollar of manufactured insolvency, even though the platform was fully
// solvent (custodial == every reservable user balance).
//
// This replays exactly the flow postgres/presets_install_test.go's
// TestInstallDefaultTemplatePresets already asserts the balances for
// (deposit 500 -> lock 105 -> withdraw_fee 5 -> withdraw_confirm 100, leaving
// main_wallet=395, locked=0, fee_expense=5, custodial=395), so the "before"
// math is independently checkable against that test's own assertions:
//
//	Liability (buggy) = main_wallet(395) + locked(0) + fee_expense(5) = 400
//	Custodial          = 395
//	Margin  (buggy)    = 395 - 400 = -5  =>  Solvent = false
//
// After the fix, Liability excludes fee_expense (BalanceRole == "" / none):
//
//	Liability (fixed)  = main_wallet(395) + locked(0) = 395
//	Margin  (fixed)    = 395 - 395 = 0  =>  Solvent = true
func TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	curID := postgrestest.SeedCurrency(t, pool, "USDT-SOLV", "Tether USD Solvency")
	userID := int64(9101)

	_, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-deposit"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(500)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-lock"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(105)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_fee", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-fee"),
		Amounts:        map[string]decimal.Decimal{"fee": decimal.NewFromInt(5)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-confirm"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	report, err := pbStore.SolvencyCheck(ctx, curID)
	require.NoError(t, err)

	assert.True(t, report.Custodial.Equal(decimal.NewFromInt(395)), "custodial: got %s", report.Custodial)
	assert.True(t, report.Liability.Equal(decimal.NewFromInt(395)), "liability must exclude fee_expense: got %s", report.Liability)
	assert.True(t, report.Margin.IsZero(), "margin: got %s", report.Margin)
	assert.True(t, report.Solvent, "platform must report solvent -- custodial covers every reservable user balance")

	liability, err := pbStore.GetTotalLiabilityByAsset(ctx, curID)
	require.NoError(t, err)
	assert.True(t, liability.Equal(decimal.NewFromInt(395)), "GetTotalLiabilityByAsset must agree with SolvencyCheck: got %s", liability)
}

// --- Per-preset solvency pins (2026-09-02 audit, A-C1 / A-M2 / A-M4 / A-M6) ---
//
// The 2026-09-02 audit found three shipped presets posting the wrong sign
// against a system counterparty, and traced all three to the same blind spot:
// postgres/solvency_test.go had exactly ONE case and it only covered the
// withdrawal path, so capital_*, checkout_settlement_*, fee_charge and fx_*
// had no solvency assertion at all. TestPresetSolvency_EveryShippedTemplate
// closes that blind spot structurally: every template this repository ships
// gets one row, and the row states the exact expected effect on the solvency
// report rather than "it balances" (presets.assertBalanced is true by
// construction for two entries sharing one amount key and can never catch a
// direction error).
//
// The sign rule every row is derived from -- the ledger's only sign
// authority, core.Sign / ledger_signed_amount (docs/INVARIANTS.md I-43):
//
//	signed(entry) = +amount when entry_type == classification.normal_side
//	              -amount otherwise
//
// A journal must satisfy sum(DR amounts) == sum(CR amounts). Two accounts
// that both INCREASE in the same journal must therefore carry OPPOSITE
// normal_sides, and two accounts where one increases while the other
// decreases must carry the SAME normal_side. That constraint -- not standard
// accounting's "debit an asset to increase it" -- is what fixes every entry
// direction in presets/.
//
// Solvency scope (see postgres/sql/queries/platform_balances.sql):
//
//	Liability = user-side classifications with balance_role in
//	            (available, pending, locked)
//	Custodial = system-side classifications whose code is in the configured
//	            custodial set, default {custodial, settlement}
//	Margin    = Custodial - Liability
//
// Two rows below deliberately pin a NEGATIVE margin. They are not defects:
//
//   - deposit_pending shows the holder a pending balance before the funds
//     are confirmed into custody, so the platform genuinely is short by that
//     amount until deposit_confirm_pending lands. The pair nets to zero.
//   - dev_credit mints holder balance with no custodied asset behind it; the
//     shortfall equalling the dev_credit balance is the entire point of the
//     preset (presets/devcredit.go, TestDevCredit_SolvencyShortfallEqualsDevCreditBalance).

type solvencyStep struct {
	template string
	holder   int64
	amounts  map[string]string
}

type solvencyCase struct {
	name string
	// why states the accounting reading each expectation is derived from.
	why       string
	steps     []solvencyStep
	custodial string
	liability string
	margin    string
	solvent   bool
}

func TestPresetSolvency_EveryShippedTemplate(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	require.NoError(t, svc.InstallExtendedPresets(ctx))
	require.NoError(t, svc.InstallDevCreditPreset(ctx))

	const user = int64(7101)

	cases := []solvencyCase{
		{
			name: "deposit_pending",
			why:  "pending is a user liability (balance_role=pending); suspense is not in the custodial set, so an unconfirmed deposit is honestly reported as unbacked",
			steps: []solvencyStep{
				{"deposit_pending", user, map[string]string{"amount": "100"}},
			},
			custodial: "0", liability: "100", margin: "-100", solvent: false,
		},
		{
			name: "deposit_pending + deposit_confirm_pending",
			why:  "confirming moves the liability from pending to main_wallet and the asset from suspense to custodial; the pair nets to a fully backed position",
			steps: []solvencyStep{
				{"deposit_pending", user, map[string]string{"amount": "100"}},
				{"deposit_confirm_pending", user, map[string]string{"amount": "100"}},
			},
			custodial: "100", liability: "100", margin: "0", solvent: true,
		},
		{
			name: "deposit_pending + deposit_release_pending",
			why:  "releasing an unconfirmed deposit removes the liability again; nothing ever entered custody",
			steps: []solvencyStep{
				{"deposit_pending", user, map[string]string{"amount": "100"}},
				{"deposit_release_pending", user, map[string]string{"amount": "100"}},
			},
			custodial: "0", liability: "0", margin: "0", solvent: true,
		},
		{
			name: "deposit_confirm",
			why:  "custodial is credit-normal so CR increases it; main_wallet is debit-normal so DR increases it -- both sides of one arriving deposit",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
			},
			custodial: "1000", liability: "1000", margin: "0", solvent: true,
		},
		{
			name: "deposit_record_overage",
			why:  "an unexpected surplus lands in custody with nobody credited yet, so the platform is over-collateralised by exactly the overage",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"deposit_record_overage", user, map[string]string{"amount": "20"}},
			},
			custodial: "1020", liability: "1000", margin: "20", solvent: true,
		},
		{
			name: "deposit_record_overage + deposit_resolve_overage",
			why:  "resolving credits the overage to the holder, closing the surplus",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"deposit_record_overage", user, map[string]string{"amount": "20"}},
				{"deposit_resolve_overage", user, map[string]string{"amount": "20"}},
			},
			custodial: "1020", liability: "1020", margin: "0", solvent: true,
		},
		{
			name: "deposit_record_overage + deposit_release_overage",
			why:  "releasing returns the overage to its sender: custody drops back, no holder was ever credited",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"deposit_record_overage", user, map[string]string{"amount": "20"}},
				{"deposit_release_overage", user, map[string]string{"amount": "20"}},
			},
			custodial: "1000", liability: "1000", margin: "0", solvent: true,
		},
		{
			name: "lock_funds",
			why:  "locking moves a liability between two role-bearing buckets (available -> locked); the total owed is unchanged",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"lock_funds", user, map[string]string{"amount": "105"}},
			},
			custodial: "1000", liability: "1000", margin: "0", solvent: true,
		},
		{
			name: "unlock_funds",
			why:  "unlocking is lock_funds reversed; still liability-neutral",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"lock_funds", user, map[string]string{"amount": "105"}},
				{"unlock_funds", user, map[string]string{"amount": "105"}},
			},
			custodial: "1000", liability: "1000", margin: "0", solvent: true,
		},
		{
			name: "withdraw_fee + withdraw_confirm",
			why:  "the reference four-entry shape: the fee leaves the custodial pool into fee_revenue while the holder's locked claim drops by the same amount, and the payout removes asset and liability together",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "500"}},
				{"lock_funds", user, map[string]string{"amount": "105"}},
				{"withdraw_fee", user, map[string]string{"fee": "5"}},
				{"withdraw_confirm", user, map[string]string{"amount": "100"}},
			},
			custodial: "395", liability: "395", margin: "0", solvent: true,
		},
		{
			name: "transfer_out + transfer_in",
			why:  "settlement is the transit asset: the sender's journal removes asset and liability together, the receiver's journal adds them back",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"transfer_out", user, map[string]string{"amount": "40"}},
				{"transfer_in", user + 1, map[string]string{"amount": "40"}},
			},
			custodial: "1000", liability: "1000", margin: "0", solvent: true,
		},
		{
			name: "fee_charge",
			why:  "A-M4: fees is credit-normal, so revenue increases on CR; the fee leaves the custodial pool exactly as withdraw_fee's does, keeping the platform exactly backed",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "1000"}},
				{"fee_charge", user, map[string]string{"amount": "30"}},
			},
			custodial: "970", liability: "970", margin: "0", solvent: true,
		},
		{
			name: "checkout_settlement_gross",
			why:  "A-M2: the merchant RECEIVES money, so main_wallet must be debited (+gross) and custodial credited (+gross); the journal type declares HolderTxKindDeposit and the ledger must agree with it",
			steps: []solvencyStep{
				{"checkout_settlement_gross", user, map[string]string{"gross_amount": "100"}},
			},
			custodial: "100", liability: "100", margin: "0", solvent: true,
		},
		{
			name: "checkout_settlement_net",
			why:  "A-M2: gross arrives and splits into the merchant's claim (custodial, +net) and the platform's own earnings (fees, +fee); the custodial pool holds exactly what is owed, so margin stays zero",
			steps: []solvencyStep{
				{"checkout_settlement_net", user, map[string]string{"net_amount": "97", "fee_amount": "3"}},
			},
			custodial: "97", liability: "97", margin: "0", solvent: true,
		},
		{
			name: "capital_injection",
			why:  "A-C1: injecting platform capital must RAISE custody with no new liability -- it is the only action that can improve the solvency margin",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "500"}},
				{"capital_injection", user, map[string]string{"amount": "1000"}},
			},
			custodial: "1500", liability: "500", margin: "1000", solvent: true,
		},
		{
			name: "capital_injection + capital_withdraw",
			why:  "A-C1: withdrawing platform capital is the exact inverse; taking the buffer back out returns the margin to zero",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "500"}},
				{"capital_injection", user, map[string]string{"amount": "1000"}},
				{"capital_withdraw", user, map[string]string{"amount": "1000"}},
			},
			custodial: "500", liability: "500", margin: "0", solvent: true,
		},
		{
			name: "fx_sell (currency sold)",
			why:  "A-M6: the platform absorbs the sold currency into settlement, so the custodial set (custodial + settlement) nets to the zero it owes",
			steps: []solvencyStep{
				{"deposit_confirm", user, map[string]string{"amount": "100"}},
				{"fx_sell", user, map[string]string{"amount": "100"}},
			},
			custodial: "0", liability: "0", margin: "0", solvent: true,
		},
		{
			name: "fx_buy (currency bought)",
			why:  "A-M6: the bought currency is released from the settlement pool; counting settlement as custodial is what stops a healthy FX position reporting permanent insolvency",
			steps: []solvencyStep{
				{"fx_buy", user, map[string]string{"amount": "90"}},
			},
			custodial: "90", liability: "90", margin: "0", solvent: true,
		},
		{
			name: "dev_credit",
			why:  "unbacked by construction: the shortfall must equal the dev_credit balance (presets/devcredit.go)",
			steps: []solvencyStep{
				{"dev_credit", user, map[string]string{"amount": "250"}},
			},
			custodial: "0", liability: "250", margin: "-250", solvent: false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One currency per case: solvency is per-currency, so distinct
			// currencies isolate the cases without a fresh container each.
			curUID := postgrestest.SeedCurrency(t,
				pool,
				fmt.Sprintf("SOLV%02d", i),
				fmt.Sprintf("Solvency fixture %02d", i),
			)

			for j, st := range tc.steps {
				amounts := make(map[string]decimal.Decimal, len(st.amounts))
				for k, v := range st.amounts {
					amounts[k] = decimal.RequireFromString(v)
				}
				_, err := svc.JournalWriter().ExecuteTemplate(ctx, st.template, core.TemplateParams{
					HolderID:       st.holder,
					CurrencyUID:    curUID,
					IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("solv-%d-%d-%s", i, j, st.template)),
					Amounts:        amounts,
					Source:         "solvency_test",
				})
				require.NoErrorf(t, err, "step %d (%s)", j, st.template)
			}

			report, err := svc.SolvencyChecker().SolvencyCheck(ctx, curUID)
			require.NoError(t, err)

			assert.Truef(t, report.Custodial.Equal(decimal.RequireFromString(tc.custodial)),
				"custodial: want %s got %s (%s)", tc.custodial, report.Custodial, tc.why)
			assert.Truef(t, report.Liability.Equal(decimal.RequireFromString(tc.liability)),
				"liability: want %s got %s (%s)", tc.liability, report.Liability, tc.why)
			assert.Truef(t, report.Margin.Equal(decimal.RequireFromString(tc.margin)),
				"margin: want %s got %s (%s)", tc.margin, report.Margin, tc.why)
			assert.Equalf(t, tc.solvent, report.Solvent,
				"solvent: want %t (%s)", tc.solvent, tc.why)
		})
	}
}

// TestSolvencyCheck_CustodialScopeMustMatchSomething pins the fail-loud half of
// A-N3: the custodial scope used to be the string literal 'custodial' inside
// GetSystemSideCustodialBalance, so a deployment that named its custody
// classification anything else got Custodial=0 and permanent insolvency with
// no error and no hint that a naming assumption existed at all.
func TestSolvencyCheck_CustodialScopeMustMatchSomething(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.InstallDefaultPresets(ctx))
	curUID := postgrestest.SeedCurrency(t, pool, "SOLVSCOPE", "Solvency scope fixture")

	// A store scoped to a classification code nobody installed must say so
	// rather than reporting a zero custody position.
	scoped := postgres.NewPlatformBalanceStore(pool).WithCustodialClassCodes("reserve")
	_, err = scoped.SolvencyCheck(ctx, curUID)
	require.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "reserve")

	// The default scope resolves, so the same call succeeds.
	_, err = svc.SolvencyChecker().SolvencyCheck(ctx, curUID)
	require.NoError(t, err)
}
