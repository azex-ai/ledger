// Package authdev is the library's default implementation of
// core.Attestor / core.AuthVerifier (design doc §7, P5): an ed25519
// keypair held in process memory. For a monolith deployment this is a
// production-ready implementation, not a placeholder -- the threat model
// (design doc §1 non-goal 2: "app + KMS 同时失陷" is out of scope
// regardless of where the key lives) already concedes that an attacker
// who compromises the app process can forge signatures no matter what key
// custody model is behind Attestor. What this package's key DOES defend
// against is exactly what P5 exists for: an attacker who has only a
// database write credential (a leaked DATABASE_URL, a SQL injection) can
// forge a perfectly balanced journal, but cannot sign it, because the key
// never enters the database (Team Lead's 2026-08-21 simplification pass:
// the original brief's remote-KMS failure-mode/latency configuration
// surface was solving a deployment problem this project does not have).
//
// Key material is always supplied by the caller -- this package never
// generates or hardcodes a key. See NewLocalAttestor's doc comment for the
// deployment constraint on where that key must NOT live.
package authdev

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// LocalAttestor is a core.Attestor backed by an in-process ed25519 private
// key.
type LocalAttestor struct {
	keyID string
	priv  ed25519.PrivateKey
}

var _ core.Attestor = (*LocalAttestor)(nil)

// NewLocalAttestor derives an ed25519 keypair from seed (exactly
// ed25519.SeedSize == 32 bytes) and returns an Attestor over the private
// half plus a LocalVerifier over the public half. keyID is stamped on
// every signature this Attestor produces -- use something that identifies
// the key generation (e.g. "ed25519-2026-08"), not a secret, so rotations
// are distinguishable in stored journals.
//
// The caller is responsible for obtaining seed (env var, file, secret
// manager -- whatever the deployment already uses) and MUST NOT load it
// from the same secrets store / env bundle as DATABASE_URL (see
// core.Attestor's doc comment for why). This function itself never reads
// any environment, file, or generates a key on its own -- if seed is
// wrong, construction fails here, at the caller's composition root, not
// silently later inside the ledger.
func NewLocalAttestor(seed []byte, keyID string) (*LocalAttestor, *LocalVerifier, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, nil, fmt.Errorf("authdev: seed must be %d bytes, got %d: %w", ed25519.SeedSize, len(seed), core.ErrInvalidInput)
	}
	if keyID == "" {
		return nil, nil, fmt.Errorf("authdev: key id must not be empty: %w", core.ErrInvalidInput)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("authdev: derived public key has unexpected type %T", priv.Public())
	}
	return &LocalAttestor{keyID: keyID, priv: priv}, NewLocalVerifier(pub, keyID), nil
}

// Sign implements core.Attestor.
func (a *LocalAttestor) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
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

// LocalVerifier is the core.AuthVerifier counterpart to LocalAttestor. It
// holds only public keys, so it can run in a process that never has
// access to the private signing key at all (core.AuthVerifier's doc
// comment: "verification can run entirely outside the database host").
type LocalVerifier struct {
	keys map[string]ed25519.PublicKey
}

var _ core.AuthVerifier = (*LocalVerifier)(nil)

// NewLocalVerifier builds a LocalVerifier from a single public key/keyID
// pair. Use this directly (instead of NewLocalAttestor) in a process that
// only needs to verify, never sign.
func NewLocalVerifier(pub ed25519.PublicKey, keyID string) *LocalVerifier {
	return &LocalVerifier{keys: map[string]ed25519.PublicKey{keyID: pub}}
}

// Verify implements core.AuthVerifier.
func (v *LocalVerifier) Verify(ctx context.Context, digest, signature []byte, keyID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	pub, ok := v.keys[keyID]
	if !ok {
		// core.AuthVerifier's contract (I-45): this verifier not holding
		// keyID is a coverage gap (a rotated-out or foreign key), not
		// tamper evidence -- wrap ErrUnknownAuthKey so callers that need
		// to tell the two apart (e.g. the unauthorized_journals reconcile
		// check) can, without this package importing anything beyond
		// core's sentinels.
		return fmt.Errorf("authdev: unknown key id %q: %w", keyID, core.ErrUnknownAuthKey)
	}
	if !ed25519.Verify(pub, digest, signature) {
		return fmt.Errorf("authdev: signature verification failed for key id %q", keyID)
	}
	return nil
}
