package ledger_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestExchange_RejectsNonAvailableMovement(t *testing.T) {
	type movement struct {
		class string
		units int
	}
	for _, leg := range []string{"sell", "buy"} {
		for _, tc := range []struct {
			name  string
			moves []movement
		}{
			{"pending_only", []movement{{"pending", 1}}},
			{"locked_only", []movement{{"locked", 1}}},
			{"available_excess_offset_by_pending", []movement{{"available", 2}, {"pending", -1}}},
			{"pending_locked_offset", []movement{{"available", 1}, {"pending", 1}, {"locked", -1}}},
			{"pending_classifications_offset", []movement{{"available", 1}, {"pending", 1}, {"pending_other", -1}}},
			{"memo_extra", []movement{{"available", 1}, {"memo", 1}}},
		} {
			t.Run(leg+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				f := seedExchangeFixture(t, ctx)
				classes := make(map[string]core.Classification)
				wallet, err := f.svc.Classifications().GetByCode(ctx, "main_wallet")
				require.NoError(t, err)
				classes["available"] = *wallet
				for _, class := range []struct {
					name string
					role core.BalanceRole
					side core.NormalSide
				}{
					{"pending", core.BalanceRolePending, core.NormalSideCredit},
					{"pending_other", core.BalanceRolePending, core.NormalSideDebit},
					{"locked", core.BalanceRoleLocked, core.NormalSideDebit},
					{"memo", core.BalanceRoleMemo, core.NormalSideDebit},
				} {
					created, err := f.svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
						Code: "exchange_" + class.name, Name: class.name, BalanceRole: class.role, NormalSide: class.side,
					})
					require.NoError(t, err)
					classes[class.name] = *created
				}
				settlement, err := f.svc.Classifications().GetByCode(ctx, "settlement")
				require.NoError(t, err)
				journalType, err := f.svc.JournalTypes().GetJournalTypeByCode(ctx, "fx_"+leg)
				require.NoError(t, err)
				var lines []core.TemplateLineInput
				for _, move := range tc.moves {
					units := move.units
					if leg == "sell" {
						units = -units
					}
					class := classes[move.class]
					side := core.EntryType(class.NormalSide)
					if units < 0 {
						units = -units
						side = oppositeExchangeEntry(side)
					}
					for range units {
						// Pair every user entry with the system counterpart so the
						// misconfigured template is still perfectly double-entry.
						lines = append(lines,
							core.TemplateLineInput{ClassificationUID: class.UID, EntryType: side, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: len(lines) + 1},
							core.TemplateLineInput{ClassificationUID: settlement.UID, EntryType: oppositeExchangeEntry(side), HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: len(lines) + 2},
						)
					}
				}
				const template = "misconfigured_exchange"
				_, err = f.svc.Templates().CreateTemplate(ctx, core.TemplateInput{Code: template, Name: template, JournalTypeUID: journalType.UID, Lines: lines})
				require.NoError(t, err)
				input := f.input("nonavailable")
				if leg == "sell" {
					input.SellTemplateCode = template
				} else {
					input.BuyTemplateCode = template
				}

				rows := func() [6]int64 {
					var counts [6]int64
					require.NoError(t, f.pool.QueryRow(ctx, `SELECT
						(SELECT count(*) FROM journals), (SELECT count(*) FROM journal_entries),
						(SELECT count(*) FROM reservations), (SELECT count(*) FROM reservation_operation_receipts),
						(SELECT count(*) FROM balance_checkpoints), (SELECT count(*) FROM rollup_queue)`).Scan(
						&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5]))
					return counts
				}
				before := rows()
				result, err := f.svc.Exchange(ctx, input)
				require.ErrorIs(t, err, core.ErrInvalidInput, "a balanced journal with the right total net can still move the wrong liquidity")
				require.Nil(t, result)
				f.requireNothingWritten(t, ctx)
				require.Equal(t, before, rows(), "both journals, hold, settlement receipt and rollup writes must roll back")
				for name, class := range classes {
					if name == "available" {
						continue
					}
					for _, currency := range []string{f.usdc, f.credits} {
						balance, err := f.svc.BalanceReader().GetBalance(ctx, f.holder, currency, class.UID)
						require.NoError(t, err)
						require.True(t, balance.IsZero(), "refused exchange must leave %s unchanged", name)
					}
				}

				// Refusal does not consume any derived idempotency key. A retry
				// with the standard templates can perform the intended exchange.
				input.SellTemplateCode, input.BuyTemplateCode = "", ""
				_, err = f.svc.Exchange(ctx, input)
				require.NoError(t, err)
				require.True(t, f.balance(t, ctx, f.usdc).IsZero())
				require.True(t, f.balance(t, ctx, f.credits).Equal(decimal.NewFromInt(1000)))
			})
		}
	}
}

func oppositeExchangeEntry(side core.EntryType) core.EntryType {
	if side == core.EntryTypeDebit {
		return core.EntryTypeCredit
	}
	return core.EntryTypeDebit
}

func TestExchange_AllowsZeroNetWithinNonAvailableDimension(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	pending, err := f.svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "exchange_net_zero_pending", Name: "Pending", BalanceRole: core.BalanceRolePending, NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)
	settlement, err := f.svc.Classifications().GetByCode(ctx, "settlement")
	require.NoError(t, err)
	for _, leg := range []string{"sell", "buy"} {
		jt, err := f.svc.JournalTypes().GetJournalTypeByCode(ctx, "fx_"+leg)
		require.NoError(t, err)
		walletSide := core.EntryTypeCredit
		if leg == "buy" {
			walletSide = core.EntryTypeDebit
		}
		_, err = f.svc.Templates().CreateTemplate(ctx, core.TemplateInput{
			Code: "net_zero_" + leg, Name: "net zero " + leg, JournalTypeUID: jt.UID,
			Lines: []core.TemplateLineInput{
				{ClassificationUID: f.walletClass, EntryType: walletSide, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
				{ClassificationUID: settlement.UID, EntryType: oppositeExchangeEntry(walletSide), HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
				{ClassificationUID: pending.UID, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 3},
				{ClassificationUID: pending.UID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 4},
			},
		})
		require.NoError(t, err)
	}
	input := f.input("same-dimension-net-zero")
	input.SellTemplateCode, input.BuyTemplateCode = "net_zero_sell", "net_zero_buy"
	first, err := f.svc.Exchange(ctx, input)
	require.NoError(t, err)
	replayed, err := f.svc.Exchange(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	require.Equal(t, 3, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).IsZero())
	require.True(t, f.balance(t, ctx, f.credits).Equal(decimal.NewFromInt(1000)))
	for _, currency := range []string{f.usdc, f.credits} {
		balance, err := f.svc.BalanceReader().GetBalance(ctx, f.holder, currency, pending.UID)
		require.NoError(t, err)
		require.True(t, balance.IsZero(), "each nonavailable dimension must remain unchanged")
	}
}
