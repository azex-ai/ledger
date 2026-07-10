package evm

import (
	"context"

	"github.com/azex-ai/ledger/core"
	"github.com/shopspring/decimal"
)

// TODO(merge): these mirror the core ports dev-service is landing in
// core/interfaces.go (see .team/context Team Lead message on bus task #3).
// They exist here only so this module compiles a var-assertion against the
// exact signatures it must satisfy without depending on the root module's
// not-yet-merged interfaces.go changes. Delete this file and replace the
// `var _ chainReader = ...` etc. assertions in client_reader.go / sweeper.go
// / scanner.go / signer.go with `var _ core.ChainReader = ...` (etc.) once
// core/interfaces.go on the merge target actually has them.
//
// chainScanner and signer already exist in core/interfaces.go as
// core.ChainScanner / core.Signer (Foundation, contract §5) -- those two are
// asserted directly against core.*, not mirrored here.

// chainReader mirrors the not-yet-merged core.ChainReader (watcher-facing
// port: latest head, log-scan a block range for registered addresses, and
// check whether a previously seen tx is still canonical -- design doc §3/§6).
type chainReader interface {
	LatestBlock(ctx context.Context, chainID int64) (int64, error)
	FetchDeposits(ctx context.Context, chainID int64, fromBlock, toBlock int64, addresses []string) ([]core.DepositSighting, error)
	TxIncluded(ctx context.Context, chainID int64, txHash string) (bool, error)
}

// SweepTarget pairs a registered deposit address with the account_holder id
// it was derived from (= the CREATE2 salt / DepositFactory nonce). Resolved
// per bus task #3's blocked/unblocked exchange with team-lead (option A):
// DepositFactory.batchSweep/batchSweepNative only accept nonces and compute
// each proxy's address on-chain, so BatchSweep needs the holder id, not just
// the address. Sweeper.BatchSweep re-derives Address from AccountHolder via
// core.DeriveDepositAddress and rejects (does not sign or send) any target
// whose Address does not match -- a mismatch here means the caller passed a
// stale/wrong registry row, and signing anyway would sweep the wrong proxy.
type SweepTarget struct {
	Address       string
	AccountHolder int64
}

// sweeper mirrors the not-yet-merged core.Sweeper (nonce persisted +
// gas-bump capable BatchSweep -- design doc §4).
type sweeper interface {
	NextNonce(ctx context.Context, chainID int64) (uint64, error)
	BatchSweep(ctx context.Context, chainID int64, token string, targets []SweepTarget, signerNonce uint64) (txHash string, err error)
	GasPrice(ctx context.Context, chainID int64) (decimal.Decimal, error)
}
