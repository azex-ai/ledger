package authdev

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/azex-ai/ledger/core"
)

func testSeed(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return priv.Seed()
}

func TestNewLocalAttestor_SignVerifyRoundTrip(t *testing.T) {
	attestor, verifier, err := NewLocalAttestor(testSeed(t), "ed25519-pin-1")
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}

	digest := []byte("0123456789abcdef0123456789abcdef") // arbitrary 33-byte stand-in
	sig, keyID, err := attestor.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if keyID != "ed25519-pin-1" {
		t.Errorf("keyID = %q, want %q", keyID, "ed25519-pin-1")
	}
	if err := verifier.Verify(context.Background(), digest, sig, keyID); err != nil {
		t.Errorf("Verify: unexpected error: %v", err)
	}
}

func TestNewLocalAttestor_RejectsTamperedDigest(t *testing.T) {
	attestor, verifier, err := NewLocalAttestor(testSeed(t), "ed25519-pin-1")
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}
	digest := []byte("0123456789abcdef0123456789abcdef")
	sig, keyID, err := attestor.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tampered := append([]byte{}, digest...)
	tampered[0] ^= 0xFF
	err = verifier.Verify(context.Background(), tampered, sig, keyID)
	if err == nil {
		t.Fatal("Verify with tampered digest: expected error, got nil")
	}
	// I-45: a known key whose signature fails to verify is tamper evidence,
	// not a coverage gap -- must NOT wrap ErrUnknownAuthKey, or a caller
	// like the reconcile unauthorized_journals check would misreport real
	// tampering as "just register the key".
	if errors.Is(err, core.ErrUnknownAuthKey) {
		t.Errorf("Verify with tampered digest under a KNOWN key must not wrap ErrUnknownAuthKey, got: %v", err)
	}
}

func TestNewLocalAttestor_RejectsUnknownKeyID(t *testing.T) {
	_, verifier, err := NewLocalAttestor(testSeed(t), "ed25519-pin-1")
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}
	digest := []byte("0123456789abcdef0123456789abcdef")
	err = verifier.Verify(context.Background(), digest, []byte("not-a-real-signature-but-64-bytes-long-so-it-doesnt-panic-abcd"), "unknown-key")
	if err == nil {
		t.Fatal("Verify with unknown key id: expected error, got nil")
	}
	// I-45: this is a coverage gap (verifier doesn't hold the key), not
	// tamper evidence -- callers that need to tell the two apart key off
	// this sentinel.
	if !errors.Is(err, core.ErrUnknownAuthKey) {
		t.Errorf("Verify with unknown key id must wrap core.ErrUnknownAuthKey, got: %v", err)
	}
}

func TestNewLocalAttestor_RejectsEmptyKeyID(t *testing.T) {
	if _, _, err := NewLocalAttestor(testSeed(t), ""); err == nil {
		t.Error("expected error for empty key id, got nil")
	}
}

func TestNewLocalAttestor_RejectsWrongSeedLength(t *testing.T) {
	if _, _, err := NewLocalAttestor([]byte("too-short"), "ed25519-pin-1"); err == nil {
		t.Error("expected error for wrong-length seed, got nil")
	}
}

func TestNewLocalAttestor_DeterministicFromSameSeed(t *testing.T) {
	seed := testSeed(t)
	a1, v1, err := NewLocalAttestor(seed, "ed25519-pin-1")
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}
	a2, _, err := NewLocalAttestor(seed, "ed25519-pin-1")
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}

	digest := []byte("0123456789abcdef0123456789abcdef")
	sig1, _, err := a1.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig2, _, err := a2.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// ed25519 signatures are deterministic for a given key + message.
	if string(sig1) != string(sig2) {
		t.Error("same seed produced different signatures for the same digest")
	}
	if err := v1.Verify(context.Background(), digest, sig2, "ed25519-pin-1"); err != nil {
		t.Errorf("cross-instance verify failed: %v", err)
	}
}

