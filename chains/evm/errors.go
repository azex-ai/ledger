package evm

import "errors"

var (
	// ErrChainNotConfigured is returned when a caller asks for a chain ID
	// that was not present in the core.ChainSet / RPC URL map this adapter
	// was constructed with.
	ErrChainNotConfigured = errors.New("evm: chain not configured")
	// ErrTokenNotConfigured is returned when a caller asks to scan/sweep a
	// token that is not present in the chain's CreditTokens/SweepTokens
	// allowlist.
	ErrTokenNotConfigured = errors.New("evm: token not configured")
	// ErrUnsupported marks functionality blocked on an unresolved contract
	// question (see core.Sweeper's doc comment in the root module).
	ErrUnsupported = errors.New("evm: unsupported")
	// ErrBalanceUnreadable is returned by Scanner.ScanBalances when a single
	// address's balance read reverted or returned a malformed result. This is
	// deliberately distinct from a transport-level RPC error (network
	// failure, timeout): it means the call reached the chain and the chain
	// (or a broken/malicious token contract) gave back something that is not
	// a valid balance -- "unreadable", not "zero". Treating it as zero was
	// this scanner's fail-open bug (onchain-money-path.md Major); the fix
	// fails the whole scan closed instead, on both the Multicall3 path and
	// the concurrent fallback path, so which RPC method a chain happens to
	// support no longer changes what a broken read means.
	ErrBalanceUnreadable = errors.New("evm: balance unreadable")
)
