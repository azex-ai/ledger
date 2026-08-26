package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountPolicyStatus_IsValid(t *testing.T) {
	assert.True(t, AccountPolicyStatusActive.IsValid())
	assert.True(t, AccountPolicyStatusFrozen.IsValid())
	assert.True(t, AccountPolicyStatusClosed.IsValid())
	assert.False(t, AccountPolicyStatus("bogus").IsValid())
	assert.False(t, AccountPolicyStatus("").IsValid())
}

func TestAccountPolicyInput_Validate(t *testing.T) {
	valid := AccountPolicyInput{
		AccountHolder: 7,
		CurrencyUID:   "cur-1",
		Status:        AccountPolicyStatusFrozen,
	}
	require.NoError(t, valid.Validate())

	// Wildcard tiers (currency_uid == "", classification_uid == "") are valid.
	require.NoError(t, AccountPolicyInput{AccountHolder: 7, Status: AccountPolicyStatusActive}.Validate())

	cases := []struct {
		name  string
		input AccountPolicyInput
	}{
		{
			name:  "zero holder",
			input: AccountPolicyInput{Status: AccountPolicyStatusActive},
		},
		{
			name:  "classification without currency",
			input: AccountPolicyInput{AccountHolder: 7, ClassificationUID: "cls-1", Status: AccountPolicyStatusActive},
		},
		{
			name:  "invalid status",
			input: AccountPolicyInput{AccountHolder: 7, Status: AccountPolicyStatus("bogus")},
		},
		{
			name: "note too long",
			input: AccountPolicyInput{
				AccountHolder: 7,
				Status:        AccountPolicyStatusActive,
				Note:          string(make([]byte, accountPolicyNoteMaxLen+1)),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.input.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestAccountPolicyInput_Validate_NegativeMinBalanceAllowed(t *testing.T) {
	// Negative min_balance (overdraft/credit limit) is a valid, deliberate
	// configuration — must not be rejected.
	input := AccountPolicyInput{
		AccountHolder:     7,
		Status:            AccountPolicyStatusActive,
		MinBalance:        decimal.NewFromInt(-100),
		EnforceMinBalance: true,
	}
	require.NoError(t, input.Validate())
}

func TestEntryDirection(t *testing.T) {
	cases := []struct {
		name       string
		entryType  EntryType
		normalSide NormalSide
		want       BalanceDirection
	}{
		{"debit on debit-normal increases", EntryTypeDebit, NormalSideDebit, BalanceDirectionIncrease},
		{"credit on debit-normal decreases", EntryTypeCredit, NormalSideDebit, BalanceDirectionDecrease},
		{"credit on credit-normal increases", EntryTypeCredit, NormalSideCredit, BalanceDirectionIncrease},
		{"debit on credit-normal decreases", EntryTypeDebit, NormalSideCredit, BalanceDirectionDecrease},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EntryDirection(tc.entryType, tc.normalSide)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSign_RejectsUnknownNormalSideAndEntryType is the I-43 pin: Sign is the
// sole authority for interpreting normal_side, and a value outside
// {debit, credit} — the "third value" the audit's failure scenario
// describes (docs/audits/2026-08-25-financial-engineering/financial-correctness.md,
// "同一个符号语义有 17 处独立实现") — must be refused, not defaulted to
// debit-normal (as postgres/ledger_store.go and postgres/trial_balance_store.go
// used to) and not silently excluded from the balance (as checkpoints.sql's
// ListComputedBalancesForHolders `ELSE 0` used to).
func TestSign_RejectsUnknownNormalSideAndEntryType(t *testing.T) {
	_, err := Sign(NormalSide("frozen"), EntryTypeDebit)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = Sign(NormalSideDebit, EntryType("hold"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = Sign(NormalSide(""), EntryType(""))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

// TestSign_Table pins the four valid (normalSide, entryType) combinations
// against the sign every wrapper (SignedAmount, Delta, EntryDirection) must
// agree with.
func TestSign_Table(t *testing.T) {
	cases := []struct {
		name       string
		normalSide NormalSide
		entryType  EntryType
		want       int
	}{
		{"debit-normal, debit entry increases", NormalSideDebit, EntryTypeDebit, 1},
		{"debit-normal, credit entry decreases", NormalSideDebit, EntryTypeCredit, -1},
		{"credit-normal, credit entry increases", NormalSideCredit, EntryTypeCredit, 1},
		{"credit-normal, debit entry decreases", NormalSideCredit, EntryTypeDebit, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sign(tc.normalSide, tc.entryType)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSignedAmount_RejectsUnknownNormalSide pins that SignedAmount — used
// directly on postgres/account_policy_enforce.go's hot path (I-17
// enforcement, one call per journal entry) — refuses rather than defaults,
// same as Sign.
func TestSignedAmount_RejectsUnknownNormalSide(t *testing.T) {
	_, err := SignedAmount(NormalSide("bogus"), EntryTypeDebit, decimal.NewFromInt(100))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestSignedAmount_Table(t *testing.T) {
	amt := decimal.NewFromInt(100)
	got, err := SignedAmount(NormalSideDebit, EntryTypeDebit, amt)
	require.NoError(t, err)
	assert.True(t, got.Equal(amt))

	got, err = SignedAmount(NormalSideDebit, EntryTypeCredit, amt)
	require.NoError(t, err)
	assert.True(t, got.Equal(amt.Neg()))
}

// TestDelta_RejectsUnknownNormalSide is the I-43 pin for the aggregate
// (debitSum, creditSum) -> net shape used by service/rollup.go,
// service/reconcile.go (x3), postgres/ledger_store.go's GetBalance, and
// postgres/trial_balance_store.go / postgres/reconcile_queries.go. Before
// this collapse, this exact shape had four different fates for an unknown
// normal_side across those files: service/rollup.go and the switch-based
// site in service/reconcile.go's ReconcileAccount errored (matching this
// test); postgres/ledger_store.go's getBalanceWithQueries silently defaulted
// to debit-normal; postgres/trial_balance_store.go's TrialBalance and
// postgres/reconcile_queries.go's NegativeBalanceAccounts silently defaulted
// to credit-normal (any non-"debit" string fell into the else branch). All
// six now agree — because all six now call this function.
func TestDelta_RejectsUnknownNormalSide(t *testing.T) {
	_, err := Delta(NormalSide("unknown"), decimal.NewFromInt(500), decimal.NewFromInt(200))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestDelta_Table(t *testing.T) {
	debit := decimal.NewFromInt(500)
	credit := decimal.NewFromInt(200)

	got, err := Delta(NormalSideDebit, debit, credit)
	require.NoError(t, err)
	assert.True(t, got.Equal(decimal.NewFromInt(300)))

	got, err = Delta(NormalSideCredit, debit, credit)
	require.NoError(t, err)
	assert.True(t, got.Equal(decimal.NewFromInt(-300)))
}

// TestDelta_AgreesWithSignedAmount pins the linearity Delta's doc comment
// claims: Delta(ns, debitSum, creditSum) == SignedAmount(ns, debit,
// debitSum) + SignedAmount(ns, credit, creditSum), for every valid
// normal_side. If a future edit makes Delta diverge from Sign/SignedAmount,
// this fails — Sign stops being the sole authority the moment any wrapper
// can disagree with it.
func TestDelta_AgreesWithSignedAmount(t *testing.T) {
	debit := decimal.NewFromInt(837)
	credit := decimal.NewFromInt(419)

	for _, ns := range []NormalSide{NormalSideDebit, NormalSideCredit} {
		debitPart, err := SignedAmount(ns, EntryTypeDebit, debit)
		require.NoError(t, err)
		creditPart, err := SignedAmount(ns, EntryTypeCredit, credit)
		require.NoError(t, err)
		want := debitPart.Add(creditPart)

		got, err := Delta(ns, debit, credit)
		require.NoError(t, err)
		assert.True(t, got.Equal(want), "Delta(%s, %s, %s) = %s, want %s", ns, debit, credit, got, want)
	}
}

// TestEntryDirection_RejectsUnknownNormalSide pins that EntryDirection —
// account_policy_enforce.go's hot-path call, gating deposit/withdrawal
// account-freeze enforcement (I-17) — refuses rather than silently
// classifying an unknown normal_side as a decrease (its pre-I-43 behavior:
// `increases` stayed false for any normalSide outside {debit, credit},
// which would have misclassified genuine increases as consumption and
// could have wrongly blocked deposits under a frozen/min-balance policy).
func TestEntryDirection_RejectsUnknownNormalSide(t *testing.T) {
	_, err := EntryDirection(EntryTypeDebit, NormalSide("bogus"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}
