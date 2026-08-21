package core

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// EncodeAmount golden vectors.
//
// Every hex string below was computed independently in Python (not by
// calling this package), replicating the byte layout documented on
// EncodeAmount: rescale to exactly 18 fractional digits, then big-endian
// two's complement, fixed 16 bytes. See
// docs/plans/2026-08-21-integrity-hardening-contracts.md §9 item 3 ("golden
// vectors 的实际值") for why this file exists.
// ---------------------------------------------------------------------------

func TestEncodeAmount_GoldenVectors(t *testing.T) {
	cases := []struct {
		name    string
		amount  string
		wantHex string
	}{
		{"zero", "0", "00000000000000000000000000000000"},
		{"positive with fraction", "123.456", "0000000000000006b14bd1e6eea00000"},
		{"negative with fraction", "-123.456", "fffffffffffffff94eb42e1911600000"},
		{"max NUMERIC(30,18) magnitude (30 nines)", "999999999999.999999999999999999", "0000000c9f2c9cd04674edea3fffffff"},
		{"negative zero", "-0", "00000000000000000000000000000000"},
		{"smallest positive representable (1 at 18th decimal)", "0.000000000000000001", "00000000000000000000000000000001"},
		{"smallest negative representable", "-0.000000000000000001", "ffffffffffffffffffffffffffffffff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amt, err := decimal.NewFromString(tc.amount)
			if err != nil {
				t.Fatalf("decimal.NewFromString(%q): %v", tc.amount, err)
			}
			got, err := EncodeAmount(amt)
			if err != nil {
				t.Fatalf("EncodeAmount(%s): unexpected error: %v", tc.amount, err)
			}
			gotHex := hex.EncodeToString(got)
			if gotHex != tc.wantHex {
				t.Errorf("EncodeAmount(%s) = %s, want %s", tc.amount, gotHex, tc.wantHex)
			}
			if len(got) != authAmountEncodedLen {
				t.Errorf("EncodeAmount(%s) length = %d, want %d", tc.amount, len(got), authAmountEncodedLen)
			}
		})
	}
}

func TestEncodeAmount_ZeroAndNegativeZeroEncodeIdentically(t *testing.T) {
	zero, _ := decimal.NewFromString("0")
	negZero, _ := decimal.NewFromString("-0")

	zeroBytes, err := EncodeAmount(zero)
	if err != nil {
		t.Fatalf("EncodeAmount(0): %v", err)
	}
	negZeroBytes, err := EncodeAmount(negZero)
	if err != nil {
		t.Fatalf("EncodeAmount(-0): %v", err)
	}
	if hex.EncodeToString(zeroBytes) != hex.EncodeToString(negZeroBytes) {
		t.Errorf("0 and -0 must encode identically: got %x vs %x", zeroBytes, negZeroBytes)
	}
}

func TestEncodeAmount_RejectsMoreThan18FractionalDigits(t *testing.T) {
	amt, err := decimal.NewFromString("1.1234567890123456789") // 19 fractional digits
	if err != nil {
		t.Fatalf("decimal.NewFromString: %v", err)
	}
	if _, err := EncodeAmount(amt); err == nil {
		t.Fatal("EncodeAmount with 19 fractional digits: expected error, got nil")
	}
}

func TestEncodeAmount_AcceptsExactly18FractionalDigits(t *testing.T) {
	amt, err := decimal.NewFromString("1.123456789012345678") // exactly 18 fractional digits
	if err != nil {
		t.Fatalf("decimal.NewFromString: %v", err)
	}
	if _, err := EncodeAmount(amt); err != nil {
		t.Fatalf("EncodeAmount with exactly 18 fractional digits: unexpected error: %v", err)
	}
}

