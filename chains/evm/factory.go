package evm

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// depositFactoryABIJSON is the calldata-packing subset of
// azex-contracts/src/DepositFactory.sol's ABI this adapter needs to build
// sweep transactions. batchSweep/batchSweepNative take `nonces` -- the
// CREATE2 salt each deposit address was derived from, i.e. core's
// account_holder (see core/create2.go's DeriveDepositAddress) -- because the
// factory recomputes each proxy's address on-chain from the nonce; it does
// not accept arbitrary addresses (see sweeper.go's blocked-on-team-lead doc
// comment).
const depositFactoryABIJSON = `[
  {"inputs":[{"internalType":"uint256[]","name":"nonces","type":"uint256[]"},{"internalType":"address","name":"token","type":"address"}],"name":"batchSweep","outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[{"internalType":"uint256[]","name":"nonces","type":"uint256[]"}],"name":"batchSweepNative","outputs":[],"stateMutability":"nonpayable","type":"function"}
]`

var depositFactoryABI = mustParseFactoryABI(depositFactoryABIJSON)

func mustParseFactoryABI(raw string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(raw))
	if err != nil {
		panic("evm: invalid embedded DepositFactory ABI json: " + err.Error())
	}
	return parsed
}

// packBatchSweep ABI-encodes a call to
// DepositFactory.batchSweep(nonces, token) or, when token is
// core.SweepNativeToken, DepositFactory.batchSweepNative(nonces).
func packBatchSweep(nonces []int64, tokenAddress string, native bool) ([]byte, error) {
	bigNonces := make([]*big.Int, len(nonces))
	for i, n := range nonces {
		bigNonces[i] = big.NewInt(n)
	}
	if native {
		return depositFactoryABI.Pack("batchSweepNative", bigNonces)
	}
	return depositFactoryABI.Pack("batchSweep", bigNonces, common.HexToAddress(tokenAddress))
}
