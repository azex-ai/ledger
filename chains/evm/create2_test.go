package evm

import (
	"math/big"
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestCreate2VectorsCrossValidateAgainstGoEthereum reproduces the exact
// test vectors pinned in .team/context/foundation-contract.md §4 (in turn
// cast-verified against `cast create2` / `cast to-check-sum-address`), but
// computes the CREATE2 address independently via go-ethereum's own
// crypto.CreateAddress2 instead of core.DeriveDepositAddress's manual
// keccak construction. Agreement between the two independent
// implementations is the cross-check design doc §7 calls for ("对拍同一组
// vector") -- core and chains/evm must never silently drift on this
// formula, since a mismatch means the watcher/scanner/sweeper would target
// a different address than the one actually registered.
func TestCreate2VectorsCrossValidateAgainstGoEthereum(t *testing.T) {
	const factory = "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323"
	const initHash = "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece"

	cases := []struct {
		holder int64
		want   string
	}{
		{holder: 1, want: "0xB3e7eA5de7C24b4e89b1AC454f02a42DBAE0BFc0"},
		{holder: 2, want: "0x550427E1afB2EbD4889031DCE4801D993017d7eC"},
		{holder: 1001, want: "0x8eaA0E854b1ff86188774AA71AeCfDFFc0f93F67"},
	}

	for _, tc := range cases {
		got := goEthereumCreate2(t, factory, initHash, tc.holder)
		if got != tc.want {
			t.Errorf("holder=%d: go-ethereum CreateAddress2 = %s, want %s (pinned vector)", tc.holder, got, tc.want)
		}

		// Same vector, computed via core.DeriveDepositAddress (the
		// production code path) -- must agree byte-for-byte with the
		// independent go-ethereum computation above.
		coreGot, err := core.DeriveDepositAddress(factory, initHash, tc.holder)
		if err != nil {
			t.Fatalf("holder=%d: core.DeriveDepositAddress: %v", tc.holder, err)
		}
		if coreGot != tc.want {
			t.Errorf("holder=%d: core.DeriveDepositAddress = %s, want %s", tc.holder, coreGot, tc.want)
		}
	}
}

// goEthereumCreate2 independently computes the CREATE2 address using
// go-ethereum's crypto.CreateAddress2, with the same salt convention as
// core.DeriveDepositAddress: salt = bytes32(holder), big-endian.
func goEthereumCreate2(t *testing.T, factoryHex, initHashHex string, holder int64) string {
	t.Helper()
	factory := common.HexToAddress(factoryHex)
	initHash := common.HexToHash(initHashHex)

	var salt [32]byte
	new(big.Int).SetInt64(holder).FillBytes(salt[:])

	addr := crypto.CreateAddress2(factory, salt, initHash.Bytes())
	return addr.Hex()
}