func TestEncodeAmount_RejectsOutOfRangeMagnitude(t *testing.T) {
	// One order of magnitude beyond NUMERIC(30,18)'s max (30 nines) still
	// fits comfortably in 128 bits (10^31 < 2^127), so push further: a
	// number whose scaled magnitude exceeds 2^127.
	amt, err := decimal.NewFromString("170141183460469231731.687303715884105728") // ~2^127 scaled by 1e18 already at the boundary; go one further below
	if err != nil {
		t.Fatalf("decimal.NewFromString: %v", err)
	}
	if _, err := EncodeAmount(amt); err == nil {
		t.Fatal("EncodeAmount with out-of-range magnitude: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// CanonicalJournalDigest golden vector + determinism properties.
// ---------------------------------------------------------------------------

func goldenJournalInput() JournalInput {
	return JournalInput{
		JournalTypeUID: "jt-deposit-uid",
		IdempotencyKey: "idem-key-001",
		ActorID:        42,
		Source:         "test-source",
		Entries: []EntryInput{
			{AccountHolder: 1001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-mainwallet-uid", EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("100.5")},
			{AccountHolder: 2001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-fees-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("0.5")},
			{AccountHolder: 2002, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-revenue-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("100")},
		},
	}
}

func TestCanonicalJournalDigest_GoldenVector(t *testing.T) {
	input := goldenJournalInput()
	effectiveAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	// Independently computed in Python from the exact byte layout documented
	// on CanonicalJournalDigest (see /tmp/golden.py used to derive this --
	// reproduced in the plan report). Any diff here is a breaking encoding
	// change requiring a new domain separator, not a silent fix.
	const want = "94fb4ab57aee5ace05e4c78752a28d98267d2ddccdddc821568d67ed6aa098c9"

	got, err := CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("CanonicalJournalDigest = %s, want %s", hex.EncodeToString(got), want)
	}
}

func TestCanonicalJournalDigest_EntryOrderIndependent(t *testing.T) {
	input := goldenJournalInput()
	effectiveAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	d1, err := CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}

	reordered := goldenJournalInput()
	reordered.Entries = []EntryInput{reordered.Entries[2], reordered.Entries[0], reordered.Entries[1]}
	d2, err := CanonicalJournalDigest(reordered, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest (reordered): %v", err)
	}

	if hex.EncodeToString(d1) != hex.EncodeToString(d2) {
		t.Errorf("entry order changed the digest: %x vs %x", d1, d2)
	}
}

func TestCanonicalJournalDigest_DoesNotMutateCallerEntries(t *testing.T) {
	input := goldenJournalInput()
	original := make([]EntryInput, len(input.Entries))
	copy(original, input.Entries)

	if _, err := CanonicalJournalDigest(input, time.Now()); err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}

	for i := range input.Entries {
		if input.Entries[i] != original[i] {
			t.Errorf("entries[%d] mutated: got %+v, want %+v", i, input.Entries[i], original[i])
		}
	}
}

func TestCanonicalJournalDigest_DifferentAmountDifferentDigest(t *testing.T) {
	effectiveAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	a := goldenJournalInput()
	d1, err := CanonicalJournalDigest(a, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}

	b := goldenJournalInput()
	b.Entries[0].Amount = b.Entries[0].Amount.Add(decimal.NewFromInt(1))
	d2, err := CanonicalJournalDigest(b, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}

	if hex.EncodeToString(d1) == hex.EncodeToString(d2) {
		t.Error("changing an entry amount did not change the digest")
	}
}

func TestCanonicalJournalDigest_DifferentEffectiveAtDifferentDigest(t *testing.T) {
	input := goldenJournalInput()
	d1, err := CanonicalJournalDigest(input, time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}
	d2, err := CanonicalJournalDigest(input, time.Date(2026, 8, 21, 12, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}
	if hex.EncodeToString(d1) == hex.EncodeToString(d2) {
		t.Error("changing effectiveAt did not change the digest")
	}
}

func TestCanonicalJournalDigest_DeterministicAcrossCalls(t *testing.T) {
	input := goldenJournalInput()
	effectiveAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	d1, err := CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}
	d2, err := CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}
	if hex.EncodeToString(d1) != hex.EncodeToString(d2) {
		t.Error("same input + same effectiveAt produced different digests -- signature reuse (design doc §7.3) depends on this being deterministic")
	}
}

