package evm

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// tokenDecimalsCaller is the subset of *ethclient.Client the startup
// decimals() cross-check needs -- narrowed (like quoteFeeClient in
// sweeper.go) so the check is unit-testable without a live RPC connection.
type tokenDecimalsCaller interface {
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

// VerifyTokenDecimals cross-checks every configured non-native token's
// core.TokenConfig.Decimals against the token contract's own decimals()
// return value, on every chain in the set. Read-only: it issues one
// eth_call per (chain, token) and writes nothing.
//
// Why this exists (onchain-money-path.md Major, G-M7): Decimals is the sole
// input to normalizeAmount, so it alone decides the ORDER OF MAGNITUDE every
// deposit is credited at, and it is hand-typed in the consumer's composition
// root. Configuring 18 for a 6-decimals token credits 10^-12 of the real
// amount; configuring 6 for an 18-decimals token credits 10^12 times it.
// Only the second direction has any chance of being caught downstream (by
// TokenConfig.AutoCreditCeiling's review gate); under-crediting is
// completely silent. The chain already knows the right answer, so not asking
// it was the gap.
//
// This library ships no binary and therefore cannot call this for the
// consumer: composition roots must call it once at startup, before wiring
// service.Onchain, and refuse to start on error (see doc.go). A mismatch is
// wrapped in ErrTokenDecimalsMismatch; a token whose decimals() cannot be
// read at all (not an ERC-20, a proxy that reverts, an RPC failure) is
// wrapped in ErrTokenDecimalsUnreadable so a consumer can tell the two apart
// and decide -- but neither is silently ignored here.
func (cs *ClientSet) VerifyTokenDecimals(ctx context.Context) error {
	chainIDs := make([]int64, 0, len(cs.chains))
	for chainID := range cs.chains {
		chainIDs = append(chainIDs, chainID)
	}
	sort.Slice(chainIDs, func(i, j int) bool { return chainIDs[i] < chainIDs[j] })

	for _, chainID := range chainIDs {
		client, err := cs.client(chainID)
		if err != nil {
			return err
		}
		if err := verifyTokenDecimals(ctx, client, chainID, cs.chains[chainID]); err != nil {
			return err
		}
	}
	return nil
}

// verifyTokenDecimals is VerifyTokenDecimals' body for one chain, over the
// narrowed caller interface (see TestVerifyTokenDecimals_*).
func verifyTokenDecimals(ctx context.Context, client tokenDecimalsCaller, chainID int64, cfg core.ChainConfig) error {
	configured := make(map[string]core.TokenConfig)
	for _, tokens := range []map[string]core.TokenConfig{cfg.CreditTokens, cfg.SweepTokens} {
		for key, tc := range tokens {
			if key == core.SweepNativeToken {
				continue // native asset has no contract to ask
			}
			configured[normalizeTokenKey(key)] = tc
		}
	}

	keys := make([]string, 0, len(configured))
	for key := range configured {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		tc := configured[key]
		if err := tc.Validate(); err != nil {
			return fmt.Errorf("evm: verify token decimals: chain %d token %q: %w", chainID, key, err)
		}
		if !common.IsHexAddress(key) {
			return fmt.Errorf("evm: verify token decimals: chain %d token %q is neither %q nor a contract address: %w",
				chainID, key, core.SweepNativeToken, core.ErrInvalidInput)
		}
		onchain, err := readTokenDecimals(ctx, client, key)
		if err != nil {
			return fmt.Errorf("evm: verify token decimals: chain %d token %q: %w", chainID, key, err)
		}
		if onchain != tc.Decimals {
			return fmt.Errorf("evm: verify token decimals: chain %d token %q: configured Decimals=%d but the contract reports %d -- every deposit of this token would be credited at 10^%d times the real amount: %w",
				chainID, key, tc.Decimals, onchain, int(onchain)-int(tc.Decimals), ErrTokenDecimalsMismatch)
		}
	}
	return nil
}

// readTokenDecimals calls decimals() on token and decodes the uint8 return.
// Anything that is not a well-formed uint8 word is ErrTokenDecimalsUnreadable
// -- never a defaulted zero, which would look like a validly configured
// 0-decimals token.
func readTokenDecimals(ctx context.Context, client tokenDecimalsCaller, token string) (int32, error) {
	data, err := erc20ABI.Pack("decimals")
	if err != nil {
		return 0, fmt.Errorf("pack decimals: %w", err)
	}
	addr := common.HexToAddress(token)
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: data}, nil)
	if err != nil {
		return 0, fmt.Errorf("call decimals(): %w: %w", err, ErrTokenDecimalsUnreadable)
	}
	if len(out) != 32 {
		return 0, fmt.Errorf("decimals() returned %d bytes (want 32): %w", len(out), ErrTokenDecimalsUnreadable)
	}
	raw := new(big.Int).SetBytes(out)
	if !raw.IsInt64() || raw.Int64() > 255 {
		return 0, fmt.Errorf("decimals() returned %s, which does not fit uint8: %w", raw, ErrTokenDecimalsUnreadable)
	}
	return int32(raw.Int64()), nil
}
