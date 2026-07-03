package postgres_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// --- SetPolicy / GetPolicy / ListPolicies / audit trail ---

func TestAccountPolicyStore_SetPolicy_CreateAndGet(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	policies := postgres.NewAccountPolicyStore(p)

	holder := int64(4001)
	curID := postgrestest.SeedCurrency(t, p, "USDT-POLICY-CREATE", "Test USDT")

	created, err := policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder:     holder,
		CurrencyUID:       curID,
		Status:            core.AccountPolicyStatusFrozen,
		MinBalance:        decimal.NewFromInt(-10),
		EnforceMinBalance: true,
		Note:              "AML hold",
		ActorID:           42,
	})
	require.NoError(t, err)
	assert.Equal(t, core.AccountPolicyStatusFrozen, created.Status)
	assert.True(t, created.MinBalance.Equal(decimal.NewFromInt(-10)))
	assert.True(t, created.EnforceMinBalance)
	assert.Equal(t, "AML hold", created.Note)

	got, err := policies.GetPolicy(ctx, holder, curID, "")
	require.NoError(t, err)
	assert.Equal(t, created.UID, got.UID)
	assert.Equal(t, core.AccountPolicyStatusFrozen, got.Status)
}

func TestAccountPolicyStore_GetPolicy_NotFound(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	policies := postgres.NewAccountPolicyStore(p)

	_, err := policies.GetPolicy(ctx, 999999, "00000000-0000-7000-8000-000000000001", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAccountPolicyStore_ListPolicies(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	policies := postgres.NewAccountPolicyStore(p)

	holder := int64(4002)
	curA := postgrestest.SeedCurrency(t, p, "USDT-LIST-A", "Test USDT A")
	curB := postgrestest.SeedCurrency(t, p, "USDT-LIST-B", "Test USDT B")

	_, err := policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, CurrencyUID: curA, Status: core.AccountPolicyStatusFrozen})
	require.NoError(t, err)
	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, CurrencyUID: curB, Status: core.AccountPolicyStatusClosed})
	require.NoError(t, err)

	list, err := policies.ListPolicies(ctx, holder)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestAccountPolicyStore_SetPolicy_AuditTrail(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	policies := postgres.NewAccountPolicyStore(p)

	holder := int64(4003)
	curID := postgrestest.SeedCurrency(t, p, "USDT-AUDIT", "Test USDT")

	created, err := policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder: holder,
		CurrencyUID:   curID,
		Status:        core.AccountPolicyStatusActive,
		ActorID:       7,
	})
	require.NoError(t, err)

	updated, err := policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder: holder,
		CurrencyUID:   curID,
		Status:        core.AccountPolicyStatusFrozen,
		Note:          "manual freeze",
		ActorID:       8,
	})
	require.NoError(t, err)
	assert.Equal(t, created.UID, updated.UID, "SetPolicy on the same dimension must UPSERT the same row")

	rows, err := p.Query(ctx, `SELECT policy_id, old_state, new_state, actor_id FROM account_policy_changes WHERE policy_id = (SELECT id FROM account_policies WHERE uid=$1::uuid) ORDER BY created_at`, created.UID)
	require.NoError(t, err)
	defer rows.Close()

	type change struct {
		policyID int64
		oldState map[string]any
		newState map[string]any
		actorID  int64
	}
	var changes []change
	for rows.Next() {
		var c change
		var oldRaw, newRaw []byte
		require.NoError(t, rows.Scan(&c.policyID, &oldRaw, &newRaw, &c.actorID))
		require.NoError(t, json.Unmarshal(oldRaw, &c.oldState))
		require.NoError(t, json.Unmarshal(newRaw, &c.newState))
		changes = append(changes, c)
	}
	require.NoError(t, rows.Err())
	require.Len(t, changes, 2, "one audit row per SetPolicy call")

	// First change: created from nothing.
	assert.Empty(t, changes[0].oldState)
	assert.Equal(t, "active", changes[0].newState["status"])
	assert.Equal(t, int64(7), changes[0].actorID)

	// Second change: active -> frozen, old_state carries the prior row.
	assert.Equal(t, "active", changes[1].oldState["status"])
	assert.Equal(t, "frozen", changes[1].newState["status"])
	assert.Equal(t, "manual freeze", changes[1].newState["note"])
	assert.Equal(t, int64(8), changes[1].actorID)
}

// --- Status enforcement matrix: {active, frozen, closed} x {increase, decrease, Reserve} ---

