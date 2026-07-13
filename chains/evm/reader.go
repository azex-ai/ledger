package evm

import (
	"context"
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

// Reader implements the watcher-facing chain-read port (core.ChainReader)
// against a ClientSet.
type Reader struct {
	clients         *ClientSet
	addressShardLen int
}

// NewReader builds a Reader over clients. shardLen overrides
// defaultAddressShardSize when > 0 (0 keeps the default), matching the
// "默认 500/批可配" instruction.
func NewReader(clients *ClientSet, shardLen int) *Reader {
	if shardLen <= 0 {
		shardLen = defaultAddressShardSize
	}
	return &Reader{clients: clients, addressShardLen: shardLen}
}

var _ core.ChainReader = (*Reader)(nil)

// LatestBlock returns chainID's current head block number.
func (r *Reader) LatestBlock(ctx context.Context, chainID int64) (int64, error) {
	client, err := r.clients.client(chainID)
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
// core.DepositSighting -- amount divided by the token's configured
// decimals, confirmations computed against the chain's current head, and
// TxLogSeq assigned as each hit's ordinal position among the *other hits in
// the same tx* (not the chain's block-level log index -- see
// core.DepositSighting's doc comment on why).
func (r *Reader) FetchDeposits(ctx context.Context, chainID int64, fromBlock, toBlock int64, addresses []string) ([]core.DepositSighting, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	client, err := r.clients.client(chainID)
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

	// Block-level Log.Index is monotonic within a block, so sorting by it
	// reconstructs each tx's internal Transfer emission order even though
	// the value itself is not tx-scoped.
	sort.Slice(allLogs, func(i, j int) bool {
		if allLogs[i].BlockNumber != allLogs[j].BlockNumber {
			return allLogs[i].BlockNumber < allLogs[j].BlockNumber
		}
		return allLogs[i].Index < allLogs[j].Index
	})

	latest, err := r.LatestBlock(ctx, chainID)
	if err != nil {
		return nil, err
	}

	seqByTx := make(map[common.Hash]int32)
	sightings := make([]core.DepositSighting, 0, len(allLogs))
	for _, lg := range allLogs {
		tokenCfg, ok := chainCfg.CreditTokens[normalizeTokenKey(lg.Address.Hex())]
		if !ok {
			continue // unregistered token -- ignore per design doc §3
		}
		from, to, amount, err := decodeTransferLog(lg)
		if err != nil {
			continue // malformed log data from an untrusted RPC response -- skip, don't panic
		}
		seq := seqByTx[lg.TxHash]
		seqByTx[lg.TxHash] = seq + 1

		confirmations := int32(latest - int64(lg.BlockNumber) + 1)
		if confirmations < 0 {
			confirmations = 0
		}
		sightings = append(sightings, core.DepositSighting{
			ChainID:       chainID,
			TxHash:        lg.TxHash.Hex(),
			TxLogSeq:      seq,
			Token:         lg.Address.Hex(),
			From:          from.Hex(),
			To:            to.Hex(),
			Amount:        normalizeAmount(amount, tokenCfg.Decimals),
			Confirmations: confirmations,
			// BlockNumber persists the block this log was mined in, so a
			// later recheck can recompute confirmations without re-scanning
			// (core.DepositSighting's doc comment) -- confirmations above is
			// only valid at this exact moment of observation.
			BlockNumber: int64(lg.BlockNumber),
		})
	}
	return sightings, nil
}

// TxIncluded reports whether txHash is still present on canonical chainID --
// used by the manual ReorgPolicy's periodic recheck of confirmed bookings
// (design doc §6).
func (r *Reader) TxIncluded(ctx context.Context, chainID int64, txHash string) (bool, error) {
	client, err := r.clients.client(chainID)
	if err != nil {
		return false, err
	}
	_, err = client.TransactionReceipt(ctx, common.HexToHash(txHash))
	if err != nil {
		if err == ethereum.NotFound {
			return false, nil
		}
		return false, fmt.Errorf("evm: reader: tx included: chain %d tx %s: %w", chainID, txHash, err)
	}
	return true, nil
}