// TestNewLocalVerifier_StandaloneFromPublicKey exercises the verify-only
// construction path directly (a process holding only the public key, not
// NewLocalAttestor's paired return value) -- e.g. loaded from config.
func TestNewLocalVerifier_StandaloneFromPublicKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	verifier := NewLocalVerifier(pub, "ed25519-pin-3")

	digest := []byte("0123456789abcdef0123456789abcdef")
	sig := ed25519.Sign(priv, digest)
	if err := verifier.Verify(context.Background(), digest, sig, "ed25519-pin-3"); err != nil {
		t.Errorf("Verify: unexpected error: %v", err)
	}
}

// TestLocalVerifier_MultiKeyVerifiesBothGenerations pins C-M5's unit half
// (2026-09-02 audit, tamper-evident.md M-5): a verifier must be able to hold
// more than one key generation, because journals signed under a retired key
// stay in the ledger forever (append-only) and still have to verify.
//
// The end-to-end consequence -- a rotation refusing withdrawals for every
// holder with history -- is pinned at the facade in
// TestService_KeyRotation_OldGenerationMustStayRegistered.
func TestLocalVerifier_MultiKeyVerifiesBothGenerations(t *testing.T) {
	oldAttestor, oldPub := keyGeneration(t, "gen-old")
	newAttestor, newPub := keyGeneration(t, "gen-new")

	digest := []byte("a 32-byte-ish canonical digest..")
	oldSig, oldKeyID, err := oldAttestor.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("sign with the old key: %v", err)
	}
	newSig, newKeyID, err := newAttestor.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("sign with the new key: %v", err)
	}

	both, err := NewLocalVerifierSet(map[string]ed25519.PublicKey{
		"gen-old": oldPub,
		"gen-new": newPub,
	})
	if err != nil {
		t.Fatalf("NewLocalVerifierSet: %v", err)
	}
	if err := both.Verify(context.Background(), digest, oldSig, oldKeyID); err != nil {
		t.Fatalf("the retired generation must still verify: %v", err)
	}
	if err := both.Verify(context.Background(), digest, newSig, newKeyID); err != nil {
		t.Fatalf("the current generation must verify: %v", err)
	}
	if got := both.KeyIDs(); len(got) != 2 || got[0] != "gen-new" || got[1] != "gen-old" {
		t.Fatalf("KeyIDs() = %v, want both generations sorted", got)
	}

	// The new-key-only verifier is what a naive rotation produces: the
	// retired generation becomes unknown, which core.VerifyJournalAuth turns
	// into ErrUnauthorizedJournal and the withdrawal gate turns into a
	// refusal for every holder with history.
	newOnly, err := NewLocalVerifierSet(map[string]ed25519.PublicKey{"gen-new": newPub})
	if err != nil {
		t.Fatalf("NewLocalVerifierSet: %v", err)
	}
	err = newOnly.Verify(context.Background(), digest, oldSig, oldKeyID)
	if !errors.Is(err, core.ErrUnknownAuthKey) {
		t.Fatalf("dropping the retired key must report ErrUnknownAuthKey (a coverage gap, not tamper evidence), got %v", err)
	}
}

// TestNewLocalVerifierSet_RejectsUnusableKeyMaterial pins the constructor's
// fail-at-construction contract: a verifier that silently accepted an empty
// set, an empty key id, or a wrong-length public key would fail later, inside
// the ledger, as "unauthorized journal" -- indistinguishable from tampering.
func TestNewLocalVerifierSet_RejectsUnusableKeyMaterial(t *testing.T) {
	_, pub := keyGeneration(t, "gen-1")

	cases := map[string]map[string]ed25519.PublicKey{
		"empty set":       {},
		"empty key id":    {"": pub},
		"short publickey": {"gen-1": ed25519.PublicKey([]byte{1, 2, 3})},
	}
	for name, keys := range cases {
		if _, err := NewLocalVerifierSet(keys); !errors.Is(err, core.ErrInvalidInput) {
			t.Fatalf("%s: expected ErrInvalidInput, got %v", name, err)
		}
	}
}

// keyGeneration returns an Attestor plus the public half of the same key.
func keyGeneration(t *testing.T, keyID string) (core.Attestor, ed25519.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	attestor, _, err := NewLocalAttestor(priv.Seed(), keyID)
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("unexpected public key type %T", priv.Public())
	}
	return attestor, pub
}