func TestLedgerStore_AccountPolicy_StatusMatrix(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	ls := postgres.NewLedgerStore(p)
	reserver := postgres.NewReserverStore(p, ls)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-MATRIX", "Test USDT")
	walletID := postgrestest.SeedClassification(t, p, "wallet_matrix", "Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_matrix", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_matrix", "Test JT")

	seedBalance := func(t *testing.T, holder int64, amount decimal.Decimal) {
		t.Helper()
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("matrix-seed"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: amount},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: amount},
			},
			Source: "test",
		})
		require.NoError(t, err)
	}

	increase := func(holder int64) error {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("matrix-increase"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
			},
			Source: "test",
		})
		return err
	}

	decrease := func(holder int64) error {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("matrix-decrease"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			},
			Source: "test",
		})
		return err
	}

	reserve := func(holder int64) error {
		_, err := reserver.Reserve(ctx, core.ReserveInput{
			AccountHolder:  holder,
			CurrencyUID:    curID,
			Amount:         decimal.NewFromInt(5),
			IdempotencyKey: postgrestest.UniqueKey("matrix-reserve"),
		})
		return err
	}

	tests := []struct {
		name            string
		status          core.AccountPolicyStatus
		wantIncreaseErr error
		wantDecreaseErr error
		wantReserveErr  error
	}{
		{"active", core.AccountPolicyStatusActive, nil, nil, nil},
		{"frozen", core.AccountPolicyStatusFrozen, nil, core.ErrAccountFrozen, core.ErrAccountFrozen},
		{"closed", core.AccountPolicyStatusClosed, core.ErrAccountClosed, core.ErrAccountClosed, core.ErrAccountClosed},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			holder := int64(9100 + i)
			seedBalance(t, holder, decimal.NewFromInt(1000))

			// currency-wide (tier 2: holder,currency,0) so the SAME policy
			// governs both the classification-scoped journal entries above
			// and Reserve (which is never tied to a classification).
			_, err := policies.SetPolicy(ctx, core.AccountPolicyInput{
				AccountHolder: holder,
				CurrencyUID:   curID,
				Status:        tc.status,
			})
			require.NoError(t, err)

			errInc := increase(holder)
			errDec := decrease(holder)
			errRes := reserve(holder)

			if tc.wantIncreaseErr == nil {
				assert.NoError(t, errInc, "increase")
			} else {
				assert.ErrorIs(t, errInc, tc.wantIncreaseErr, "increase")
			}
			if tc.wantDecreaseErr == nil {
				assert.NoError(t, errDec, "decrease")
			} else {
				assert.ErrorIs(t, errDec, tc.wantDecreaseErr, "decrease")
			}
			if tc.wantReserveErr == nil {
				assert.NoError(t, errRes, "reserve")
			} else {
				assert.ErrorIs(t, errRes, tc.wantReserveErr, "reserve")
			}
		})
	}
}

// TestLedgerStore_AccountPolicy_MatchPriority pins the priority order:
// (holder,currency,classification) > (holder,currency,0) > (holder,0,0).
func TestLedgerStore_AccountPolicy_MatchPriority(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	ls := postgres.NewLedgerStore(p)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-PRIORITY", "Test USDT")
	walletAID := postgrestest.SeedClassification(t, p, "wallet_a_priority", "Wallet A", "debit", false)
	walletBID := postgrestest.SeedClassification(t, p, "wallet_b_priority", "Wallet B", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_priority", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_priority", "Test JT")
	holder := int64(4200)

	seed := func(classificationUID string, amount decimal.Decimal) {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("priority-seed"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: classificationUID, EntryType: core.EntryTypeDebit, Amount: amount},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: amount},
			},
			Source: "test",
		})
		require.NoError(t, err)
	}
	seed(walletAID, decimal.NewFromInt(100))
	seed(walletBID, decimal.NewFromInt(100))

	decrease := func(classificationUID string) error {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("priority-decrease"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: classificationUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
			},
			Source: "test",
		})
		return err
	}

	// Tier 2: freeze the whole holder+currency.
	_, err := policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, CurrencyUID: curID, Status: core.AccountPolicyStatusFrozen})
	require.NoError(t, err)

	// Tier 1: explicitly re-activate wallet_a only — more specific, must win.
	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletAID, Status: core.AccountPolicyStatusActive})
	require.NoError(t, err)

	assert.NoError(t, decrease(walletAID), "tier-1 active override must win over tier-2 frozen")
	assert.ErrorIs(t, decrease(walletBID), core.ErrAccountFrozen, "wallet_b has no tier-1 override, must fall back to tier-2 frozen")
}

