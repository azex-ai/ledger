package evm

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
)

const defaultScanConcurrency = 8

// Scanner implements core.ChainScanner (Foundation, contract §5) -- it
// enumerates a batch of registered deposit addresses' balances for the
// sweep job (design doc §4), preferring a single Multicall3 aggregate3 call
// per chain when the contract is deployed there, and falling back to
// bounded-concurrency single balanceOf/eth_getBalance calls otherwise.
type Scanner struct {
	clients     *ClientSet
	concurrency int

	mu             sync.Mutex
	multicallKnown map[int64]bool // chainID -> has multicall3 deployed
}

// NewScanner builds a Scanner over clients. concurrency bounds the
// fallback path's in-flight RPC calls when Multicall3 is unavailable (0
// keeps defaultScanConcurrency).
func NewScanner(clients *ClientSet, concurrency int) *Scanner {
	if concurrency <= 0 {
		concurrency = defaultScanConcurrency
	}
	return &Scanner{clients: clients, concurrency: concurrency, multicallKnown: make(map[int64]bool)}
}

var _ core.ChainScanner = (*Scanner)(nil)

// ScanBalances returns token's current balance (human-unit decimal.Decimal,
// normalized by the token's configured decimals) at every address in
// addresses, on chainID. token is either a contract address or
// core.SweepNativeToken.
func (s *Scanner) ScanBalances(ctx context.Context, chainID int64, token string, addresses []string) (map[string]decimal.Decimal, error) {
	if len(addresses) == 0 {
		return map[string]decimal.Decimal{}, nil
	}
	client, err := s.clients.client(chainID)
	if err != nil {
		return nil, err
	}
	decimals, err := s.tokenDecimals(chainID, token)
	if err != nil {
		return nil, err
	}

	hasMulticall, err := s.probeMulticall(ctx, client, chainID)
	if err != nil {
		return nil, err
	}
	if hasMulticall {
		return s.scanViaMulticall(ctx, client, token, addresses, decimals)
	}
	return s.scanConcurrently(ctx, client, token, addresses, decimals)
}

func (s *Scanner) tokenDecimals(chainID int64, token string) (int32, error) {
	cfg, err := s.clients.chainConfig(chainID)
	if err != nil {
		return 0, err
	}
	key := normalizeTokenKey(token)
	if tc, ok := cfg.SweepTokens[key]; ok {
		return tc.Decimals, nil
	}
	if tc, ok := cfg.CreditTokens[key]; ok {
		return tc.Decimals, nil
	}
	return 0, fmt.Errorf("evm: scanner: chain %d token %q: %w", chainID, token, ErrTokenNotConfigured)
}

func (s *Scanner) probeMulticall(ctx context.Context, client *ethclient.Client, chainID int64) (bool, error) {
	s.mu.Lock()
	known, ok := s.multicallKnown[chainID]
	s.mu.Unlock()
	if ok {
		return known, nil
	}
	code, err := client.CodeAt(ctx, multicall3Address, nil)
	if err != nil {
		return false, fmt.Errorf("evm: scanner: probe multicall3: chain %d: %w", chainID, err)
	}
	has := len(code) > 0
	s.mu.Lock()
	s.multicallKnown[chainID] = has
	s.mu.Unlock()
	return has, nil
}

func (s *Scanner) scanViaMulticall(ctx context.Context, client *ethclient.Client, token string, addresses []string, decimals int32) (map[string]decimal.Decimal, error) {
	calls := make([]multicall3Call, len(addresses))
	native := token == core.SweepNativeToken
	for i, addr := range addresses {
		if !common.IsHexAddress(addr) {
			return nil, fmt.Errorf("evm: scanner: invalid address %q: %w", addr, core.ErrInvalidInput)
		}
		account := common.HexToAddress(addr)
		if native {
			data, err := multicall3ABI.Pack("getEthBalance", account)
			if err != nil {
				return nil, fmt.Errorf("evm: scanner: pack getEthBalance: %w", err)
			}
			calls[i] = multicall3Call{Target: multicall3Address, AllowFailure: true, CallData: data}
		} else {
			data, err := erc20ABI.Pack("balanceOf", account)
			if err != nil {
				return nil, fmt.Errorf("evm: scanner: pack balanceOf: %w", err)
			}
			calls[i] = multicall3Call{Target: common.HexToAddress(token), AllowFailure: true, CallData: data}
		}
	}

	packed, err := multicall3ABI.Pack("aggregate3", calls)
	if err != nil {
		return nil, fmt.Errorf("evm: scanner: pack aggregate3: %w", err)
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &multicall3Address, Data: packed}, nil)
	if err != nil {
		return nil, fmt.Errorf("evm: scanner: aggregate3 call: %w", err)
	}
	var results []multicall3Result
	if err := multicall3ABI.UnpackIntoInterface(&results, "aggregate3", out); err != nil {
		return nil, fmt.Errorf("evm: scanner: unpack aggregate3: %w", err)
	}
	if len(results) != len(addresses) {
		return nil, fmt.Errorf("evm: scanner: aggregate3 returned %d results for %d addresses", len(results), len(addresses))
	}
	return multicallResultsToBalances(addresses, results, decimals)
}

