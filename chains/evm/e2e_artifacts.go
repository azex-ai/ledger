//go:build e2e

package evm

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

//go:embed testdata/DepositFactory.json
var depositFactoryArtifactJSON []byte

//go:embed testdata/MockUSDT.json
var mockUSDTArtifactJSON []byte

// artifact is the trimmed forge output shape this package's e2e test
// consumes: {"abi": [...], "bytecode": "0x..."}. Full artifacts (built via
// `forge build` against azex-contracts/src/DepositFactory.sol and its
// test/DepositFactory.t.sol MockUSDT) were copied into testdata/ read-only --
// azex-contracts itself is never modified or imported (task instructions).
type artifact struct {
	ABI      abi.ABI
	Bytecode []byte
}

func mustLoadArtifact(raw []byte) artifact {
	var doc struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode string          `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(fmt.Sprintf("evm: e2e: invalid embedded artifact: %v", err))
	}
	parsedABI, err := abi.JSON(strings.NewReader(string(doc.ABI)))
	if err != nil {
		panic(fmt.Sprintf("evm: e2e: invalid embedded artifact ABI: %v", err))
	}
	bytecode, err := hex.DecodeString(strings.TrimPrefix(doc.Bytecode, "0x"))
	if err != nil {
		panic(fmt.Sprintf("evm: e2e: invalid embedded artifact bytecode: %v", err))
	}
	return artifact{ABI: parsedABI, Bytecode: bytecode}
}

func depositFactoryArtifact() artifact { return mustLoadArtifact(depositFactoryArtifactJSON) }
func mockUSDTArtifact() artifact       { return mustLoadArtifact(mockUSDTArtifactJSON) }
