package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDecimalsClient satisfies tokenDecimalsCaller, answering decimals()
// with a configured value (or an error) without a live RPC connection.
type fakeDecimalsClient struct {
	decimals int64
	// returnBytes overrides the encoded return value when non-nil, so
	// malformed answers are expressible.
	returnBytes []byte
	err         error
	calls       int
}

func (f *fakeDecimalsClient) CallContract(_ context.Context, _ ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.returnBytes != nil {
		return f.returnBytes, nil
	}
	out := make([]byte, 32)
	big.NewInt(f.decimals).FillBytes(out)
	return out, nil
}

func decimalsChainConfig(decimals int32) core.ChainConfig {
	return core.ChainConfig{
		ChainID: 1,
		CreditTokens: map[string]core.TokenConfig{
			testTokenAddr: {TokenAddress: testTokenAddr, CurrencyCode: "USDT", Decimals: decimals},
		},
		SweepTokens: map[string]core.TokenConfig{
			core.SweepNativeToken: {TokenAddress: core.SweepNativeToken, CurrencyCode: "ETH", Decimals: 18},
		},
	}
}

// TestVerifyTokenDecimals_RejectsMismatch pins G-M7
// (onchain-money-path.md Major): TokenConfig.Decimals is the only input to
// the raw-amount normalization, so it alone decides the order of magnitude
// every deposit is credited at -- and it was hand-typed in the composition
// root and never checked against the token contract. Under-crediting
// (configuring 18 for a 6-decimals token) produces no signal anywhere
// downstream, since the review ceiling only ever catches amounts that are
// too LARGE.
func TestVerifyTokenDecimals_RejectsMismatch(t *testing.T) {
	client := &fakeDecimalsClient{decimals: 6}

	err := verifyTokenDecimals(context.Background(), client, 1, decimalsChainConfig(18))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTokenDecimalsMismatch), "got %v", err)
	assert.Equal(t, 1, client.calls, "native tokens have no contract to ask, so only the ERC-20 is queried")
}

func TestVerifyTokenDecimals_AcceptsMatch(t *testing.T) {
	client := &fakeDecimalsClient{decimals: 6}
	require.NoError(t, verifyTokenDecimals(context.Background(), client, 1, decimalsChainConfig(6)))
}

// TestVerifyTokenDecimals_UnreadableIsDistinctFromMismatch pins the
// fail-closed half: "we could not check" must be a distinguishable error, and
// must never be a defaulted zero that then looks like a validly configured
// 0-decimals token (working-agreements §3: 未运行 ≠ 通过).
func TestVerifyTokenDecimals_UnreadableIsDistinctFromMismatch(t *testing.T) {
	for name, client := range map[string]*fakeDecimalsClient{
		"rpc error":       {err: fmt.Errorf("execution reverted")},
		"short return":    {returnBytes: []byte{0x06}},
		"not a uint8":     {returnBytes: append(make([]byte, 31), 0x00), decimals: 0},
		"absurd decimals": {decimals: 300},
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyTokenDecimals(context.Background(), client, 1, decimalsChainConfig(6))
			if name == "not a uint8" {
				// A 32-byte zero word decodes fine; it is a MISMATCH
				// against the configured 6, not an unreadable answer.
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrTokenDecimalsMismatch), "got %v", err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrTokenDecimalsUnreadable), "got %v", err)
			assert.False(t, errors.Is(err, ErrTokenDecimalsMismatch),
				"an unreadable answer must not be reported as a disagreement")
		})
	}
}

// TestVerifyTokenDecimals_RejectsUnusableConfiguredValue pins that the
// startup check runs core.TokenConfig.Validate before going to the chain: a
// negative Decimals MULTIPLIES every credited amount, so it must not even
// get as far as being compared.
func TestVerifyTokenDecimals_RejectsUnusableConfiguredValue(t *testing.T) {
	client := &fakeDecimalsClient{decimals: 6}

	err := verifyTokenDecimals(context.Background(), client, 1, decimalsChainConfig(-6))
	require.Error(t, err)
	assert.True(t, errors.Is(err, core.ErrInvalidInput), "got %v", err)
	assert.Equal(t, 0, client.calls, "an unusable configured value is rejected before any RPC call")
}
