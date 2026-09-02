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
	// ErrBalanceUnreadable is wrapped into an entry's read error inside
	// Scanner.ScanBalances when a single address's balance read reverted or
	// returned a malformed result -- that address then lands in
	// ScanBalances' unreadable return, not balances. This is deliberately
	// distinct from a transport-level RPC error (network failure, timeout):
	// it means the call reached the chain and the chain (or a
	// broken/malicious token contract) gave back something that is not a
	// valid balance -- "unreadable", not "zero". Treating it as zero was
	// this scanner's original fail-open bug (onchain-money-path.md Major);
	// m-10 (2026-08-26 independent review, third pass) corrected the first
	// fix's own overcorrection -- failing THAT address closed (excluded from
	// balances, reported in unreadable) rather than failing the entire scan
	// closed over one address, on both the Multicall3 path and the
	// concurrent fallback path, so which RPC method a chain happens to
	// support no longer changes what a broken read means, and no longer
	// changes how many OTHER addresses pay for it.
	ErrBalanceUnreadable = errors.New("evm: balance unreadable")
	// ErrTokenDecimalsMismatch is returned by ClientSet.VerifyTokenDecimals
	// when a configured core.TokenConfig.Decimals disagrees with the token
	// contract's own decimals() -- the configuration would credit every
	// deposit of that token at the wrong order of magnitude (G-M7).
	ErrTokenDecimalsMismatch = errors.New("evm: token decimals mismatch")
	// ErrTokenDecimalsUnreadable is returned by
	// ClientSet.VerifyTokenDecimals when a token's decimals() could not be
	// read at all (RPC failure, the address is not an ERC-20, a proxy that
	// reverts). Deliberately distinct from ErrTokenDecimalsMismatch: "we
	// could not check" is not "we checked and it disagrees", and a consumer
	// may reasonably treat the two differently -- but neither is silently
	// ignored inside this package (working-agreements §3: 未运行 ≠ 通过).
	ErrTokenDecimalsUnreadable = errors.New("evm: token decimals unreadable")
)
