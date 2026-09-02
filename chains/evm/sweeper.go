package evm

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/shopspring/decimal"
)

// defaultSweepGasLimit is the fallback gas limit used only if EstimateGas
// itself fails to return a usable value (task instructions: sweep must not
// silently half-fail -- EstimateGas errors are still propagated as errors,
// this constant is not a silent substitute for them, see BatchSweep).
const defaultSweepGasLimit = 500_000

// gasBumpNumerator / gasBumpDenominator encode the required >=12.5% fee
// bump on a same-signerNonce retry (task instructions: "同 nonce 重呼 =
// gas-bump 替换（fee 上浮 ≥12.5%）") -- most nodes' mempool replacement rule
// requires >=10%; the design doc's chosen margin is 12.5% (9/8).
const gasBumpNumerator = 1125
const gasBumpDenominator = 1000

// weiPerGwei normalizes GasPrice's wei-denominated RPC result into the gwei
// decimal.Decimal unit core.SweepPolicy.GasCeiling is configured in.
const weiGweiDecimals = 9

// Sweeper implements core.Sweeper (design doc §4; signature finalized with
// team-lead on bus task #3 -- see core.SweepTarget's doc comment for the
// nonce-vs-address rationale). One Sweeper instance owns exactly one signing
// EOA per the design's "一把 sweeper key 只允许一个部署使用" rule; the caller
// (service/'s sweep job) is responsible for the advisory-lock single-flight
// around calls into it.
type Sweeper struct {
	clients       *ClientSet
	signer        core.Signer
	signerAddress common.Address

	mu      sync.Mutex
	lastFee map[int64]map[uint64]feeQuote // chainID -> signerNonce -> last fee used, for gas-bump retries
}

type feeQuote struct {
	gasFeeCap *big.Int
	gasTipCap *big.Int
}

// NewSweeper builds a Sweeper. signerAddress is the EOA address signer signs
// for (e.g. (*LocalSigner).Address()) -- core.Signer itself has no Address()
// method (so KMS/HSM implementations aren't forced to expose one), so the
// composition root supplies it explicitly alongside the signer.
func NewSweeper(clients *ClientSet, signer core.Signer, signerAddress string) *Sweeper {
	return &Sweeper{
		clients:       clients,
		signer:        signer,
		signerAddress: common.HexToAddress(signerAddress),
		lastFee:       make(map[int64]map[uint64]feeQuote),
	}
}

var _ core.Sweeper = (*Sweeper)(nil)

// NextNonce returns the signer EOA's next usable nonce (pending, so an
// in-flight sweep tx is accounted for) on chainID.
func (s *Sweeper) NextNonce(ctx context.Context, chainID int64) (uint64, error) {
	client, err := s.clients.client(chainID)
	if err != nil {
		return 0, err
	}
	nonce, err := client.PendingNonceAt(ctx, s.signerAddress)
	if err != nil {
		return 0, fmt.Errorf("evm: sweeper: next nonce: chain %d: %w", chainID, err)
	}
	return nonce, nil
}

// GasPrice returns chainID's current fee cap basis, in gwei -- the SAME
// 2*baseFee+tip formula quoteFee uses to compute BatchSweep's non-retry
// gasFeeCap (feeCapBasis below). Before this fix it called SuggestGasPrice,
// a different (legacy, typically ~baseFee+tip) RPC estimate: the ceiling
// check in service/onchain.go compares THIS return value against
// SweepPolicy.GasCeiling, so a lower-tending estimate here meant
// GasCeiling never actually bounded what BatchSweep went on to pay --
// onchain-money-path.md's Minor finding ("GasCeiling 校验的量与实际支付的量
// 不是同一个"). GasCeiling's own doc comment (core.SweepPolicy) promises a
// real upper bound, and only holds for that promise if this function
// reports the quantity that will actually be paid.
func (s *Sweeper) GasPrice(ctx context.Context, chainID int64) (decimal.Decimal, error) {
	client, err := s.clients.client(chainID)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return gasPriceFrom(ctx, client, chainID)
}

