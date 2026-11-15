package ledger

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// Templates currently render one currency and one holder/counterpart pair.
// Pin the leg's own scope check too, so future template extensions cannot
// silently broaden which balances Exchange is allowed to change.
func TestExchangeHolderLegNet_RequiresAvailableAndExpectedScope(t *testing.T) {
	classes := map[string]core.Classification{
		"wallet":        {BalanceRole: core.BalanceRoleAvailable, NormalSide: core.NormalSideDebit},
		"credit_wallet": {BalanceRole: core.BalanceRoleAvailable, NormalSide: core.NormalSideCredit},
		"pending":       {BalanceRole: core.BalanceRolePending, NormalSide: core.NormalSideCredit},
		"pending_other": {BalanceRole: core.BalanceRolePending, NormalSide: core.NormalSideCredit},
		"memo":          {BalanceRole: core.BalanceRoleMemo, NormalSide: core.NormalSideDebit},
		"none":          {BalanceRole: core.BalanceRoleNone, NormalSide: core.NormalSideDebit},
	}
	entry := func(holder int64, currency, class string, side core.EntryType, amount int64) core.Entry {
		return core.Entry{AccountHolder: holder, CurrencyUID: currency, ClassificationUID: class, EntryType: side, Amount: decimal.NewFromInt(amount)}
	}
	sell := entry(42, "source", "wallet", core.EntryTypeCredit, 1)
	for _, tc := range []struct {
		name       string
		entries    []core.Entry
		wantNet    int64
		unexpected bool
		wantErr    error
	}{
		{"debit_normal_available", []core.Entry{sell}, -1, false, nil},
		{"credit_normal_available", []core.Entry{entry(42, "source", "credit_wallet", core.EntryTypeCredit, 1)}, 1, false, nil},
		{"mixed_available_normal_sides", []core.Entry{
			entry(42, "source", "wallet", core.EntryTypeCredit, 2), entry(42, "source", "credit_wallet", core.EntryTypeCredit, 1),
		}, -1, false, nil},
		{"same_pending_dimension_nets_zero", []core.Entry{
			sell, entry(42, "source", "pending", core.EntryTypeCredit, 1), entry(42, "source", "pending", core.EntryTypeDebit, 1),
		}, -1, false, nil},
		{"pending_dimensions_cannot_offset", []core.Entry{
			sell, entry(42, "source", "pending", core.EntryTypeCredit, 1), entry(42, "source", "pending_other", core.EntryTypeDebit, 1),
		}, 0, true, nil},
		{"memo_change", []core.Entry{sell, entry(42, "source", "memo", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"none_change", []core.Entry{sell, entry(42, "source", "none", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"other_user", []core.Entry{sell, entry(43, "source", "wallet", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"other_leg_currency", []core.Entry{sell, entry(42, "target", "wallet", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"third_currency", []core.Entry{sell, entry(42, "third", "wallet", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"system_other_currency", []core.Entry{sell, entry(-42, "third", "none", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"other_counterpart", []core.Entry{sell, entry(-43, "source", "none", core.EntryTypeDebit, 1)}, 0, true, nil},
		{"expected_counterpart", []core.Entry{sell, entry(-42, "source", "none", core.EntryTypeDebit, 1)}, -1, false, nil},
		{"unknown_user_classification", []core.Entry{entry(42, "source", "unknown", core.EntryTypeCredit, 1)}, 0, false, core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net, unexpected, err := holderLegNet(tc.entries, 42, "source", classes)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.unexpected, unexpected)
			if !tc.unexpected {
				require.True(t, net.Equal(decimal.NewFromInt(tc.wantNet)), "net=%s want=%d", net, tc.wantNet)
			}
		})
	}
}
