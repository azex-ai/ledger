package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/sync/errgroup"
)

// erc20TransferSig is keccak256("Transfer(address,address,uint256)"), the
// topic0 every ERC-20 Transfer log carries.
var erc20TransferSig = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// defaultAddressShardSize bounds how many "to" addresses go into a single
// eth_getLogs topic filter -- most RPC providers cap the number of distinct
// topic values (or total filter complexity) accepted per call (task
// instructions: "地址列表按 provider 上限分片，默认 500/批可配").
const defaultAddressShardSize = 500

// depositLogClient is the subset of *ethclient.Client the deposit watcher
// needs -- narrowed (like quoteFeeClient in sweeper.go) so FetchDeposits'
// own derivation, above all I-20's TxLogSeq, is unit-testable without a live
// RPC connection. TransactionReceipt is part of it because TxLogSeq is the
// hit log's position *within its transaction's receipt*, which the filtered
// eth_getLogs response alone cannot tell us (see FetchDeposits).
type depositLogClient interface {
	BlockNumber(ctx context.Context) (uint64, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// Reader implements the watcher-facing chain-read port (core.ChainReader)
// against a ClientSet.
type Reader struct {
	clients         *ClientSet
	addressShardLen int
	// rpc resolves one chain's RPC client. NewReader points it at the
	// ClientSet's dialled *ethclient.Client; reader_test.go substitutes a
	// fake through the same seam, so the pins exercise the real
	// FetchDeposits body rather than a re-implementation of it.
	rpc    func(chainID int64) (depositLogClient, error)
	logger core.Logger
}

// ReaderOption configures a Reader at construction.
type ReaderOption func(*Reader)

// WithReaderLogger injects the logger FetchDeposits uses to report logs it
// skipped (an unregistered token, or a log an untrusted RPC response
// mangled). Without it those skips are silent, which is exactly the
// "降级必须落痕" failure working-agreements §3 forbids -- the default is
// core.NopLogger() only because a library cannot pick a consumer's logger
// (see ledger.WithLogger).
func WithReaderLogger(l core.Logger) ReaderOption {
	return func(r *Reader) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewReader builds a Reader over clients. shardLen overrides
// defaultAddressShardSize when > 0 (0 keeps the default), matching the
// "默认 500/批可配" instruction.
func NewReader(clients *ClientSet, shardLen int, opts ...ReaderOption) *Reader {
	if shardLen <= 0 {
		shardLen = defaultAddressShardSize
	}
	r := &Reader{
		clients:         clients,
		addressShardLen: shardLen,
		rpc: func(chainID int64) (depositLogClient, error) {
			return clients.client(chainID)
		},
		logger: core.NopLogger(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

var _ core.ChainReader = (*Reader)(nil)

// LatestBlock returns chainID's current head block number.
func (r *Reader) LatestBlock(ctx context.Context, chainID int64) (int64, error) {
	client, err := r.rpc(chainID)
	if err != nil {
		return 0, err
	}
	head, err := client.BlockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("evm: reader: latest block: chain %d: %w", chainID, err)
	}
	return int64(head), nil
}

// FetchDeposits scans [fromBlock, toBlock] on chainID for ERC-20 Transfer
// logs crediting any of addresses, restricted to the chain's CreditTokens
// allowlist (design doc §3), and returns them normalized into
// core.DepositSighting -- amount divided by the token's configured decimals,
// confirmations computed against the chain's current head, and TxLogSeq set
// to the hit log's zero-based position among ALL logs in its transaction's
// receipt.
//
// That definition is load-bearing and was corrected in remediation G-C2
// (docs/audits/2026-09-02-deep-audit/onchain-money-path.md, Critical #2).
// TxLogSeq used to be the hit's ordinal among the logs THIS call happened to
// return, i.e. a function of the `addresses` filter passed in -- and the two
// ingestion paths pass different filters (the watcher passes every registered
// address, a registration rescan passes exactly one). A transaction crediting
// two registered addresses therefore produced a different TxLogSeq, hence a
// different booking idempotency key, depending on which path saw it first:
// either the same transfer booked twice, or a legitimate one dead-lettered
// forever. Receipt position is independent of any filter, so both paths now
// derive the same key -- and, unlike the chain's block-level log index, it
// survives the same transaction being re-mined at a different position by a
// reorg (the reason I-20 rejected block-level log_index in the first place).
//
// The cost of that correctness is one eth_getTransactionReceipt per
// transaction that actually credited us in this window -- deposits are rare
// relative to blocks scanned. A receipt that cannot be read fails the whole
// call: without it there is no way to derive a stable key, and inventing an
// unstable one is what this fix exists to stop (the caller then leaves its
// cursor where it is and retries -- I-52).
func (r *Reader) FetchDeposits(ctx context.Context, chainID int64, fromBlock, toBlock int64, addresses []string) ([]core.DepositSighting, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	client, err := r.rpc(chainID)
	if err != nil {
		return nil, err
	}
	chainCfg, err := r.clients.chainConfig(chainID)
	if err != nil {
		return nil, err
	}
	if len(chainCfg.CreditTokens) == 0 {
		return nil, nil
	}

	tokenContracts := make([]common.Address, 0, len(chainCfg.CreditTokens))
	for tokenAddr := range chainCfg.CreditTokens {
		tokenContracts = append(tokenContracts, common.HexToAddress(tokenAddr))
	}

	toTopics, err := addressesToTopics(addresses)
	if err != nil {
		return nil, err
	}

	shards := shardTopics(toTopics, r.addressShardLen)
	logsPerShard := make([][]types.Log, len(shards))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, shard := range shards {
		i, shard := i, shard
		g.Go(func() error {
			q := ethereum.FilterQuery{
				FromBlock: big.NewInt(fromBlock),
				ToBlock:   big.NewInt(toBlock),
				Addresses: tokenContracts,
				Topics:    [][]common.Hash{{erc20TransferSig}, nil, shard},
			}
			logs, err := client.FilterLogs(gctx, q)
			if err != nil {
				return fmt.Errorf("evm: reader: fetch deposits: chain %d shard %d: %w", chainID, i, err)
			}
			logsPerShard[i] = logs
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var allLogs []types.Log
	for _, logs := range logsPerShard {
		allLogs = append(allLogs, logs...)
	}
	if len(allLogs) == 0 {
		return nil, nil
	}

	// Sort for a deterministic return order only. TxLogSeq no longer depends
	// on this ordering (it comes from each log's receipt position below), but
	// callers still get sightings in chain order.
	sort.Slice(allLogs, func(i, j int) bool {
		if allLogs[i].BlockNumber != allLogs[j].BlockNumber {
			return allLogs[i].BlockNumber < allLogs[j].BlockNumber
		}
		return allLogs[i].Index < allLogs[j].Index
	})

	type transferHit struct {
		lg     types.Log
		token  core.TokenConfig
		from   common.Address
		to     common.Address
		amount *big.Int
	}
	hits := make([]transferHit, 0, len(allLogs))
	for _, lg := range allLogs {
		tokenCfg, ok := chainCfg.CreditTokens[normalizeTokenKey(lg.Address.Hex())]
		if !ok {
			// Unregistered token -- ignored per design doc §3, but never
			// silently: a token quietly dropped here is a deposit the ledger
			// will never know about (G-m1).
			r.logger.Warn("evm: reader: skipping transfer log",
				"reason", "token_not_in_credit_allowlist", "chain_id", chainID,
				"tx_hash", lg.TxHash.Hex(), "log_index", lg.Index, "token", lg.Address.Hex())
			continue
		}
		from, to, amount, err := decodeTransferLog(lg)
		if err != nil {
			// Malformed log data from an untrusted RPC response -- skip,
			// don't panic, but leave a trace (G-m1).
			r.logger.Warn("evm: reader: skipping transfer log",
				"reason", "malformed_log", "chain_id", chainID,
				"tx_hash", lg.TxHash.Hex(), "log_index", lg.Index, "error", err)
			continue
		}
		hits = append(hits, transferHit{lg: lg, token: tokenCfg, from: from, to: to, amount: amount})
	}
	if len(hits) == 0 {
		return nil, nil
	}

	txHashes := make([]common.Hash, 0, len(hits))
	seen := make(map[common.Hash]struct{}, len(hits))
	for _, h := range hits {
		if _, ok := seen[h.lg.TxHash]; ok {
			continue
		}
		seen[h.lg.TxHash] = struct{}{}
		txHashes = append(txHashes, h.lg.TxHash)
	}
	receiptSeq, err := receiptLogSeqs(ctx, client, chainID, txHashes)
	if err != nil {
		return nil, err
	}

	latest, err := r.LatestBlock(ctx, chainID)
	if err != nil {
		return nil, err
	}

	sightings := make([]core.DepositSighting, 0, len(hits))
	for _, h := range hits {
		seq, ok := receiptSeq[h.lg.TxHash][h.lg.Index]
		if !ok {
			// The receipt we just read does not contain the log the log
			// filter returned: the two RPC responses disagree about this
			// transaction (a reorg mid-scan, or an inconsistent provider
			// backend). Fail closed rather than fall back to a key that is
			// not receipt-derived -- G-C2's whole point is that only one
			// definition of TxLogSeq may ever be used.
			return nil, fmt.Errorf("evm: reader: fetch deposits: chain %d tx %s: log index %d is absent from the transaction receipt, refusing to derive an idempotency key from a disagreeing view: %w",
				chainID, h.lg.TxHash.Hex(), h.lg.Index, core.ErrConflict)
		}
		confirmations := int32(latest - int64(h.lg.BlockNumber) + 1)
		if confirmations < 0 {
			confirmations = 0
		}
		sightings = append(sightings, core.DepositSighting{
			ChainID:       chainID,
			TxHash:        h.lg.TxHash.Hex(),
			TxLogSeq:      seq,
			Token:         h.lg.Address.Hex(),
			From:          h.from.Hex(),
			To:            h.to.Hex(),
			Amount:        normalizeAmount(h.amount, h.token.Decimals),
			Confirmations: confirmations,
			// BlockNumber persists the block this log was mined in, so a
			// later recheck can recompute confirmations without re-scanning
			// (core.DepositSighting's doc comment) -- confirmations above is
			// only valid at this exact moment of observation.
			BlockNumber: int64(h.lg.BlockNumber),
		})
	}
	return sightings, nil
}

// receiptLogSeqs maps, for each transaction in txHashes, every log's
// block-level index to its zero-based position within that transaction's
// receipt -- the filter-independent quantity core.DepositSighting.TxLogSeq is
// defined as (see FetchDeposits). Fails closed: a receipt that cannot be read
// aborts the scan instead of yielding a sighting whose idempotency key would
// depend on which addresses the caller asked about.
func receiptLogSeqs(ctx context.Context, client depositLogClient, chainID int64, txHashes []common.Hash) (map[common.Hash]map[uint]int32, error) {
	out := make(map[common.Hash]map[uint]int32, len(txHashes))
	for _, txHash := range txHashes {
		receipt, err := client.TransactionReceipt(ctx, txHash)
		if err != nil {
			return nil, fmt.Errorf("evm: reader: fetch deposits: chain %d tx %s: receipt: %w", chainID, txHash.Hex(), err)
		}
		if receipt == nil {
			return nil, fmt.Errorf("evm: reader: fetch deposits: chain %d tx %s: receipt: %w", chainID, txHash.Hex(), ethereum.NotFound)
		}
		byIndex := make(map[uint]int32, len(receipt.Logs))
		for i, lg := range receipt.Logs {
			if lg == nil {
				continue
			}
			byIndex[lg.Index] = int32(i)
		}
		out[txHash] = byIndex
	}
	return out, nil
}

// TxIncluded reports whether txHash is still present on canonical chainID --
// used by the manual ReorgPolicy's periodic recheck of confirmed bookings
// (design doc §6).
func (r *Reader) TxIncluded(ctx context.Context, chainID int64, txHash string) (bool, error) {
	client, err := r.rpc(chainID)
	if err != nil {
		return false, err
	}
	_, err = client.TransactionReceipt(ctx, common.HexToHash(txHash))
	if err != nil {
		// errors.Is, not ==: a wrapped sentinel (a retry/transport
		// middleware, a different client implementation) must still read as
		// "not found" rather than falling into the error branch (G-m4).
		if errors.Is(err, ethereum.NotFound) {
			return false, nil
		}
		return false, fmt.Errorf("evm: reader: tx included: chain %d tx %s: %w", chainID, txHash, err)
	}
	return true, nil
}
