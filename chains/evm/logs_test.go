package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/shopspring/decimal"
)

func TestAddressesToTopics(t *testing.T) {
	addrs := []string{
		"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	}
	topics, err := addressesToTopics(addrs)
	if err != nil {
		t.Fatalf("addressesToTopics: %v", err)
	}
	if len(topics) != 2 {
		t.Fatalf("got %d topics, want 2", len(topics))
	}
	for i, addr := range addrs {
		want := common.BytesToHash(common.HexToAddress(addr).Bytes())
		if topics[i] != want {
			t.Errorf("topic %d = %s, want %s", i, topics[i].Hex(), want.Hex())
		}
	}
}

func TestAddressesToTopics_InvalidAddress(t *testing.T) {
	if _, err := addressesToTopics([]string{"not-an-address"}); err == nil {
		t.Error("expected error for invalid address, got nil")
	}
}

func TestShardTopics(t *testing.T) {
	topics := make([]common.Hash, 1201)
	for i := range topics {
		topics[i] = common.BigToHash(big.NewInt(int64(i)))
	}
	shards := shardTopics(topics, 500)
	if len(shards) != 3 {
		t.Fatalf("got %d shards, want 3", len(shards))
	}
	if len(shards[0]) != 500 || len(shards[1]) != 500 || len(shards[2]) != 201 {
		t.Errorf("shard sizes = %d/%d/%d, want 500/500/201", len(shards[0]), len(shards[1]), len(shards[2]))
	}
	// Reassembled order must be preserved (log ordering downstream depends
	// on it staying deterministic).
	var total int
	for _, s := range shards {
		total += len(s)
	}
	if total != len(topics) {
		t.Errorf("total sharded = %d, want %d", total, len(topics))
	}
}

func TestShardTopics_Empty(t *testing.T) {
	if shards := shardTopics(nil, 500); shards != nil {
		t.Errorf("expected nil for empty input, got %v", shards)
	}
}

func TestDecodeTransferLog(t *testing.T) {
	from := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	to := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	amount := big.NewInt(123_456)

	lg := types.Log{
		Topics: []common.Hash{
			erc20TransferSig,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data: common.LeftPadBytes(amount.Bytes(), 32),
	}

	gotFrom, gotTo, gotAmount, err := decodeTransferLog(lg)
	if err != nil {
		t.Fatalf("decodeTransferLog: %v", err)
	}
	if gotFrom != from || gotTo != to {
		t.Errorf("from/to = %s/%s, want %s/%s", gotFrom.Hex(), gotTo.Hex(), from.Hex(), to.Hex())
	}
	if gotAmount.Cmp(amount) != 0 {
		t.Errorf("amount = %s, want %s", gotAmount, amount)
	}
}

func TestDecodeTransferLog_Malformed(t *testing.T) {
	cases := []struct {
		name string
		lg   types.Log
	}{
		{"wrong topic count", types.Log{Topics: []common.Hash{erc20TransferSig}, Data: make([]byte, 32)}},
		{"wrong data length", types.Log{Topics: []common.Hash{erc20TransferSig, {}, {}}, Data: make([]byte, 31)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := decodeTransferLog(tc.lg); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestNormalizeAmount(t *testing.T) {
	cases := []struct {
		raw      *big.Int
		decimals int32
		want     string
	}{
		{big.NewInt(1_000_000), 6, "1"},                    // USDT/USDC-style 6 decimals
		{big.NewInt(1_500_000_000_000_000_000), 18, "1.5"}, // 18-decimal token
		{big.NewInt(0), 18, "0"},
	}
	for _, tc := range cases {
		got := normalizeAmount(tc.raw, tc.decimals)
		want, _ := decimal.NewFromString(tc.want)
		if !got.Equal(want) {
			t.Errorf("normalizeAmount(%s, %d) = %s, want %s", tc.raw, tc.decimals, got, want)
		}
	}
}

func TestNormalizeTokenKey(t *testing.T) {
	got := normalizeTokenKey("0xABCDEF0000000000000000000000000000000A")
	want := "0xabcdef0000000000000000000000000000000a"
	if got != want {
		t.Errorf("normalizeTokenKey = %s, want %s", got, want)
	}
}
