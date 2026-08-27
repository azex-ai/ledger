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
// addresses, on chainID, that could actually be read this round.
// unreadable lists the rest -- see core.ChainScanner's doc comment for the
// fail-closed-per-address contract this implements (m-10,
// `.local/independent-review-2026-08-26.md`). token is either a contract
// address or core.SweepNativeToken.
func (s *Scanner) ScanBalances(ctx context.Context, chainID int64, token string, addresses []string) (map[string]decimal.Decimal, []string, error) {
	if len(addresses) == 0 {
		return map[string]decimal.Decimal{}, nil, nil
	}
	client, err := s.clients.client(chainID)
	if err != nil {
		return nil, nil, err
	}
	decimals, err := s.tokenDecimals(chainID, token)
	if err != nil {
		return nil, nil, err
	}

	hasMulticall, err := s.probeMulticall(ctx, client, chainID)
	if err != nil {
		return nil, nil, err
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

func (s *Scanner) scanViaMulticall(ctx context.Context, client *ethclient.Client, token string, addresses []string, decimals int32) (map[string]decimal.Decimal, []string, error) {
	calls := make([]multicall3Call, len(addresses))
	native := token == core.SweepNativeToken
	for i, addr := range addresses {
		if !common.IsHexAddress(addr) {
			return nil, nil, fmt.Errorf("evm: scanner: invalid address %q: %w", addr, core.ErrInvalidInput)
		}
		account := common.HexToAddress(addr)
		if native {
			data, err := multicall3ABI.Pack("getEthBalance", account)
			if err != nil {
				return nil, nil, fmt.Errorf("evm: scanner: pack getEthBalance: %w", err)
			}
			calls[i] = multicall3Call{Target: multicall3Address, AllowFailure: true, CallData: data}
		} else {
			data, err := erc20ABI.Pack("balanceOf", account)
			if err != nil {
				return nil, nil, fmt.Errorf("evm: scanner: pack balanceOf: %w", err)
			}
			calls[i] = multicall3Call{Target: common.HexToAddress(token), AllowFailure: true, CallData: data}
		}
	}

	packed, err := multicall3ABI.Pack("aggregate3", calls)
	if err != nil {
		return nil, nil, fmt.Errorf("evm: scanner: pack aggregate3: %w", err)
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &multicall3Address, Data: packed}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("evm: scanner: aggregate3 call: %w", err)
	}
	var results []multicall3Result
	if err := multicall3ABI.UnpackIntoInterface(&results, "aggregate3", out); err != nil {
		return nil, nil, fmt.Errorf("evm: scanner: unpack aggregate3: %w", err)
	}
	if len(results) != len(addresses) {
		return nil, nil, fmt.Errorf("evm: scanner: aggregate3 returned %d results for %d addresses", len(results), len(addresses))
	}
	balances, unreadable := multicallResultsToBalances(addresses, results, decimals)
	return balances, unreadable, nil
}

// multicallResultsToBalances translates an already-unpacked
// Multicall3.aggregate3 response into a balance map plus the addresses that
// could not be read. Split out of scanViaMulticall as a pure function (no
// RPC client involved) so the fail-closed behavior below is directly
// unit-testable -- see TestMulticallResultsToBalances_FailsClosedPerAddress.
//
// m-10 (2026-08-26 independent review, third pass) changed this from
// returning an error (which failed the ENTIRE batch closed the moment any
// one address reverted or returned a malformed length) to returning that
// address in unreadable and continuing with the rest. The original
// all-or-nothing form traded one fail-open bug (unreadable treated as zero)
// for a different fail-closed-too-coarsely bug: a single unreadable address
// -- a broken/malicious token contract, or one flaky node response among
// many -- discarded every OTHER, perfectly readable address from that
// round's sweep-eligible set too. Per-address fail-closed keeps both
// properties I-41 point 4 requires (unreadable is never silently treated as
// zero; a failure is never silently dropped with no signal -- callers must
// surface unreadable, see core.ChainScanner's doc comment) without that
// unrelated cost.
func multicallResultsToBalances(addresses []string, results []multicall3Result, decimals int32) (balances map[string]decimal.Decimal, unreadable []string) {
	balances = make(map[string]decimal.Decimal, len(addresses))
	for i, addr := range addresses {
		if !results[i].Success || len(results[i].ReturnData) != 32 {
			// A reverted call or a malformed return length is not the same
			// fact as "balance is zero" (a broken/malicious token contract,
			// or a transient node issue, can produce exactly this shape) --
			// exclude it from balances rather than defaulting to zero, but
			// let every other address in this batch proceed.
			unreadable = append(unreadable, addr)
			continue
		}
		raw := new(big.Int).SetBytes(results[i].ReturnData)
		balances[addr] = normalizeAmount(raw, decimals)
	}
	return balances, unreadable
}

// scanConcurrently is the non-Multicall3 fallback path. m-10 (2026-08-26
// independent review, third pass): a per-address RPC failure (BalanceAt /
// CallContract erroring, or decodeERC20BalanceOf rejecting a malformed
// return) used to be returned from the goroutine, which errgroup treats as
// "abort the whole group" -- cancelling gctx out from under every other
// still-in-flight address and failing the entire scan for one address's
// problem. Per-address failures are now recorded into unreadable and the
// goroutine returns nil, so errgroup never cancels its siblings over them;
// g.Wait() only ever returns a non-nil error for something that legitimately
// invalidates the whole batch (the parent ctx itself being cancelled --
// still observed via gctx inside each call -- propagates the same way it
// always did, since every RPC call below still takes gctx).
func (s *Scanner) scanConcurrently(ctx context.Context, client *ethclient.Client, token string, addresses []string, decimals int32) (map[string]decimal.Decimal, []string, error) {
	native := token == core.SweepNativeToken
	var tokenAddr common.Address
	if !native {
		if !common.IsHexAddress(token) {
			return nil, nil, fmt.Errorf("evm: scanner: invalid token address %q: %w", token, core.ErrInvalidInput)
		}
		tokenAddr = common.HexToAddress(token)
	}

	balances := make(map[string]decimal.Decimal, len(addresses))
	var unreadable []string
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.concurrency)
	for _, addr := range addresses {
		addr := addr
		if !common.IsHexAddress(addr) {
			return nil, nil, fmt.Errorf("evm: scanner: invalid address %q: %w", addr, core.ErrInvalidInput)
		}
		account := common.HexToAddress(addr)
		g.Go(func() error {
			var raw *big.Int
			var readErr error
			if native {
				bal, err := client.BalanceAt(gctx, account, nil)
				if err != nil {
					readErr = fmt.Errorf("evm: scanner: native balance %s: %w", addr, err)
				} else {
					raw = bal
				}
			} else {
				data, err := erc20ABI.Pack("balanceOf", account)
				if err != nil {
					// Packing failure depends only on the fixed ABI
					// signature and a well-formed account, not on this
					// address's chain state -- it would fail identically
					// for every address in the batch, so it is a genuine
					// whole-batch error, not a per-address read failure.
					return fmt.Errorf("evm: scanner: pack balanceOf: %w", err)
				}
				out, err := client.CallContract(gctx, ethereum.CallMsg{To: &tokenAddr, Data: data}, nil)
				if err != nil {
					readErr = fmt.Errorf("evm: scanner: balanceOf %s: %w", addr, err)
				} else {
					raw, readErr = decodeERC20BalanceOf(addr, out)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if readErr != nil {
				unreadable = append(unreadable, addr)
				return nil
			}
			balances[addr] = normalizeAmount(raw, decimals)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return balances, unreadable, nil
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
