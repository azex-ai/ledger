package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// A holder-wide freeze must judge each currency separately even when the
// journal is balanced in every currency and the numeric sum is nonnegative.
func TestLedgerStore_Frozen_CrossCurrencyCannotOffsetDecrease(t *testing.T) {
	for _, tc := range []struct {
		name         string
		points       int64
		depositFirst bool
	}{
		{name: "equal_numeric_amount", points: 100},
		{name: "greater_numeric_amount", points: 1000},
		{name: "equal_numeric_amount_deposit_first", points: 100, depositFirst: true},
		{name: "greater_numeric_amount_deposit_first", points: 1000, depositFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := postgrestest.SetupDB(t)
			ctx := context.Background()
			ls := postgres.NewLedgerStore(p)
			policies := postgres.NewAccountPolicyStore(p)
			usd := postgrestest.SeedCurrencyWithExponent(t, p, "USD", "Dollar", 2)
			points := postgrestest.SeedCurrencyWithExponent(t, p, "PTS", "Points", 0)
			wallet := postgrestest.SeedClassificationWithRole(t, p, "frozen_wallet", "Wallet", "debit", false, "available")
			counter := postgrestest.SeedClassification(t, p, "frozen_counter", "Counter", "credit", true)
			journalType := postgrestest.SeedJournalType(t, p, "frozen", "Frozen currency test")
			const holder int64 = 42
			entry := func(holder int64, currency, class string, side core.EntryType, amount int64) core.EntryInput {
				return core.EntryInput{AccountHolder: holder, CurrencyUID: currency, ClassificationUID: class, EntryType: side, Amount: decimal.NewFromInt(amount)}
			}
			_, err := ls.PostJournal(ctx, core.JournalInput{
				JournalTypeUID: journalType,
				IdempotencyKey: "seed",
				Entries: []core.EntryInput{
					entry(holder, usd, wallet, core.EntryTypeDebit, 100),
					entry(-holder, usd, counter, core.EntryTypeCredit, 100),
				},
			})
			require.NoError(t, err)
			_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, Status: core.AccountPolicyStatusFrozen})
			require.NoError(t, err)

			// Count durable accounting rows before either rejected attempt. This
			// also covers rollup/checkpoint writes, not just the visible balance.
			accountingRows := func() [4]int64 {
				var counts [4]int64
				require.NoError(t, p.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM journals),
					(SELECT count(*) FROM journal_entries),
					(SELECT count(*) FROM balance_checkpoints),
					(SELECT count(*) FROM rollup_queue)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]))
				return counts
			}
			before := accountingRows()
			usdSpend := []core.EntryInput{
				entry(holder, usd, wallet, core.EntryTypeCredit, 100),
				entry(-holder, usd, counter, core.EntryTypeDebit, 100),
			}
			_, err = ls.PostJournal(ctx, core.JournalInput{
				JournalTypeUID: journalType, IdempotencyKey: "usd-only", Entries: usdSpend,
			})
			require.ErrorIs(t, err, core.ErrAccountFrozen, "control: spending USD alone is rejected")
			require.Equal(t, before, accountingRows(), "rejected USD spending must not write accounting rows")

			pointsDeposit := []core.EntryInput{
				entry(holder, points, wallet, core.EntryTypeDebit, tc.points),
				entry(-holder, points, counter, core.EntryTypeCredit, tc.points),
			}
			entries := append(usdSpend, pointsDeposit...)
			if tc.depositFirst {
				entries = append(pointsDeposit, usdSpend...)
			}
			input := core.JournalInput{
				JournalTypeUID: journalType, IdempotencyKey: "cross-currency", Entries: entries,
			}
			_, err = ls.PostJournal(ctx, input)
			require.ErrorIs(t, err, core.ErrAccountFrozen, "PTS cannot offset a USD decrease under the same frozen policy")
			require.Equal(t, before, accountingRows(), "rejected multi-currency journal must not write accounting rows")

			assertBalance := func(accountHolder int64, currency, classification string, want int64) {
				balance, err := ls.GetBalance(ctx, accountHolder, currency, classification)
				require.NoError(t, err)
				require.True(t, balance.Equal(decimal.NewFromInt(want)), "holder %d balance = %s, want %d", accountHolder, balance, want)
			}
			assertBalance(holder, usd, wallet, 100)
			assertBalance(-holder, usd, counter, 100)
			assertBalance(holder, points, wallet, 0)
			assertBalance(-holder, points, counter, 0)

			// The same balanced journal is valid after unfreezing; a rejected
			// attempt must not consume its idempotency key or either currency leg.
			_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, Status: core.AccountPolicyStatusActive})
			require.NoError(t, err)
			_, err = ls.PostJournal(ctx, input)
			require.NoError(t, err)
			assertBalance(holder, usd, wallet, 0)
			assertBalance(-holder, usd, counter, 0)
			assertBalance(holder, points, wallet, tc.points)
			assertBalance(-holder, points, counter, tc.points)
		})
	}
}
