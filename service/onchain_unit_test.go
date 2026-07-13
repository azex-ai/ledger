package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- parseDepositMeta ---

func TestParseDepositMeta(t *testing.T) {
	complete := map[string]string{"chain_id": "1", "tx_hash": "0xabc", "txlog_seq": "2", "block_number": "100"}
	cases := []struct {
		name string
		meta map[string]string
		ok   bool
	}{
		{name: "complete", meta: complete, ok: true},
		{name: "missing chain_id", meta: map[string]string{"tx_hash": "0xabc", "txlog_seq": "2", "block_number": "100"}, ok: false},
		{name: "missing tx_hash", meta: map[string]string{"chain_id": "1", "txlog_seq": "2", "block_number": "100"}, ok: false},
		{name: "missing txlog_seq", meta: map[string]string{"chain_id": "1", "tx_hash": "0xabc", "block_number": "100"}, ok: false},
		{name: "missing block_number", meta: map[string]string{"chain_id": "1", "tx_hash": "0xabc", "txlog_seq": "2"}, ok: false},
		{name: "empty tx_hash", meta: map[string]string{"chain_id": "1", "tx_hash": "", "txlog_seq": "2", "block_number": "100"}, ok: false},
		{name: "non-numeric chain_id", meta: map[string]string{"chain_id": "x", "tx_hash": "0xabc", "txlog_seq": "2", "block_number": "100"}, ok: false},
		{name: "non-numeric txlog_seq", meta: map[string]string{"chain_id": "1", "tx_hash": "0xabc", "txlog_seq": "x", "block_number": "100"}, ok: false},
		{name: "non-numeric block_number", meta: map[string]string{"chain_id": "1", "tx_hash": "0xabc", "txlog_seq": "2", "block_number": "x"}, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chainID, txHash, txLogSeq, blockNumber, ok := parseDepositMeta(tc.meta)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, int64(1), chainID)
				assert.Equal(t, "0xabc", txHash)
				assert.Equal(t, int32(2), txLogSeq)
				assert.Equal(t, int64(100), blockNumber)
			}
		})
	}
}

func TestDepositChannelRef_UniquePerTxLogSeq(t *testing.T) {
	ref0 := depositChannelRef("0xabc", 0)
	ref1 := depositChannelRef("0xabc", 1)
	assert.NotEqual(t, ref0, ref1, "two Transfer logs within the same tx must not collide on bookings' UNIQUE (channel_name, channel_ref) index")
}

// --- depositIdempotencyKey ---

func TestDepositIdempotencyKey_StableAcrossReobservation(t *testing.T) {
	// Same underlying transfer observed twice (once at 0 confirmations, once
	// at threshold) must derive the identical key -- this is the whole point
	// of keying on (chain_id, tx_hash, txlog_seq) and NOT on anything that
	// varies per observation (design doc §3).
	k1 := depositIdempotencyKey(1, "0xabc", 0)
	k2 := depositIdempotencyKey(1, "0xabc", 0)
	assert.Equal(t, k1, k2)

	// A different txlog_seq (a second Transfer log crediting us within the
	// same tx) must derive a DIFFERENT key -- no collision between two
	// distinct transfers in one transaction.
	k3 := depositIdempotencyKey(1, "0xabc", 1)
	assert.NotEqual(t, k1, k3)

	// A different chain must derive a different key even for the same tx
	// hash (tx hashes are not unique across chains).
	k4 := depositIdempotencyKey(2, "0xabc", 0)
	assert.NotEqual(t, k1, k4)
}

// --- sweepSystemHolder ---

func TestSweepSystemHolder_DistinctAndNegative(t *testing.T) {
	h1 := sweepSystemHolder(1)
	h2 := sweepSystemHolder(2)
	assert.NotEqual(t, h1, h2)
	assert.Negative(t, h1)
	assert.Negative(t, h2)

	// Must not collide with SystemAccountHolder's realistic negation range
	// (real userIDs are not expected anywhere near sweepSystemHolderBase).
	assert.NotEqual(t, h1, core.SystemAccountHolder(1))
}

// --- canonicalFactory ---

