package authdev

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
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
	if err := verifier.Verify(context.Background(), tampered, sig, keyID); err == nil {
		t.Error("Verify with tampered digest: expected error, got nil")
	}
}

func TestNewLocalAttestor_RejectsUnknownKeyID(t *testing.T) {
	_, verifier, err := NewLocalAttestor(testSeed(t), "ed25519-pin-1")
	if err != nil {
		t.Fatalf("NewLocalAttestor: %v", err)
	}
	digest := []byte("0123456789abcdef0123456789abcdef")
	if err := verifier.Verify(context.Background(), digest, []byte("not-a-real-signature-but-64-bytes-long-so-it-doesnt-panic-abcd"), "unknown-key"); err == nil {
		t.Error("Verify with unknown key id: expected error, got nil")
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
