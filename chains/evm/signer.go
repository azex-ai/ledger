package evm

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/azex-ai/ledger/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// LocalSigner is the library's default core.Signer implementation (Foundation,
// contract §5) -- a single in-process private key. It exists so the crypto
// deposit bundle works out of the box; consumers who need custodial-grade
// key management swap in a KMS/HSM Signer without touching sweep
// orchestration (design doc §0).
//
// The "unsignedTx []byte" / "signedTx []byte" wire format both sides of
// this port agree on is a go-ethereum typed transaction envelope
// (types.Transaction.MarshalBinary): the caller builds a *types.Transaction
// with a zero-value signature and marshals it; SignTx unmarshals it back,
// computes the EIP-155/typed signing hash, signs, reattaches the signature,
// and remarshals.
type LocalSigner struct {
	key *ecdsa.PrivateKey
}

// NewLocalSigner parses a hex-encoded secp256k1 private key (with or
// without "0x" prefix). The key is held only in memory for the lifetime of
// the process -- callers must never log hexKey or the returned *LocalSigner's
// internal state.
func NewLocalSigner(hexKey string) (*LocalSigner, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("evm: local signer: invalid private key: %w", err)
	}
	return &LocalSigner{key: key}, nil
}

var _ core.Signer = (*LocalSigner)(nil)

// Address returns the signer's public EOA address -- callers need this to
// query NextNonce / gas balance for the sweeper account, without exposing
// the private key itself.
func (s *LocalSigner) Address() string {
	return crypto.PubkeyToAddress(s.key.PublicKey).Hex()
}

// SignTx unmarshals unsignedTx (a go-ethereum typed transaction envelope
// with a placeholder signature), signs it for chainID, and returns the
// re-marshaled signed envelope.
func (s *LocalSigner) SignTx(ctx context.Context, chainID int64, unsignedTx []byte) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(unsignedTx); err != nil {
		return nil, fmt.Errorf("evm: local signer: unmarshal unsigned tx: %w", err)
	}
	signer := types.LatestSignerForChainID(big.NewInt(chainID))
	signed, err := types.SignTx(tx, signer, s.key)
	if err != nil {
		return nil, fmt.Errorf("evm: local signer: sign: %w", err)
	}
	out, err := signed.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("evm: local signer: marshal signed tx: %w", err)
	}
	return out, nil
}

// EncodeUnsignedTx marshals a *types.Transaction (with placeholder V=0
// signature, as produced by types.NewTx) into the byte envelope SignTx
// expects. Exported so Sweeper (and tests) can build the request.
func EncodeUnsignedTx(tx *types.Transaction) ([]byte, error) {
	out, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("evm: encode unsigned tx: %w", err)
	}
	return out, nil
}

// DecodeSignedTx parses SignTx's output back into a *types.Transaction, for
// broadcasting.
func DecodeSignedTx(signedTx []byte) (*types.Transaction, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(signedTx); err != nil {
		return nil, fmt.Errorf("evm: decode signed tx: %w", err)
	}
	return tx, nil
}
