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
	"sort"

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
// Immutable after construction (there is no Register method), which is why
// Verify reads keys without a lock.
type LocalVerifier struct {
	keys map[string]ed25519.PublicKey
}

var _ core.AuthVerifier = (*LocalVerifier)(nil)

// NewLocalVerifier builds a LocalVerifier from a single public key/keyID
// pair -- the convenience constructor for a deployment that has never
// rotated. Use this directly (instead of NewLocalAttestor) in a process
// that only needs to verify, never sign. To verify across a rotation, use
// NewLocalVerifierSet.
//
// KEY ROTATION CAVEAT: this verifier has no notion of key validity windows or
// revocation — a keyID it holds verifies any digest, forever. Because old
// public keys must stay registered to verify historical journals, rotating a
// COMPROMISED private key out does NOT reduce that key's power: it can still
// sign a newly-forged journal that this verifier will accept. If your threat
// model needs rotation to shrink a leaked key's blast radius, verify against
// the journal's effective_at with a per-key NotAfter — this dev implementation
// deliberately does not, and a production verifier should.
func NewLocalVerifier(pub ed25519.PublicKey, keyID string) *LocalVerifier {
	return &LocalVerifier{keys: map[string]ed25519.PublicKey{keyID: pub}}
}

// NewLocalVerifierSet builds a LocalVerifier holding SEVERAL key
// generations at once -- what a deployment that has rotated its P5 signing
// key needs, and what this package could not express at all before the
// 2026-09-02 audit (tamper-evident.md M-5 / C-M5).
//
// Why it matters more than it looks: journals are append-only and each one
// carries the key id it was signed with. A verifier that holds only the
// CURRENT key answers ErrUnknownAuthKey for every journal signed under the
// previous one, which core.VerifyJournalAuth wraps as
// ErrUnauthorizedJournal, which makes postgres.VerifiedBalanceStore report
// that dimension UNDEFINED -- so with RequireVerifiedBalance=true, a
// routine rotation refuses withdrawals for every holder with history. I-45
// already told operators to "register the retired key to restore
// verification coverage"; until this constructor existed, the library
// shipped no way to do it.
//
// keys maps key id -> public key and is COPIED: the returned verifier is
// immutable, so Verify needs no lock and no caller can widen the trusted
// set after construction (deliberately no Register method -- a verifier
// whose trusted keys can change at runtime is a different, larger security
// surface than this package is willing to own).
//
// Rotation procedure, plus why rotating a LEAKED key does not shrink its
// power, are in docs/RUNBOOK.md's "P5 signing key rotation" section. The
// caveat on NewLocalVerifier applies here identically -- once registered,
// a key id verifies any digest, forever.
func NewLocalVerifierSet(keys map[string]ed25519.PublicKey) (*LocalVerifier, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("authdev: verifier needs at least one public key: %w", core.ErrInvalidInput)
	}
	copied := make(map[string]ed25519.PublicKey, len(keys))
	for keyID, pub := range keys {
		if keyID == "" {
			return nil, fmt.Errorf("authdev: key id must not be empty: %w", core.ErrInvalidInput)
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("authdev: public key for key id %q must be %d bytes, got %d: %w",
				keyID, ed25519.PublicKeySize, len(pub), core.ErrInvalidInput)
		}
		copied[keyID] = pub
	}
	return &LocalVerifier{keys: copied}, nil
}

// KeyIDs returns the key ids this verifier trusts, for an operator-facing
// startup log or health endpoint ("which generations can this process
// verify?"). Public keys are deliberately not returned: nothing needs them
// back out, and an accessor invites callers to treat this verifier as a key
// store.
func (v *LocalVerifier) KeyIDs() []string {
	out := make([]string, 0, len(v.keys))
	for keyID := range v.keys {
		out = append(out, keyID)
	}
	sort.Strings(out)
	return out
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
