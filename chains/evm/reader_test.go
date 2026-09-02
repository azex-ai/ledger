package evm

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes -----------------------------------------------------------------

// fakeDepositLogClient satisfies depositLogClient without a live RPC
// connection. FilterLogs reproduces the one property of eth_getLogs these
// tests turn on: the response is filtered by the `to` address topic the
// caller asked for, so the watcher (all registered addresses) and a
// registration rescan (exactly one) genuinely see different subsets of the
// same block.
type fakeDepositLogClient struct {
	head     uint64
	logs     []gethtypes.Log
	receipts map[common.Hash]*gethtypes.Receipt
	// receiptErr, when set, is returned by TransactionReceipt for every hash.
	receiptErr error

	mu             sync.Mutex
	receiptCalls   int
	filterLogsSeen int
}

func (f *fakeDepositLogClient) BlockNumber(context.Context) (uint64, error) { return f.head, nil }

func (f *fakeDepositLogClient) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]gethtypes.Log, error) {
	f.mu.Lock()
	f.filterLogsSeen++
	f.mu.Unlock()

	tokens := make(map[common.Address]bool, len(q.Addresses))
	for _, a := range q.Addresses {
		tokens[a] = true
	}
	var wantTo map[common.Hash]bool
	if len(q.Topics) > 2 && len(q.Topics[2]) > 0 {
		wantTo = make(map[common.Hash]bool, len(q.Topics[2]))
		for _, t := range q.Topics[2] {
			wantTo[t] = true
		}
	}

	var out []gethtypes.Log
	for _, lg := range f.logs {
		if len(tokens) > 0 && !tokens[lg.Address] {
			continue
		}
		if len(q.Topics) > 0 && len(q.Topics[0]) > 0 {
			if len(lg.Topics) == 0 || lg.Topics[0] != q.Topics[0][0] {
				continue
			}
		}
		if wantTo != nil {
			if len(lg.Topics) < 3 || !wantTo[lg.Topics[2]] {
				continue
			}
		}
		out = append(out, lg)
	}
	return out, nil
}

func (f *fakeDepositLogClient) TransactionReceipt(_ context.Context, txHash common.Hash) (*gethtypes.Receipt, error) {
	f.mu.Lock()
	f.receiptCalls++
	f.mu.Unlock()
	if f.receiptErr != nil {
		return nil, f.receiptErr
	}
	r, ok := f.receipts[txHash]
	if !ok {
		return nil, ethereum.NotFound
	}
	return r, nil
}

// recordingLogger captures Warn/Error calls so a "skipped silently" claim can
// be asserted against, not just believed.
type recordingLogger struct {
	core.Logger
	mu    sync.Mutex
	warns []string
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{Logger: core.NopLogger()}
}

func (l *recordingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprint(append([]any{msg}, args...)...))
}

func (l *recordingLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

// testReader builds a Reader over a fake RPC client through the same seam
// NewReader uses, so these tests exercise the real FetchDeposits body.
func testReader(chains core.ChainSet, client depositLogClient, logger core.Logger) *Reader {
	if logger == nil {
		logger = core.NopLogger()
	}
	return &Reader{
		clients:         &ClientSet{chains: chains},
		addressShardLen: defaultAddressShardSize,
		rpc:             func(int64) (depositLogClient, error) { return client, nil },
		logger:          logger,
	}
}

const testTokenAddr = "0xdac17f958d2ee523a2206206994597c13d831ec7"

func testChainSet(chainID int64, decimals int32) core.ChainSet {
	return core.ChainSet{
		chainID: {
			ChainID:       chainID,
			Confirmations: 3,
			CreditTokens: map[string]core.TokenConfig{
				testTokenAddr: {TokenAddress: testTokenAddr, CurrencyCode: "USDT", Decimals: decimals},
			},
		},
	}
}

func transferLog(token common.Address, txHash common.Hash, blockNumber uint64, logIndex uint, from, to common.Address, amount int64) gethtypes.Log {
	data := make([]byte, 32)
	big.NewInt(amount).FillBytes(data)
	return gethtypes.Log{
		Address:     token,
		Topics:      []common.Hash{erc20TransferSig, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())},
		Data:        data,
		BlockNumber: blockNumber,
		TxHash:      txHash,
		Index:       logIndex,
	}
}

// --- G-C2 ------------------------------------------------------------------