// ---------------------------------------------------------------------------
// AuthPolicy
// ---------------------------------------------------------------------------

func TestAuthPolicy_ValidateFailureMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    AttestorFailureMode
		wantErr bool
	}{
		{"unset is rejected", AttestorFailureModeUnset, true},
		{"fail closed is accepted", AttestorFailureModeFailClosed, false},
		{"fail open is accepted", AttestorFailureModeFailOpen, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := AuthPolicy{FailureMode: tc.mode}
			err := p.ValidateFailureMode()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAuthPolicy_RequirementFor(t *testing.T) {
	p := AuthPolicy{
		FailureMode: AttestorFailureModeFailClosed,
		Coverage: map[string]SignatureRequirement{
			"deposit":  SignatureRequirementRequired,
			"transfer": SignatureRequirementExempt,
		},
	}

	if req, err := p.RequirementFor("deposit"); err != nil || req != SignatureRequirementRequired {
		t.Errorf("RequirementFor(deposit) = (%v, %v), want (Required, nil)", req, err)
	}
	if req, err := p.RequirementFor("transfer"); err != nil || req != SignatureRequirementExempt {
		t.Errorf("RequirementFor(transfer) = (%v, %v), want (Exempt, nil)", req, err)
	}
	if _, err := p.RequirementFor("withdrawal"); err == nil {
		t.Error("RequirementFor(withdrawal) with no coverage decision: expected error, got nil")
	}
}

func TestWithdrawalSignatureThreshold_Configured(t *testing.T) {
	unset := WithdrawalSignatureThreshold{}
	if unset.Configured() {
		t.Error("zero-value WithdrawalSignatureThreshold must not be Configured()")
	}
	disabled := WithdrawalSignatureThreshold{MinSignedAmount: WithdrawalGateDisabled}
	if !disabled.Configured() {
		t.Error("WithdrawalGateDisabled must be Configured()")
	}
	positive := WithdrawalSignatureThreshold{MinSignedAmount: decimal.NewFromInt(100)}
	if !positive.Configured() {
		t.Error("a positive threshold must be Configured()")
	}
}

// ---------------------------------------------------------------------------
// VerifyJournalAuth
// ---------------------------------------------------------------------------

// Real cryptography (the ed25519 dev Attestor/AuthVerifier pair) is
// exercised end-to-end in postgres/auth_pin_test.go; these three cases only
// need to assert VerifyJournalAuth's own control flow, all of which return
// before ever calling verifier.Verify -- passing a nil AuthVerifier is
// deliberate, not an oversight.

func TestVerifyJournalAuth_RejectsEmptyStoredDigest(t *testing.T) {
	input := goldenJournalInput()
	err := VerifyJournalAuth(context.Background(), nil, input, time.Now(), nil, []byte("sig"), "key-1")
	if err == nil {
		t.Fatal("expected error for empty stored digest, got nil")
	}
}

func TestVerifyJournalAuth_RejectsMismatchedDigest(t *testing.T) {
	input := goldenJournalInput()
	effectiveAt := time.Now()
	wrongDigest := []byte("not-the-real-digest-but-16-plus-bytes-long")
	err := VerifyJournalAuth(context.Background(), nil, input, effectiveAt, wrongDigest, []byte("sig"), "key-1")
	if err == nil {
		t.Fatal("expected error for mismatched digest, got nil")
	}
}

func TestVerifyJournalAuth_RejectsEmptySignature(t *testing.T) {
	input := goldenJournalInput()
	effectiveAt := time.Now()
	digest, err := CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		t.Fatalf("CanonicalJournalDigest: %v", err)
	}
	if err := VerifyJournalAuth(context.Background(), nil, input, effectiveAt, digest, nil, ""); err == nil {
		t.Fatal("expected error for empty signature, got nil")
	}
}