// gasPriceFrom is GasPrice's body over the narrowed quoteFeeClient, so the
// unit it returns is directly pinnable without a live RPC connection
// (TestSweeper_GasPrice_IsGweiNotWei). core.SweepPolicy.GasCeiling is
// configured in gwei and compared against this value in
// service/onchain.go's sweep gate; before G-M3 the only statement of that
// unit was a comment on the ceiling's own field, which said wei -- a 10^9
// error in the one gate bounding what a sweep may spend.
func gasPriceFrom(ctx context.Context, client quoteFeeClient, chainID int64) (decimal.Decimal, error) {
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("evm: sweeper: gas price: suggest gas tip cap: chain %d: %w", chainID, err)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("evm: sweeper: gas price: header: chain %d: %w", chainID, err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	return normalizeAmount(feeCapBasis(baseFee, tip), weiGweiDecimals), nil
}

// ReplacementGasPrice reports, in gwei, the gas fee cap a BatchSweep at
// (signerNonce, priorTxHash) would actually bid right now -- i.e. exactly
// what quoteFee below will hand to the transaction, including the >=12.5%
// replacement bump over whatever is still pending. Read-only: it issues the
// same RPC reads quoteFee does and broadcasts nothing.
//
// This is the quantity core.SweepPolicy.GasCeiling has to be compared
// against on the retry path (G-M4). GasPrice reports only the market basis,
// and a replacement bid is max(basis, prior x 1.125): gating a gas-bump on
// the basis let the retry path escalate 1.125^n up a gas spike with the
// ceiling reading as satisfied the whole way, which is precisely the spend
// GasCeiling exists to bound.
func (s *Sweeper) ReplacementGasPrice(ctx context.Context, chainID int64, signerNonce uint64, priorTxHash string) (decimal.Decimal, error) {
	client, err := s.clients.client(chainID)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return s.replacementGasPriceFrom(ctx, client, chainID, signerNonce, priorTxHash)
}

// replacementGasPriceFrom is ReplacementGasPrice's body over the narrowed
// quoteFeeClient, so both the escalation and the gwei unit are pinnable
// without a live RPC connection (TestSweeper_ReplacementGasPrice_*). Same
// seam as gasPriceFrom.
func (s *Sweeper) replacementGasPriceFrom(ctx context.Context, client quoteFeeClient, chainID int64, signerNonce uint64, priorTxHash string) (decimal.Decimal, error) {
	fee, err := s.quoteFee(ctx, client, chainID, signerNonce, priorTxHash)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return normalizeAmount(fee.gasFeeCap, weiGweiDecimals), nil
}

// feeCapBasis is 2*baseFee + tip -- the conventional EIP-1559 headroom (a
// couple of blocks of base-fee movement) both GasPrice and quoteFee use.
// Factored out so the two can never drift back apart the way GasPrice and
// BatchSweep's actual payment did before this fix.
func feeCapBasis(baseFee, tip *big.Int) *big.Int {
	return new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)
}