// multicallResultsToBalances translates an already-unpacked
// Multicall3.aggregate3 response into a balance map. Split out of
// scanViaMulticall as a pure function (no RPC client involved) so the
// fail-closed behavior below is directly unit-testable -- see
// TestMulticallResultsToBalances_FailsClosedOnUnreadableCall.
func multicallResultsToBalances(addresses []string, results []multicall3Result, decimals int32) (map[string]decimal.Decimal, error) {
	balances := make(map[string]decimal.Decimal, len(addresses))
	for i, addr := range addresses {
		if !results[i].Success || len(results[i].ReturnData) != 32 {
			// Fail closed, matching scanConcurrently's RPC-error path below:
			// a reverted call or a malformed return length is not the same
			// fact as "balance is zero" (a broken/malicious token contract,
			// or a transient node issue, can produce exactly this shape).
			// Returning zero here silently dropped the address from this
			// round's sweep-eligible set with no error, no log, no metric --
			// onchain-money-path.md's Major finding. ScanBalances is called
			// from a periodic sweep round (service/onchain.go), so failing
			// this call closed costs a retry on the next cycle, not a
			// correctness gap.
			return nil, fmt.Errorf("evm: scanner: aggregate3 call for %s: success=%v len(return_data)=%d (want 32): %w",
				addr, results[i].Success, len(results[i].ReturnData), ErrBalanceUnreadable)
		}
		raw := new(big.Int).SetBytes(results[i].ReturnData)
		balances[addr] = normalizeAmount(raw, decimals)
	}
	return balances, nil
}

func (s *Scanner) scanConcurrently(ctx context.Context, client *ethclient.Client, token string, addresses []string, decimals int32) (map[string]decimal.Decimal, error) {
	native := token == core.SweepNativeToken
	var tokenAddr common.Address
	if !native {
		if !common.IsHexAddress(token) {
			return nil, fmt.Errorf("evm: scanner: invalid token address %q: %w", token, core.ErrInvalidInput)
		}
		tokenAddr = common.HexToAddress(token)
	}

	balances := make(map[string]decimal.Decimal, len(addresses))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.concurrency)
	for _, addr := range addresses {
		addr := addr
		if !common.IsHexAddress(addr) {
			return nil, fmt.Errorf("evm: scanner: invalid address %q: %w", addr, core.ErrInvalidInput)
		}
		account := common.HexToAddress(addr)
		g.Go(func() error {
			var raw *big.Int
			if native {
				bal, err := client.BalanceAt(gctx, account, nil)
				if err != nil {
					return fmt.Errorf("evm: scanner: native balance %s: %w", addr, err)
				}
				raw = bal
			} else {
				data, err := erc20ABI.Pack("balanceOf", account)
				if err != nil {
					return fmt.Errorf("evm: scanner: pack balanceOf: %w", err)
				}
				out, err := client.CallContract(gctx, ethereum.CallMsg{To: &tokenAddr, Data: data}, nil)
				if err != nil {
					return fmt.Errorf("evm: scanner: balanceOf %s: %w", addr, err)
				}
				raw, err = decodeERC20BalanceOf(addr, out)
				if err != nil {
					return err
				}
			}
			mu.Lock()
			balances[addr] = normalizeAmount(raw, decimals)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return balances, nil
}

// decodeERC20BalanceOf decodes a raw balanceOf(address) return value. A
// pure function (no RPC client) so the fail-closed behavior is directly
// unit-testable -- see TestDecodeERC20BalanceOf_FailsClosedOnMalformedReturn.
// Before this fix, a malformed (non-32-byte) return here silently became a
// zero balance, self-contradicting scanConcurrently's own RPC-error branch
// two lines up the call stack (which already fails closed) -- the same
// onchain-money-path.md Major as the multicall path above.
func decodeERC20BalanceOf(addr string, out []byte) (*big.Int, error) {
	if len(out) != 32 {
		return nil, fmt.Errorf("evm: scanner: balanceOf %s: len(return_data)=%d (want 32): %w",
			addr, len(out), ErrBalanceUnreadable)
	}
	return new(big.Int).SetBytes(out), nil
}
