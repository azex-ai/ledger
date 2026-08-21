// Package authdev is the library's DEV-ONLY default implementation of
// core.Attestor / core.AuthVerifier (design doc §7, P5;
// docs/plans/2026-08-21-integrity-hardening-contracts.md §7: "库自带
// ed25519 实现仅供 dev 且必须显式构造，不做静默默认").
//
// It exists so the per-journal signing subsystem can be exercised in a
// local dev environment or a testcontainers-backed test suite without a
// real KMS/HSM. It is NOT a production adapter: the private key lives in
// process memory, in plaintext, for as long as the *LocalAttestor is
// referenced -- exactly the failure mode custody.md and design doc §0
// exist to keep out of the money-path. Every constructor name in this
// package says "Insecure" for that reason; there is no un-prefixed
// convenience constructor to accidentally wire into production.
package authdev

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// InsecureLocalAttestor is a core.Attestor backed by an in-process ed25519
// private key. DEV / TEST ONLY -- see package doc comment.
type InsecureLocalAttestor struct {
	keyID string
	priv  ed25519.PrivateKey
}

var _ core.Attestor = (*InsecureLocalAttestor)(nil)

// NewInsecureLocalAttestor generates a fresh ed25519 keypair and returns an
// Attestor over it, plus the corresponding InsecureLocalVerifier and raw
// public key (for a consumer that wants to persist/distribute it). keyID is
// stamped on every signature this Attestor produces, exactly like a KMS key
// version -- callers should use something identifying the key generation
// (e.g. "dev-ed25519-1"), not a secret.
//
// Must be called explicitly; nothing in this package or core wires it in
// automatically.
func NewInsecureLocalAttestor(keyID string) (*InsecureLocalAttestor, *InsecureLocalVerifier, error) {
	if keyID == "" {
		return nil, nil, fmt.Errorf("authdev: key id must not be empty: %w", core.ErrInvalidInput)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("authdev: generate ed25519 key: %w", err)
	}
	return &InsecureLocalAttestor{keyID: keyID, priv: priv},
		&InsecureLocalVerifier{keys: map[string]ed25519.PublicKey{keyID: pub}},
		nil
}

// Sign implements core.Attestor.
func (a *InsecureLocalAttestor) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	default:
	}
	// ed25519.Sign operates on the message directly (it hashes internally
	// with its own domain-separated scheme) -- digest is already a fixed
	// 32-byte SHA-256 output from core.CanonicalJournalDigest, so this is
	// signing a hash, not re-hashing arbitrary-length data.
	sig := ed25519.Sign(a.priv, digest)
	return sig, a.keyID, nil
}

// InsecureLocalVerifier is the core.AuthVerifier counterpart to
// InsecureLocalAttestor. DEV / TEST ONLY -- see package doc comment.
type InsecureLocalVerifier struct {
	keys map[string]ed25519.PublicKey
}

var _ core.AuthVerifier = (*InsecureLocalVerifier)(nil)

// Verify implements core.AuthVerifier.
func (v *InsecureLocalVerifier) Verify(ctx context.Context, digest, signature []byte, keyID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	pub, ok := v.keys[keyID]
	if !ok {
		return fmt.Errorf("authdev: unknown key id %q", keyID)
	}
	if !ed25519.Verify(pub, digest, signature) {
		return fmt.Errorf("authdev: signature verification failed for key id %q", keyID)
	}
	return nil
}