// BatchSweep signs and submits a DepositFactory.batchSweep (or
// batchSweepNative, when token == core.SweepNativeToken) transaction moving
// every target's balance to the factory's configured treasury.
//
// Every target's Address is re-derived from AccountHolder via
// core.DeriveDepositAddress and must match exactly (case-sensitive EIP-55) --
// a mismatch aborts the whole batch before any signing happens (core.SweepTarget's
// doc comment).
//
// Calling BatchSweep again with the same signerNonce (a stuck-tx retry) is
// treated as a fee-bump replacement: the new tx's fee is
// max(current suggested fee, previous fee * 1.125) per chain gas-bump policy.
// The caller (service/'s sweep job) owns mapping signerNonce -> sweep
// booking idempotency, per design doc §4.
//
// priorTxHash is the hash of the transaction this call is replacing ("" on
// the first-ever dispatch for signerNonce). Pass the most durable hash the
// caller has -- onchain-money-path.md's Major finding is that the previous
// implementation only ever consulted this Sweeper's own in-memory lastFee
// map, which is empty after every process restart, so a restart mid-retry
// could rebroadcast underpriced relative to whatever is genuinely still
// pending on chain and get rejected forever. quoteFee below now reads
// priorTxHash's ACTUAL fee straight from the chain first and only falls
// back to the in-memory map when that lookup is unavailable (empty hash, or
// the chain no longer has that specific hash because an earlier bump this
// same process already replaced it).
func (s *Sweeper) BatchSweep(ctx context.Context, chainID int64, token string, targets []core.SweepTarget, signerNonce uint64, priorTxHash string) (string, error) {
	if len(targets) == 0 {
		return "", fmt.Errorf("evm: sweeper: batch sweep: no targets: %w", core.ErrInvalidInput)
	}
	client, err := s.clients.client(chainID)
	if err != nil {
		return "", err
	}
	chainCfg, err := s.clients.chainConfig(chainID)
	if err != nil {
		return "", err
	}

	nonces := make([]int64, len(targets))
	for i, t := range targets {
		derived, err := core.DeriveDepositAddress(chainCfg.Factory, chainCfg.InitHash, t.AccountHolder)
		if err != nil {
			return "", fmt.Errorf("evm: sweeper: batch sweep: target %d: derive address: %w", i, err)
		}
		if derived != t.Address {
			return "", fmt.Errorf("evm: sweeper: batch sweep: target %d: address %q does not match holder %d's derived address %q, refusing to sweep: %w",
				i, t.Address, t.AccountHolder, derived, core.ErrInvalidInput)
		}
		nonces[i] = t.AccountHolder
	}

	native := token == core.SweepNativeToken
	data, err := packBatchSweep(nonces, token, native)
	if err != nil {
		return "", fmt.Errorf("evm: sweeper: batch sweep: pack calldata: %w", err)
	}
	if chainCfg.Factory == "" {
		return "", fmt.Errorf("evm: sweeper: batch sweep: chain %d has no factory configured: %w", chainID, core.ErrInvalidInput)
	}
	factoryAddr := common.HexToAddress(chainCfg.Factory)

	fee, err := s.quoteFee(ctx, client, chainID, signerNonce, priorTxHash)
	if err != nil {
		return "", err
	}

	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		From:      s.signerAddress,
		To:        &factoryAddr,
		Data:      data,
		GasFeeCap: fee.gasFeeCap,
		GasTipCap: fee.gasTipCap,
	})
	if err != nil {
		return "", fmt.Errorf("evm: sweeper: batch sweep: estimate gas: chain %d: %w", chainID, err)
	}
	if gasLimit == 0 {
		gasLimit = defaultSweepGasLimit
	}

	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     signerNonce,
		GasTipCap: fee.gasTipCap,
		GasFeeCap: fee.gasFeeCap,
		Gas:       gasLimit,
		To:        &factoryAddr,
		Value:     big.NewInt(0),
		Data:      data,
	})
	unsignedBytes, err := EncodeUnsignedTx(unsigned)
	if err != nil {
		return "", fmt.Errorf("evm: sweeper: batch sweep: %w", err)
	}
	signedBytes, err := s.signer.SignTx(ctx, chainID, unsignedBytes)
	if err != nil {
		return "", fmt.Errorf("evm: sweeper: batch sweep: sign: %w", err)
	}
	signedTx, err := DecodeSignedTx(signedBytes)
	if err != nil {
		return "", fmt.Errorf("evm: sweeper: batch sweep: %w", err)
	}
	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return "", fmt.Errorf("evm: sweeper: batch sweep: send: chain %d: %w", chainID, err)
	}

	s.recordFee(chainID, signerNonce, fee)
	return signedTx.Hash().Hex(), nil
}

// quoteFeeClient is the subset of *ethclient.Client quoteFee/priorFeeFloor
// need -- narrowed so tests can fake it without a live RPC connection.
type quoteFeeClient interface {
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	TransactionByHash(ctx context.Context, txHash common.Hash) (*types.Transaction, bool, error)
}

