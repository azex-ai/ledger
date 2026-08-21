package authdev

import (
	"context"
	"testing"
)

func TestInsecureLocalAttestor_SignVerifyRoundTrip(t *testing.T) {
	attestor, verifier, err := NewInsecureLocalAttestor("dev-ed25519-1")
	if err != nil {
		t.Fatalf("NewInsecureLocalAttestor: %v", err)
	}

	digest := []byte("0123456789abcdef0123456789abcdef") // arbitrary 33-byte stand-in
	sig, keyID, err := attestor.Sign(context.Background(), digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if keyID != "dev-ed25519-1" {
		t.Errorf("keyID = %q, want %q", keyID, "dev-ed25519-1")
	}
	if err := verifier.Verify(context.Background(), digest, sig, keyID); err != nil {
		t.Errorf("Verify: unexpected error: %v", err)
	}
}

func TestInsecureLocalAttestor_RejectsTamperedDigest(t *testing.T) {
	attestor, verifier, err := NewInsecureLocalAttestor("dev-ed25519-1")
	if err != nil {
		t.Fatalf("NewInsecureLocalAttestor: %v", err)
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

func TestInsecureLocalAttestor_RejectsUnknownKeyID(t *testing.T) {
	_, verifier, err := NewInsecureLocalAttestor("dev-ed25519-1")
	if err != nil {
		t.Fatalf("NewInsecureLocalAttestor: %v", err)
	}
	digest := []byte("0123456789abcdef0123456789abcdef")
	if err := verifier.Verify(context.Background(), digest, []byte("not-a-real-signature-but-64-bytes-long-so-it-doesnt-panic-abcd"), "unknown-key"); err == nil {
		t.Error("Verify with unknown key id: expected error, got nil")
	}
}

func TestNewInsecureLocalAttestor_RejectsEmptyKeyID(t *testing.T) {
	if _, _, err := NewInsecureLocalAttestor(""); err == nil {
		t.Error("expected error for empty key id, got nil")
	}
}
