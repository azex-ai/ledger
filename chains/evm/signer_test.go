package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// generated with `openssl rand -hex 32` -- test-only key, never used
// anywhere real.
const testSignerKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func TestLocalSigner_SignTx_RoundTrip(t *testing.T) {
	signer, err := NewLocalSigner(testSignerKey)
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}

	to := common.HexToAddress("0x000000000000000000000000000000000000dead")
	const chainID = 1337
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     7,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(3_000_000_000),
		Gas:       21_000,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      nil,
	})

	unsignedBytes, err := EncodeUnsignedTx(unsigned)
	if err != nil {
		t.Fatalf("EncodeUnsignedTx: %v", err)
	}

	signedBytes, err := signer.SignTx(t.Context(), chainID, unsignedBytes)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}

	signedTx, err := DecodeSignedTx(signedBytes)
	if err != nil {
		t.Fatalf("DecodeSignedTx: %v", err)
	}

	ethSigner := types.LatestSignerForChainID(big.NewInt(chainID))
	sender, err := types.Sender(ethSigner, signedTx)
	if err != nil {
		t.Fatalf("recover sender: %v", err)
	}
	if sender.Hex() != signer.Address() {
		t.Errorf("recovered sender = %s, want signer address %s", sender.Hex(), signer.Address())
	}

	// Nonce/gas fields must survive the encode -> sign -> decode round trip
	// unchanged -- only the signature should have been added.
	if signedTx.Nonce() != unsigned.Nonce() {
		t.Errorf("nonce changed: got %d, want %d", signedTx.Nonce(), unsigned.Nonce())
	}
	if signedTx.To().Hex() != to.Hex() {
		t.Errorf("to changed: got %s, want %s", signedTx.To().Hex(), to.Hex())
	}
}

func TestLocalSigner_InvalidKey(t *testing.T) {
	if _, err := NewLocalSigner("not-hex"); err == nil {
		t.Error("expected error for invalid hex key, got nil")
	}
}
