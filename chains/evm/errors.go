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
	// question (see interfaces.go's sweeper doc comment / bus task #3).
	ErrUnsupported = errors.New("evm: unsupported")
)