// quoteFee computes this call's (gasFeeCap, gasTipCap), bumping >=12.5% over
// the fee floor for this (chainID, signerNonce) pair if this is a retry --
// see priorFeeFloor for where that floor comes from.
func (s *Sweeper) quoteFee(ctx context.Context, client quoteFeeClient, chainID int64, signerNonce uint64, priorTxHash string) (feeQuote, error) {
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return feeQuote{}, fmt.Errorf("evm: sweeper: suggest gas tip cap: chain %d: %w", chainID, err)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return feeQuote{}, fmt.Errorf("evm: sweeper: header: chain %d: %w", chainID, err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	feeCap := feeCapBasis(baseFee, tip)

	if prior, ok := s.priorFeeFloor(ctx, client, chainID, signerNonce, priorTxHash); ok {
		feeCap = maxBig(feeCap, bumpFee(prior.gasFeeCap))
		tip = maxBig(tip, bumpFee(prior.gasTipCap))
	}
	return feeQuote{gasFeeCap: feeCap, gasTipCap: tip}, nil
}

// priorFeeFloor resolves the fee floor a gas-bump replacement must beat by
// >=12.5% (gasBumpNumerator/gasBumpDenominator). It prefers chain truth --
// the actual fee of the transaction currently sitting at priorTxHash, read
// via TransactionByHash -- over this process's own in-memory lastFee map,
// because the in-memory map is wiped by every restart while priorTxHash
// (sourced by the caller from the booking's durably persisted ChannelRef,
// or from this same process's still-live tracking) survives one. This is
// the fix for onchain-money-path.md's Major finding: before it existed,
// nothing in this package ever called TransactionByHash, so a post-restart
// bump had no way to rebuild a floor high enough to replace whatever was
// genuinely still pending and would fail with "replacement transaction
// underpriced" -- indefinitely, since every subsequent bump inherited the
// same blind spot.
//
// Falls back to the in-memory map when priorTxHash is empty (first-ever
// dispatch for this nonce) or the chain no longer has that specific hash
// (already replaced by an earlier bump this same process performed, so the
// in-memory value IS chain truth for the CURRENT pending tx); returns
// ok=false when neither source has anything, which quoteFee reads as "no
// bump, just use the current market rate" (the correct behavior for a
// first-ever dispatch).
func (s *Sweeper) priorFeeFloor(ctx context.Context, client quoteFeeClient, chainID int64, signerNonce uint64, priorTxHash string) (feeQuote, bool) {
	if priorTxHash != "" {
		if tx, _, err := client.TransactionByHash(ctx, common.HexToHash(priorTxHash)); err == nil && tx != nil && tx.GasFeeCap() != nil {
			return feeQuote{gasFeeCap: tx.GasFeeCap(), gasTipCap: tx.GasTipCap()}, true
		}
		// Not found (already replaced/dropped/mined-then-pruned) or a
		// transport error: this is a best-effort chain-truth lookup, not a
		// hard requirement, so fall through to the in-memory value instead
		// of failing the whole bump attempt over it.
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok := s.lastFee[chainID][signerNonce]
	return prior, ok
}

// recordFee remembers the fee this process just broadcast at (chainID,
// signerNonce) so a later gas-bump for the SAME nonce has a floor to beat
// even when the chain no longer answers for the replaced hash (priorFeeFloor).
//
// It also prunes every entry below signerNonce for that chain: priorFeeFloor
// only ever reads the nonce currently being replaced, and a nonce lower than
// the one we are broadcasting now has necessarily been mined or replaced, so
// its quote can never be read again. Without the prune this map grew one
// entry per sweep broadcast for the life of the process, unbounded and with
// no TTL (concurrency.md Minor, B-m8).
func (s *Sweeper) recordFee(chainID int64, signerNonce uint64, fee feeQuote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFee[chainID] == nil {
		s.lastFee[chainID] = make(map[uint64]feeQuote)
	}
	for n := range s.lastFee[chainID] {
		if n < signerNonce {
			delete(s.lastFee[chainID], n)
		}
	}
	s.lastFee[chainID][signerNonce] = fee
}

// bumpFee returns prior scaled by >=1.125x (gasBumpNumerator/gasBumpDenominator),
// rounded up so the result never falls short of the required bump due to
// integer truncation.
func bumpFee(prior *big.Int) *big.Int {
	if prior == nil {
		return big.NewInt(0)
	}
	num := new(big.Int).Mul(prior, big.NewInt(gasBumpNumerator))
	num.Add(num, big.NewInt(gasBumpDenominator-1))
	return num.Div(num, big.NewInt(gasBumpDenominator))
}

func maxBig(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}