// TestLedgerStore_ConfirmPending_SucceedsWhileFrozen pins the explicit
// business requirement (design doc §4/§9-1): frozen blocks consumption, not
// the pending two-phase deposit flow. ConfirmPending posts a decrease to
// "pending" and an equal increase to "main_wallet" for the same holder in one
// journal — net zero under a holder+currency-wide freeze — so it must still
// succeed. A genuine net decrease under the same freeze must still be
// rejected.
func TestLedgerStore_ConfirmPending_SucceedsWhileFrozen(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-FROZEN-CONFIRM", "Test USDT")
	jtID := postgrestest.SeedJournalType(t, p, "jt_frozen_confirm", "Test JT")
	pendingStore := postgres.NewPendingStore(p, ls, cs)
	policies := postgres.NewAccountPolicyStore(p)

	userID := int64(4300)
	amount := decimal.NewFromInt(300)

	_, err := pendingStore.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("frozen-confirm-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder: userID,
		CurrencyUID:   curID,
		Status:        core.AccountPolicyStatusFrozen,
	})
	require.NoError(t, err)

	j, err := pendingStore.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("frozen-confirm-confirm"),
		Source:         "test",
	})
	require.NoError(t, err, "ConfirmPending must succeed while frozen — deposit finalization is not consumption")
	require.NotNil(t, j)

	mainWalletCls, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	bal, err := ls.GetBalance(ctx, userID, curID, mainWalletCls.UID)
	require.NoError(t, err)
	assert.True(t, bal.Equal(amount), "main_wallet balance should equal confirmed amount, got %s", bal)

	custodialCls, err := cs.GetByCode(ctx, "custodial")
	require.NoError(t, err)

	// A genuine net decrease under the same freeze must still be rejected.
	_, err = ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("frozen-confirm-withdraw"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: curID, ClassificationUID: mainWalletCls.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: curID, ClassificationUID: custodialCls.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
		},
		Source: "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrAccountFrozen)
}

// --- min_balance: zero / negative / positive, and same-journal netting ---

func TestLedgerStore_AccountPolicy_MinBalance_ZeroForbidsOverdraft(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	ls := postgres.NewLedgerStore(p)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-MINBAL-ZERO", "Test USDT")
	walletID := postgrestest.SeedClassification(t, p, "wallet_minbal_zero", "Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_minbal_zero", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_minbal_zero", "Test JT")
	holder := int64(4400)

	_, err := ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("minbal-zero-seed"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder:     holder,
		CurrencyUID:       curID,
		ClassificationUID: walletID,
		Status:            core.AccountPolicyStatusActive,
		MinBalance:        decimal.Zero,
		EnforceMinBalance: true,
	})
	require.NoError(t, err)

	decreaseBy := func(amount decimal.Decimal) error {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("minbal-zero-decrease"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: amount},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: amount},
			},
			Source: "test",
		})
		return err
	}

	// Exactly to zero: allowed.
	require.NoError(t, decreaseBy(decimal.NewFromInt(100)))

	bal, err := ls.GetBalance(ctx, holder, curID, walletID)
	require.NoError(t, err)
	assert.True(t, bal.IsZero())

	// One more unit would go negative: rejected.
	err = decreaseBy(decimal.NewFromInt(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)
}

