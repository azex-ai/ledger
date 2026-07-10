package evm

import (
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/ethclient"
)

func TestScanner_TokenDecimals(t *testing.T) {
	clients := &ClientSet{
		chains: core.ChainSet{
			1: {
				ChainID: 1,
				SweepTokens: map[string]core.TokenConfig{
					core.SweepNativeToken: {TokenAddress: core.SweepNativeToken, CurrencyCode: "ETH", Decimals: 18},
				},
				CreditTokens: map[string]core.TokenConfig{
					"0xdac17f958d2ee523a2206206994597c13d831ec7": {TokenAddress: "0xdac17f958d2ee523a2206206994597c13d831ec7", CurrencyCode: "USDT", Decimals: 6},
				},
			},
		},
		clients: map[int64]*ethclient.Client{},
	}
	scanner := NewScanner(clients, 0)

	decimals, err := scanner.tokenDecimals(1, "0xDAC17F958D2ee523a2206206994597C13D831ec7") // mixed case must still match the lowercase-keyed config
	if err != nil {
		t.Fatalf("tokenDecimals(credit token): %v", err)
	}
	if decimals != 6 {
		t.Errorf("decimals = %d, want 6", decimals)
	}

	decimals, err = scanner.tokenDecimals(1, core.SweepNativeToken)
	if err != nil {
		t.Fatalf("tokenDecimals(native): %v", err)
	}
	if decimals != 18 {
		t.Errorf("decimals = %d, want 18", decimals)
	}

	if _, err := scanner.tokenDecimals(1, "0x000000000000000000000000000000000000ff"); err == nil {
		t.Error("expected ErrTokenNotConfigured for unregistered token, got nil")
	}

	if _, err := scanner.tokenDecimals(999, core.SweepNativeToken); err == nil {
		t.Error("expected ErrChainNotConfigured for unregistered chain, got nil")
	}
}