func TestOnchain_CanonicalFactory(t *testing.T) {
	factory := "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323"
	initHash := "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ec"

	t.Run("empty chain set errors", func(t *testing.T) {
		o := NewOnchain(OnchainDeps{}, core.ChainSet{})
		_, _, err := o.canonicalFactory()
		require.Error(t, err)
	})

	t.Run("single chain", func(t *testing.T) {
		o := NewOnchain(OnchainDeps{}, core.ChainSet{
			1: {ChainID: 1, Factory: factory, InitHash: initHash},
		})
		f, i, err := o.canonicalFactory()
		require.NoError(t, err)
		assert.Equal(t, factory, f)
		assert.Equal(t, initHash, i)
	})

	t.Run("agreeing chains", func(t *testing.T) {
		o := NewOnchain(OnchainDeps{}, core.ChainSet{
			1: {ChainID: 1, Factory: factory, InitHash: initHash},
			2: {ChainID: 2, Factory: factory, InitHash: initHash},
		})
		f, i, err := o.canonicalFactory()
		require.NoError(t, err)
		assert.Equal(t, factory, f)
		assert.Equal(t, initHash, i)
	})

	t.Run("mismatched chains error", func(t *testing.T) {
		o := NewOnchain(OnchainDeps{}, core.ChainSet{
			1: {ChainID: 1, Factory: factory, InitHash: initHash},
			2: {ChainID: 2, Factory: "0x0000000000000000000000000000000000dEaD", InitHash: initHash},
		})
		_, _, err := o.canonicalFactory()
		require.Error(t, err)
		assert.ErrorIs(t, err, core.ErrInvalidInput)
	})
}

// --- resolveSweepCurrency ---

type fakeCurrencyStore struct {
	currencies []core.Currency
}

func (f *fakeCurrencyStore) CreateCurrency(ctx context.Context, input core.CurrencyInput) (*core.Currency, error) {
	return nil, nil
}
func (f *fakeCurrencyStore) DeactivateCurrency(ctx context.Context, uid string) error { return nil }
func (f *fakeCurrencyStore) ListCurrencies(ctx context.Context, activeOnly bool) ([]core.Currency, error) {
	return f.currencies, nil
}
func (f *fakeCurrencyStore) GetCurrency(ctx context.Context, uid string) (*core.Currency, error) {
	return nil, core.ErrNotFound
}

func TestOnchain_ResolveSweepCurrency(t *testing.T) {
	currencies := &fakeCurrencyStore{currencies: []core.Currency{
		{UID: "usdt-uid", Code: "USDT"},
		{UID: "eth-uid", Code: "ETH"},
	}}
	o := NewOnchain(OnchainDeps{Currencies: currencies}, core.ChainSet{})
	ctx := context.Background()

	cfg := core.ChainConfig{
		ChainID:      1,
		CreditTokens: map[string]core.TokenConfig{"0xusdt": {TokenAddress: "0xusdt", CurrencyCode: "USDT"}},
		SweepTokens: map[string]core.TokenConfig{
			"0xusdt":              {TokenAddress: "0xusdt", CurrencyCode: "USDT"},
			core.SweepNativeToken: {TokenAddress: core.SweepNativeToken, CurrencyCode: "ETH"},
		},
	}

	t.Run("credited token is attributed", func(t *testing.T) {
		uid, unattributed, err := o.resolveSweepCurrency(ctx, cfg, "0xusdt")
		require.NoError(t, err)
		assert.Equal(t, "usdt-uid", uid)
		assert.False(t, unattributed)
	})

	t.Run("native token is unattributed", func(t *testing.T) {
		uid, unattributed, err := o.resolveSweepCurrency(ctx, cfg, core.SweepNativeToken)
		require.NoError(t, err)
		assert.Equal(t, "eth-uid", uid)
		assert.True(t, unattributed)
	})

	t.Run("token not in sweep_tokens allowlist errors", func(t *testing.T) {
		_, _, err := o.resolveSweepCurrency(ctx, cfg, "0xunknown")
		require.Error(t, err)
		assert.ErrorIs(t, err, core.ErrInvalidInput)
	})
}

// --- currencyResolver caching ---

func TestCurrencyResolver_CachesAndRefreshesOnMiss(t *testing.T) {
	store := &fakeCurrencyStore{currencies: []core.Currency{{UID: "usdt-uid", Code: "USDT"}}}
	r := newCurrencyResolver()
	ctx := context.Background()

	uid, err := r.resolve(ctx, store, "USDT")
	require.NoError(t, err)
	assert.Equal(t, "usdt-uid", uid)

	_, err = r.resolve(ctx, store, "EUR")
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrNotFound)

	// A currency added after the first miss must be picked up on the next
	// resolve (cache refreshes on miss, not just once at construction).
	store.currencies = append(store.currencies, core.Currency{UID: "eur-uid", Code: "EUR"})
	uid, err = r.resolve(ctx, store, "EUR")
	require.NoError(t, err)
	assert.Equal(t, "eur-uid", uid)
}
