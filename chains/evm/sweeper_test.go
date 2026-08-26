package evm

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQuoteFeeClient satisfies quoteFeeClient without a live RPC connection,
// so quoteFee/priorFeeFloor's fee-floor selection logic can be pinned
// directly.
type fakeQuoteFeeClient struct {
	tip     *big.Int
	baseFee *big.Int

	// txByHash maps a hash (hex string, lowercase) to the transaction
	// TransactionByHash should return for it. A missing entry simulates
	// "not found" (the hash was replaced/dropped, or never existed).
	txByHash map[string]*gethtypes.Transaction
}

func (f *fakeQuoteFeeClient) SuggestGasTipCap(_ context.Context) (*big.Int, error) {
	return f.tip, nil
}

func (f *fakeQuoteFeeClient) HeaderByNumber(_ context.Context, _ *big.Int) (*gethtypes.Header, error) {
	return &gethtypes.Header{BaseFee: f.baseFee}, nil
}

func (f *fakeQuoteFeeClient) TransactionByHash(_ context.Context, txHash common.Hash) (*gethtypes.Transaction, bool, error) {
	tx, ok := f.txByHash[txHash.Hex()]
	if !ok {
		return nil, false, errNotFoundForTest
	}
	return tx, true, nil
}

var errNotFoundForTest = &notFoundError{}

type notFoundError struct{}

func (*notFoundError) Error() string { return "not found" }

func dynamicFeeTx(feeCap, tipCap int64) *gethtypes.Transaction {
	return gethtypes.NewTx(&gethtypes.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		GasFeeCap: big.NewInt(feeCap),
		GasTipCap: big.NewInt(tipCap),
	})
}

// TestSweeper_QuoteFee_PrefersChainTruthOverMemory pins
// onchain-money-path.md's Major finding: when priorTxHash is known and the
// chain still has it, quoteFee must bump off the chain's ACTUAL fee for that
// tx, not this process's in-memory lastFee map -- even when the in-memory
// map disagrees (simulating: another process, or this process after a
// restart, already replaced the tx with a higher fee that this process
// never recorded).
func TestSweeper_QuoteFee_PrefersChainTruthOverMemory(t *testing.T) {
	priorHash := common.HexToHash("0xaaaa000000000000000000000000000000000000000000000000000000aaaa")
	client := &fakeQuoteFeeClient{
		tip:     big.NewInt(1),
		baseFee: big.NewInt(10),
		txByHash: map[string]*gethtypes.Transaction{
			priorHash.Hex(): dynamicFeeTx(1_000_000, 500_000), // chain truth: a much higher fee than memory below
		},
	}

	s := &Sweeper{lastFee: make(map[int64]map[uint64]feeQuote)}
	// In-memory value is stale/lower than chain truth -- simulates this
	// process not being the one that last broadcast at this nonce (or
	// having restarted since).
	s.recordFee(1, 7, feeQuote{gasFeeCap: big.NewInt(100), gasTipCap: big.NewInt(50)})

	fee, err := s.quoteFee(context.Background(), client, 1, 7, priorHash.Hex())
	require.NoError(t, err)

	// Bump must be >=12.5% over the CHAIN's fee (1_000_000 / 500_000), not
	// the stale in-memory one (100 / 50). Before this fix, priorFeeFloor did
	// not exist and quoteFee only ever consulted s.lastFee -- this
	// assertion would have failed against the old code (it would have
	// bumped off 100, landing far below 1_000_000*1.125).
	wantFeeCapFloor := bumpFee(big.NewInt(1_000_000))
	wantTipFloor := bumpFee(big.NewInt(500_000))
	assert.True(t, fee.gasFeeCap.Cmp(wantFeeCapFloor) >= 0,
		"gasFeeCap %s must be >= chain-truth-derived floor %s", fee.gasFeeCap, wantFeeCapFloor)
	assert.True(t, fee.gasTipCap.Cmp(wantTipFloor) >= 0,
		"gasTipCap %s must be >= chain-truth-derived floor %s", fee.gasTipCap, wantTipFloor)
}

// TestSweeper_QuoteFee_FallsBackToMemoryWhenPriorHashUnknown pins the
// non-regression half: when the chain no longer has priorTxHash (already
// replaced by an earlier bump this same process performed -- the common
// case for a second, third, ... bump within one process lifetime), quoteFee
// must fall back to its own in-memory record rather than treating the miss
// as "no prior fee at all" and dropping the bump.
func TestSweeper_QuoteFee_FallsBackToMemoryWhenPriorHashUnknown(t *testing.T) {
	priorHash := common.HexToHash("0xbbbb000000000000000000000000000000000000000000000000000000bbbb")
	client := &fakeQuoteFeeClient{
		tip:      big.NewInt(1),
		baseFee:  big.NewInt(10),
		txByHash: map[string]*gethtypes.Transaction{}, // chain has never heard of this hash
	}

	s := &Sweeper{lastFee: make(map[int64]map[uint64]feeQuote)}
	s.recordFee(1, 7, feeQuote{gasFeeCap: big.NewInt(100), gasTipCap: big.NewInt(50)})

	fee, err := s.quoteFee(context.Background(), client, 1, 7, priorHash.Hex())
	require.NoError(t, err)

	wantFeeCapFloor := bumpFee(big.NewInt(100))
	wantTipFloor := bumpFee(big.NewInt(50))
	assert.True(t, fee.gasFeeCap.Cmp(wantFeeCapFloor) >= 0)
	assert.True(t, fee.gasTipCap.Cmp(wantTipFloor) >= 0)
}

// TestSweeper_QuoteFee_NoPriorMeansNoBump pins the first-ever-dispatch case:
// an empty priorTxHash and an empty in-memory map must produce a plain
// market-rate quote with no bump applied (this is NOT a retry).
func TestSweeper_QuoteFee_NoPriorMeansNoBump(t *testing.T) {
	client := &fakeQuoteFeeClient{tip: big.NewInt(3), baseFee: big.NewInt(20)}
	s := &Sweeper{lastFee: make(map[int64]map[uint64]feeQuote)}

	fee, err := s.quoteFee(context.Background(), client, 1, 7, "")
	require.NoError(t, err)

	// feeCap = 2*baseFee + tip = 2*20 + 3 = 43, tip = 3, no bump applied.
	assert.Equal(t, "43", fee.gasFeeCap.String())
	assert.Equal(t, "3", fee.gasTipCap.String())
}