// TestReader_FetchDeposits_TxLogSeqIsIndependentOfAddressFilter pins G-C2
// (docs/audits/2026-09-02-deep-audit/onchain-money-path.md Critical #2), and
// pins it through Reader.FetchDeposits itself -- I-20's three previous pins
// went through the store, a hand-fed sighting and the webhook parser, so the
// watcher's own derivation had no gate at all.
//
// One transaction credits two registered addresses. The watcher asks about
// both; a registration rescan asks about one. TxLogSeq for a given transfer
// must not change between those two calls, because the deposit booking's
// idempotency key is derived from it: before this fix addrB's transfer was
// seq 1 to the watcher and seq 0 to the rescan, so the same on-chain
// transfer either booked twice or dead-lettered a legitimate deposit
// forever.
func TestReader_FetchDeposits_TxLogSeqIsIndependentOfAddressFilter(t *testing.T) {
	const chainID = int64(1)
	token := common.HexToAddress(testTokenAddr)
	txHash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	sender := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	addrA := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	addrB := common.HexToAddress("0x00000000000000000000000000000000000000b2")

	logA := transferLog(token, txHash, 100, 5, sender, addrA, 100)
	// An unrelated event sits between the two transfers IN THE RECEIPT. It
	// never appears in a filtered eth_getLogs response, which is exactly why
	// "position among the logs I got back" and "position within the
	// transaction" are different numbers.
	unrelated := gethtypes.Log{
		Address:     common.HexToAddress("0x00000000000000000000000000000000000000cc"),
		Topics:      []common.Hash{common.HexToHash("0xdeadbeef")},
		BlockNumber: 100, TxHash: txHash, Index: 6,
	}
	logB := transferLog(token, txHash, 100, 7, sender, addrB, 200)

	client := &fakeDepositLogClient{
		head: 110,
		logs: []gethtypes.Log{logA, logB},
		receipts: map[common.Hash]*gethtypes.Receipt{
			txHash: {TxHash: txHash, Logs: []*gethtypes.Log{&logA, &unrelated, &logB}},
		},
	}
	r := testReader(testChainSet(chainID, 6), client, nil)
	ctx := context.Background()

	// Watcher: every registered address in one call.
	both, err := r.FetchDeposits(ctx, chainID, 100, 100, []string{addrA.Hex(), addrB.Hex()})
	require.NoError(t, err)
	require.Len(t, both, 2)

	// Registration rescan: exactly one address (service/onchain.go's
	// processRegistrationRescan passes []string{job.Address}).
	single, err := r.FetchDeposits(ctx, chainID, 100, 100, []string{addrB.Hex()})
	require.NoError(t, err)
	require.Len(t, single, 1)

	var bFromWatcher core.DepositSighting
	for _, s := range both {
		if s.To == addrB.Hex() {
			bFromWatcher = s
		}
	}
	require.Equal(t, addrB.Hex(), bFromWatcher.To)

	assert.Equal(t, bFromWatcher.TxLogSeq, single[0].TxLogSeq,
		"the same transfer must derive the same TxLogSeq no matter which addresses the caller asked about -- the booking idempotency key is built from it")
	// And the value itself is the receipt position (2), not the
	// filtered-result position (1) -- so it is stated, not merely consistent.
	assert.Equal(t, int32(2), single[0].TxLogSeq)
	assert.Equal(t, int32(0), both[0].TxLogSeq, "addrA's transfer is the transaction's first log")

	// One receipt fetch per hit transaction per call, not per log.
	assert.Equal(t, 2, client.receiptCalls)
}

