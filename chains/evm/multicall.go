package evm

import (
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// multicall3Address is Multicall3's canonical CREATE2 deployment address --
// identical across virtually every EVM chain
// (https://github.com/mds1/multicall, 0xcA11bde05977b3631167028862bE2a173976CA11).
// Scanner probes for code at this address per chain and falls back to
// concurrent single calls when absent (task instructions: "multicall 有则用
// 无则并发单查带信号量").
var multicall3Address = common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")

const multicall3ABIJSON = `[
  {"inputs":[{"components":[{"internalType":"address","name":"target","type":"address"},{"internalType":"bool","name":"allowFailure","type":"bool"},{"internalType":"bytes","name":"callData","type":"bytes"}],"internalType":"struct Multicall3.Call3[]","name":"calls","type":"tuple[]"}],"name":"aggregate3","outputs":[{"components":[{"internalType":"bool","name":"success","type":"bool"},{"internalType":"bytes","name":"returnData","type":"bytes"}],"internalType":"struct Multicall3.Result[]","name":"returnData","type":"tuple[]"}],"stateMutability":"payable","type":"function"},
  {"inputs":[{"internalType":"address","name":"addr","type":"address"}],"name":"getEthBalance","outputs":[{"internalType":"uint256","name":"balance","type":"uint256"}],"stateMutability":"view","type":"function"}
]`

const erc20ABIJSON = `[
  {"constant":true,"inputs":[{"name":"account","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"}
]`

var multicall3ABI = mustParseABI(multicall3ABIJSON)
var erc20ABI = mustParseABI(erc20ABIJSON)

func mustParseABI(raw string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(raw))
	if err != nil {
		panic("evm: invalid embedded ABI json: " + err.Error()) // compile-time-constant input, never a runtime concern
	}
	return parsed
}

// multicall3Call mirrors Multicall3.Call3 -- field names are capitalized
// versions of the ABI tuple's component names so go-ethereum's abi package
// packs it correctly.
type multicall3Call struct {
	Target       common.Address
	AllowFailure bool
	CallData     []byte
}

// multicall3Result mirrors Multicall3.Result.
type multicall3Result struct {
	Success    bool
	ReturnData []byte
}
