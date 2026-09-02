package evm

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanner_TokenDecimals(t *testing.T) {
	clients := &ClientSet{
		chains: core.ChainSet{
			1: {
				ChainID: 1,
				SweepTokens: map[string]core.TokenConfig{
					core.SweepNativeToken: {TokenAddress: core.SweepNativeToken, CurrencyCode: "ETH", Decimals: 18},
				},
				CreditTokens: map[string]core.TokenConfig{
					"0xdac17f958d2ee523a2206206994597c13d831ec7": {TokenAddress: "0xdac17f958d2ee523a2206206994597c13d831ec7", CurrencyCode: "USDT", Decimals: 6},
				},
			},
		},
		clients: map[int64]*ethclient.Client{},
	}
	scanner := NewScanner(clients, 0)

	decimals, err := scanner.tokenDecimals(1, "0xDAC17F958D2ee523a2206206994597C13D831ec7") // mixed case must still match the lowercase-keyed config
	if err != nil {
		t.Fatalf("tokenDecimals(credit token): %v", err)
	}
	if decimals != 6 {
		t.Errorf("decimals = %d, want 6", decimals)
	}

	decimals, err = scanner.tokenDecimals(1, core.SweepNativeToken)
	if err != nil {
		t.Fatalf("tokenDecimals(native): %v", err)
	}
	if decimals != 18 {
		t.Errorf("decimals = %d, want 18", decimals)
	}

	if _, err := scanner.tokenDecimals(1, "0x000000000000000000000000000000000000ff"); err == nil {
		t.Error("expected ErrTokenNotConfigured for unregistered token, got nil")
	}

	if _, err := scanner.tokenDecimals(999, core.SweepNativeToken); err == nil {
		t.Error("expected ErrChainNotConfigured for unregistered chain, got nil")
	}
}

// mustAmount32 left-pads a small balance into a 32-byte big-endian word, the
// shape both aggregate3's ReturnData and balanceOf's raw return use for a
// uint256.
func mustAmount32(v int64) []byte {
	return common.LeftPadBytes(big.NewInt(v).Bytes(), 32)
}

// TestMulticallResultsToBalances_FailsClosedPerAddress pins
// onchain-money-path.md's Major finding for the Multicall3 path (a reverted
// call or malformed return length must never become a balance of ZERO,
// silently dropping that address from the sweep round with no signal) AND
// m-10's correction on top of it (2026-08-26 independent review, third
// pass): the address that could not be read must land in unreadable and be
// excluded from balances, but every OTHER address in the same batch must
// still come through -- the original fix over-corrected into returning an
// error for the WHOLE batch the moment any one address failed, which this
// test used to assert (`require.Nil(t, balances, ...)`) and no longer does.
func TestMulticallResultsToBalances_FailsClosedPerAddress(t *testing.T) {
	addrs := []string{
		"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	}

	t.Run("reverted call", func(t *testing.T) {
		results := []multicall3Result{
			{Success: true, ReturnData: mustAmount32(500)},
			{Success: false, ReturnData: nil}, // reverted -- e.g. a broken/malicious token contract
		}
		balances, unreadable := multicallResultsToBalances(addrs, results, 0)
		require.Equal(t, []string{addrs[1]}, unreadable, "the reverted address must be reported unreadable, not silently dropped")
		assert.True(t, balances[addrs[0]].Equal(decimal.NewFromInt(500)), "the OTHER, readable address must still come through -- one bad address must not cost the whole batch")
		_, stillPresent := balances[addrs[1]]
		assert.False(t, stillPresent, "an unreadable address must be absent from balances, never defaulted to zero")
	})

	t.Run("malformed return length", func(t *testing.T) {
		results := []multicall3Result{
			{Success: true, ReturnData: mustAmount32(500)},
			{Success: true, ReturnData: []byte{0x01, 0x02}}, // not 32 bytes -- malformed, not "zero"
		}
		balances, unreadable := multicallResultsToBalances(addrs, results, 0)
		require.Equal(t, []string{addrs[1]}, unreadable)
		assert.True(t, balances[addrs[0]].Equal(decimal.NewFromInt(500)))
	})

	t.Run("all readable", func(t *testing.T) {
		results := []multicall3Result{
			{Success: true, ReturnData: mustAmount32(500)},
			{Success: true, ReturnData: mustAmount32(0)}, // a GENUINE zero balance must still pass through cleanly
		}
		balances, unreadable := multicallResultsToBalances(addrs, results, 0)
		assert.Empty(t, unreadable)
		assert.True(t, balances[addrs[0]].Equal(decimal.NewFromInt(500)))
		assert.True(t, balances[addrs[1]].IsZero())
	})
}

// TestDecodeERC20BalanceOf_FailsClosedOnMalformedReturn pins the same fix on
// the concurrent fallback path: a malformed (non-32-byte) balanceOf return
// used to become a silent zero balance instead of an error, contradicting
// the RPC-error branch immediately above it in scanConcurrently which
// already failed the whole scan closed.
func TestDecodeERC20BalanceOf_FailsClosedOnMalformedReturn(t *testing.T) {
	_, err := decodeERC20BalanceOf("0x70997970C51812dc3A010C7d01b50e0d17dc79C8", []byte{0xde, 0xad})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBalanceUnreadable))

	// A genuine, well-formed zero balance must still decode cleanly.
	raw, err := decodeERC20BalanceOf("0x70997970C51812dc3A010C7d01b50e0d17dc79C8", mustAmount32(0))
	require.NoError(t, err)
	assert.Equal(t, int64(0), raw.Int64())
}

// fakeCodeAtClient satisfies codeAtClient, answering CodeAt from a queue so
// a transient empty answer followed by a real one is expressible.
type fakeCodeAtClient struct {
	answers [][]byte
	calls   int
}

func (f *fakeCodeAtClient) CodeAt(_ context.Context, _ common.Address, _ *big.Int) ([]byte, error) {
	i := f.calls
	f.calls++
	if i < len(f.answers) {
		return f.answers[i], nil
	}
	return f.answers[len(f.answers)-1], nil
}

// TestScanner_ProbeMulticall_DoesNotCacheANegativeProbe pins G-m3
// (onchain-money-path.md Minor): a "Multicall3 is not deployed here" answer
// must not be cached. A contract cannot be un-deployed, so a positive answer
// is permanent; a negative one may just be an out-of-sync node, and caching
// it pinned the chain to the N-single-calls fallback for the whole process
// lifetime with no way back.
func TestScanner_ProbeMulticall_DoesNotCacheANegativeProbe(t *testing.T) {
	client := &fakeCodeAtClient{answers: [][]byte{{}, {0x60, 0x60}}}
	scanner := NewScanner(&ClientSet{chains: core.ChainSet{1: {ChainID: 1}}}, 0)
	ctx := context.Background()

	has, err := scanner.probeMulticall(ctx, client, 1)
	require.NoError(t, err)
	require.False(t, has, "first probe sees no code")

	has, err = scanner.probeMulticall(ctx, client, 1)
	require.NoError(t, err)
	assert.True(t, has, "a negative probe must be re-tried, not cached forever")
	assert.Equal(t, 2, client.calls, "the second call must actually reach the chain")

	// The positive answer, by contrast, is cached: no third RPC.
	has, err = scanner.probeMulticall(ctx, client, 1)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, 2, client.calls, "a positive probe is permanent and must not be re-issued")
}