// TestReader_FetchDeposits_SkippedLogsLeaveATraceAndDoNotShiftSeq pins G-m1:
// a log dropped because its token is not on the allowlist, or because an
// untrusted RPC response mangled it, must (a) leave a trace and (b) not move
// any other log's TxLogSeq. Before the fix both drops were bare `continue`s
// ahead of the seq counter, so a log that decoded today and failed tomorrow
// silently renumbered every later transfer in its transaction.
func TestReader_FetchDeposits_SkippedLogsLeaveATraceAndDoNotShiftSeq(t *testing.T) {
	const chainID = int64(1)
	token := common.HexToAddress(testTokenAddr)
	otherToken := common.HexToAddress("0x00000000000000000000000000000000000000ee")
	txHash := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	sender := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	addrB := common.HexToAddress("0x00000000000000000000000000000000000000b2")

	malformed := transferLog(token, txHash, 200, 0, sender, addrB, 1)
	malformed.Data = []byte{0x01} // not 32 bytes -> decodeTransferLog rejects it
	unlisted := transferLog(otherToken, txHash, 200, 1, sender, addrB, 2)
	good := transferLog(token, txHash, 200, 2, sender, addrB, 300)

	client := &fakeDepositLogClient{
		head: 210,
		logs: []gethtypes.Log{malformed, unlisted, good},
		receipts: map[common.Hash]*gethtypes.Receipt{
			txHash: {TxHash: txHash, Logs: []*gethtypes.Log{&malformed, &unlisted, &good}},
		},
	}
	logger := newRecordingLogger()
	// The unlisted token's log only reaches the skip branch if the filter
	// lets it through, so widen the fake's token filter by asking for it.
	r := testReader(testChainSet(chainID, 6), client, logger)
	r.clients.chains[chainID].CreditTokens[testTokenAddr] = core.TokenConfig{
		TokenAddress: testTokenAddr, CurrencyCode: "USDT", Decimals: 6,
	}

	sightings, err := r.FetchDeposits(context.Background(), chainID, 200, 200, []string{addrB.Hex()})
	require.NoError(t, err)
	require.Len(t, sightings, 1, "only the well-formed, allowlisted transfer becomes a sighting")

	assert.Equal(t, int32(2), sightings[0].TxLogSeq,
		"the surviving transfer keeps its receipt position: skipped logs must not renumber it")
	assert.GreaterOrEqual(t, logger.warnCount(), 1, "a dropped log must leave a trace (working-agreements §3)")
}

// TestReader_FetchDeposits_UnreadableReceiptFailsClosed pins the fail-closed
// half of G-C2: TxLogSeq cannot be derived without the receipt, and inventing
// a filter-dependent substitute is the bug being fixed. The scan must fail so
// the caller leaves its cursor put and retries (I-52).
func TestReader_FetchDeposits_UnreadableReceiptFailsClosed(t *testing.T) {
	const chainID = int64(1)
	token := common.HexToAddress(testTokenAddr)
	txHash := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	sender := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	addrA := common.HexToAddress("0x00000000000000000000000000000000000000a1")

	lg := transferLog(token, txHash, 300, 0, sender, addrA, 500)
	client := &fakeDepositLogClient{
		head:       310,
		logs:       []gethtypes.Log{lg},
		receiptErr: fmt.Errorf("rpc: connection reset"),
	}
	r := testReader(testChainSet(chainID, 6), client, nil)

	sightings, err := r.FetchDeposits(context.Background(), chainID, 300, 300, []string{addrA.Hex()})
	require.Error(t, err)
	assert.Nil(t, sightings, "no sighting may be produced from a transaction whose receipt we could not read")
}

// --- G-m4 ------------------------------------------------------------------

// TestReader_TxIncluded_TreatsWrappedNotFoundAsNotIncluded pins G-m4: the
// sentinel comparison must be errors.Is, so a wrapped ethereum.NotFound (any
// retry/transport middleware, any non-ethclient implementation) still reads
// as "not on chain" instead of falling into the error branch.
func TestReader_TxIncluded_TreatsWrappedNotFoundAsNotIncluded(t *testing.T) {
	client := &fakeDepositLogClient{receiptErr: fmt.Errorf("rpc middleware: %w", ethereum.NotFound)}
	r := testReader(testChainSet(1, 6), client, nil)

	included, err := r.TxIncluded(context.Background(), 1, "0x4444444444444444444444444444444444444444444444444444444444444444")
	require.NoError(t, err)
	assert.False(t, included)
}

// TestNewReader_AppliesTheLoggerOption pins the wiring of
// WithReaderLogger itself, not just the Warn calls it feeds. An option that
// is accepted and then never applied is the single most-repeated finding of
// the 2026-09-02 audit on this repository (F-M1, I-R1): the skip traces
// above would go back to being silent with nothing else changing.
func TestNewReader_AppliesTheLoggerOption(t *testing.T) {
	logger := newRecordingLogger()
	r := NewReader(&ClientSet{chains: testChainSet(1, 6)}, 0, WithReaderLogger(logger))
	got, ok := r.logger.(*recordingLogger)
	require.True(t, ok, "WithReaderLogger must actually reach the Reader (got %T)", r.logger)
	require.Same(t, logger, got)

	// A nil logger must not blank out the default -- FetchDeposits calls
	// Warn unconditionally on a skip, so a nil field would panic.
	r = NewReader(&ClientSet{chains: testChainSet(1, 6)}, 0, WithReaderLogger(nil))
	require.NotNil(t, r.logger)
}
