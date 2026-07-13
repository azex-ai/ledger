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

// GasPrice returns chainID's current suggested gas price, in gwei.
func (s *Sweeper) GasPrice(ctx context.Context, chainID int64) (decimal.Decimal, error) {
	client, err := s.clients.client(chainID)
	if err != nil {
		return decimal.Decimal{}, err
	}
	price, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("evm: sweeper: gas price: chain %d: %w", chainID, err)
	}
	return normalizeAmount(price, weiGweiDecimals), nil
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
func (s *Sweeper) BatchSweep(ctx context.Context, chainID int64, token string, targets []core.SweepTarget, signerNonce uint64) (string, error) {
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

	fee, err := s.quoteFee(ctx, client, chainID, signerNonce)
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

// quoteFee computes this call's (gasFeeCap, gasTipCap), bumping >=12.5% over
// the last fee used for this (chainID, signerNonce) pair if this is a retry.
func (s *Sweeper) quoteFee(ctx context.Context, client interface {
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}, chainID int64, signerNonce uint64) (feeQuote, error) {
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
	// feeCap = 2*baseFee + tip, a conventional headroom so the tx stays
	// includable for a couple of blocks of base fee movement.
	feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	s.mu.Lock()
	prior, ok := s.lastFee[chainID][signerNonce]
	s.mu.Unlock()
	if ok {
		feeCap = maxBig(feeCap, bumpFee(prior.gasFeeCap))
		tip = maxBig(tip, bumpFee(prior.gasTipCap))
	}
	return feeQuote{gasFeeCap: feeCap, gasTipCap: tip}, nil
}

func (s *Sweeper) recordFee(chainID int64, signerNonce uint64, fee feeQuote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFee[chainID] == nil {
		s.lastFee[chainID] = make(map[uint64]feeQuote)
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