func TestLedgerStore_AccountPolicy_MinBalance_NegativeAllowsOverdraftLimit(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	ls := postgres.NewLedgerStore(p)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-MINBAL-NEG", "Test USDT")
	walletID := postgrestest.SeedClassification(t, p, "wallet_minbal_neg", "Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_minbal_neg", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_minbal_neg", "Test JT")
	holder := int64(4500)

	_, err := policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder:     holder,
		CurrencyUID:       curID,
		ClassificationUID: walletID,
		Status:            core.AccountPolicyStatusActive,
		MinBalance:        decimal.NewFromInt(-50), // overdraft limit of 50
		EnforceMinBalance: true,
	})
	require.NoError(t, err)

	decreaseBy := func(amount decimal.Decimal, key string) error {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: key,
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: amount},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: amount},
			},
			Source: "test",
		})
		return err
	}

	// Starting balance is 0 (no seed). Overdraw to exactly -50: allowed.
	require.NoError(t, decreaseBy(decimal.NewFromInt(50), postgrestest.UniqueKey("minbal-neg-1")))

	bal, err := ls.GetBalance(ctx, holder, curID, walletID)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(-50)))

	// One more unit exceeds the overdraft limit: rejected.
	err = decreaseBy(decimal.NewFromInt(1), postgrestest.UniqueKey("minbal-neg-2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)
}

func TestLedgerStore_AccountPolicy_MinBalance_PositiveDustFloor(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	ls := postgres.NewLedgerStore(p)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-MINBAL-POS", "Test USDT")
	walletID := postgrestest.SeedClassification(t, p, "wallet_minbal_pos", "Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_minbal_pos", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_minbal_pos", "Test JT")
	holder := int64(4600)

	_, err := ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("minbal-pos-seed"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder:     holder,
		CurrencyUID:       curID,
		ClassificationUID: walletID,
		Status:            core.AccountPolicyStatusActive,
		MinBalance:        decimal.NewFromInt(10), // dust floor
		EnforceMinBalance: true,
	})
	require.NoError(t, err)

	decreaseBy := func(amount decimal.Decimal, key string) error {
		_, err := ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: key,
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: amount},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: amount},
			},
			Source: "test",
		})
		return err
	}

	// Down to exactly the floor: allowed.
	require.NoError(t, decreaseBy(decimal.NewFromInt(90), postgrestest.UniqueKey("minbal-pos-1")))

	bal, err := ls.GetBalance(ctx, holder, curID, walletID)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(10)))

	// Below the floor: rejected.
	err = decreaseBy(decimal.NewFromInt(1), postgrestest.UniqueKey("minbal-pos-2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)
}

// TestLedgerStore_AccountPolicy_MinBalance_SameJournalNetting pins that
// min_balance is evaluated against the NET delta a journal posts to a
// dimension, not per-entry — a journal with an intermediate decrease that
// nets to an increase must not be falsely rejected.
func TestLedgerStore_AccountPolicy_MinBalance_SameJournalNetting(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	ls := postgres.NewLedgerStore(p)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-MINBAL-NET", "Test USDT")
	walletID := postgrestest.SeedClassification(t, p, "wallet_minbal_net", "Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_minbal_net", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_minbal_net", "Test JT")
	holder := int64(4700)

	// Seed to exactly the floor so any per-entry (non-netted) evaluation of
	// the credit-30 leg below would read as "10 - 30 = -20 < 10" and
	// incorrectly reject.
	_, err := ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("minbal-net-seed"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder:     holder,
		CurrencyUID:       curID,
		ClassificationUID: walletID,
		Status:            core.AccountPolicyStatusActive,
		MinBalance:        decimal.NewFromInt(10),
		EnforceMinBalance: true,
	})
	require.NoError(t, err)

	// Two entries on the SAME (holder,currency,classification) dimension in
	// one journal: debit 100 (increase), credit 30 (decrease). Net +70.
	// Balanced against a single custodial leg of 70.
	_, err = ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("minbal-net-journal"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(30)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(70)},
		},
		Source: "test",
	})
	require.NoError(t, err, "net-positive journal must not be rejected by an intermediate decrease leg")

	bal, err := ls.GetBalance(ctx, holder, curID, walletID)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(80)), "10 (seed) + 100 - 30 = 80, got %s", bal)
}

// --- Concurrency: SetPolicy racing PostJournal on the same (holder,currency) ---

func TestAccountPolicyStore_SetPolicy_ConcurrentWithPostJournal(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()
	ls := postgres.NewLedgerStore(p)
	policies := postgres.NewAccountPolicyStore(p)

	curID := postgrestest.SeedCurrency(t, p, "USDT-CONCURRENT-FREEZE", "Test USDT")
	walletID := postgrestest.SeedClassification(t, p, "wallet_concurrent_freeze", "Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, p, "custodial_concurrent_freeze", "Custodial", "credit", true)
	jtID := postgrestest.SeedJournalType(t, p, "jt_concurrent_freeze", "Test JT")
	holder := int64(4800)

	_, err := ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("concurrent-freeze-seed"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1000)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1000)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	var freezeErr, journalErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, freezeErr = policies.SetPolicy(ctx, core.AccountPolicyInput{AccountHolder: holder, CurrencyUID: curID, Status: core.AccountPolicyStatusFrozen})
	}()
	go func() {
		defer wg.Done()
		_, journalErr = ls.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtID,
			IdempotencyKey: postgrestest.UniqueKey("concurrent-freeze-decrease"),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			},
			Source: "test",
		})
	}()
	wg.Wait()

	require.NoError(t, freezeErr)

	bal, err := ls.GetBalance(ctx, holder, curID, walletID)
	require.NoError(t, err)

	// Whichever transaction won the (holder, currency) advisory lock first
	// determines a single coherent outcome: either the decrease committed
	// before the freeze took effect (succeeded, balance reflects it), or the
	// freeze committed first and the decrease was rejected (balance
	// unchanged). No partial or corrupted state is observable either way.
	if journalErr == nil {
		assert.True(t, bal.Equal(decimal.NewFromInt(990)), "decrease committed, balance should reflect it")
	} else {
		assert.ErrorIs(t, journalErr, core.ErrAccountFrozen)
		assert.True(t, bal.Equal(decimal.NewFromInt(1000)), "decrease rejected, balance must be unchanged")
	}

	// Once frozen is confirmed committed, a fresh decrease must now be
	// rejected deterministically — proves the freeze is actually effective
	// going forward, not just a race artifact.
	_, err = ls.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("concurrent-freeze-followup"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: walletID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodialID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
		},
		Source: "test",
	})
	assert.ErrorIs(t, err, core.ErrAccountFrozen)
}
