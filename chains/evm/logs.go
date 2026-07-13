package evm

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/shopspring/decimal"
)

// normalizeTokenKey matches core.ChainConfig.CreditTokens/SweepTokens'
// documented convention: map keys are lowercase token contract addresses.
func normalizeTokenKey(address string) string {
	return strings.ToLower(address)
}

// addressesToTopics converts EIP-55 deposit addresses into the 32-byte
// topic form eth_getLogs expects for an indexed `address` event parameter
// (left-padded with zero bytes).
func addressesToTopics(addresses []string) ([]common.Hash, error) {
	topics := make([]common.Hash, 0, len(addresses))
	for _, addr := range addresses {
		if !common.IsHexAddress(addr) {
			return nil, fmt.Errorf("evm: reader: invalid address %q: %w", addr, core.ErrInvalidInput)
		}
		topics = append(topics, common.BytesToHash(common.HexToAddress(addr).Bytes()))
	}
	return topics, nil
}

// shardTopics splits topics into chunks of at most shardLen, so a single
// eth_getLogs call never exceeds a provider's per-request topic-value cap.
func shardTopics(topics []common.Hash, shardLen int) [][]common.Hash {
	if len(topics) == 0 {
		return nil
	}
	var shards [][]common.Hash
	for i := 0; i < len(topics); i += shardLen {
		end := i + shardLen
		if end > len(topics) {
			end = len(topics)
		}
		shards = append(shards, topics[i:end])
	}
	return shards
}

// decodeTransferLog parses an ERC-20 Transfer(address indexed from, address
// indexed to, uint256 value) log. The log is untrusted RPC-provided input --
// malformed topics/data return an error instead of panicking.
func decodeTransferLog(lg types.Log) (from, to common.Address, amount *big.Int, err error) {
	if len(lg.Topics) != 3 {
		return common.Address{}, common.Address{}, nil, fmt.Errorf("evm: decode transfer log: want 3 topics, got %d", len(lg.Topics))
	}
	if len(lg.Data) != 32 {
		return common.Address{}, common.Address{}, nil, fmt.Errorf("evm: decode transfer log: want 32 data bytes, got %d", len(lg.Data))
	}
	from = common.HexToAddress(lg.Topics[1].Hex())
	to = common.HexToAddress(lg.Topics[2].Hex())
	amount = new(big.Int).SetBytes(lg.Data)
	return from, to, amount, nil
}

// normalizeAmount converts a raw on-chain integer amount into a
// decimal.Decimal using the token's configured decimals (design doc §3:
// "adapter 边界归一为 decimal.Decimal").
func normalizeAmount(raw *big.Int, decimals int32) decimal.Decimal {
	return decimal.NewFromBigInt(raw, -decimals)
}
