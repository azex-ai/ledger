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

// TestCanonicalJournalDigest_GoldenVectors_ExtraFields covers the fields
// the base golden vector above leaves at their zero value: EventUID,
// ReversalOfUID, more than one currency in a single journal, a negative
// ActorID, and an amount at the 18-decimal precision boundary. Each
// expected hex value was computed independently in Python from the exact
// byte layout documented on CanonicalJournalDigest (Team Lead's
// 2026-08-21 direction: this encoding is the one thing in P5 that must
// not be under-invested in -- it can never change without breaking every
// previously-signed journal).
func TestCanonicalJournalDigest_GoldenVectors_ExtraFields(t *testing.T) {
	entries := []EntryInput{
		{AccountHolder: 1001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-mainwallet-uid", EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("100.5")},
		{AccountHolder: 2001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-fees-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("0.5")},
		{AccountHolder: 2002, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-revenue-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("100")},
	}

	cases := []struct {
		name        string
		input       JournalInput
		effectiveAt time.Time
		want        string
	}{
		{
			name: "event_uid set",
			input: JournalInput{
				JournalTypeUID: "jt-deposit-uid", IdempotencyKey: "idem-key-002", ActorID: 42,
				Source: "test-source", EventUID: "evt-abc-123", Entries: entries,
			},
			effectiveAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
			want:        "136e6875d56e4343dec75a5cdffd74079d8c46f04955f100052c7d7cb40aabab",
		},
		{
			name: "reversal_of_uid set",
			input: JournalInput{
				JournalTypeUID: "jt-deposit-uid", IdempotencyKey: "idem-key-003", ActorID: 42,
				Source: "reversal", ReversalOfUID: "journal-orig-uid-999", Entries: entries,
			},
			effectiveAt: time.Date(2026, 8, 21, 13, 30, 0, 0, time.UTC),
			want:        "92586cbfa74e93295397a1b935839c7e33da6d2cee3ed4f9426041c9a5d0f265",
		},
		{
			name: "multi-currency journal",
			input: JournalInput{
				JournalTypeUID: "jt-fx-uid", IdempotencyKey: "idem-key-004", ActorID: 7, Source: "fx-test",
				Entries: []EntryInput{
					{AccountHolder: 1001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-mainwallet-uid", EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("100.5")},
					{AccountHolder: 3001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-fees-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("100.5")},
					{AccountHolder: 1001, CurrencyUID: "cur-eur-uid", ClassificationUID: "class-mainwallet-uid", EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("50.25")},
					{AccountHolder: 3001, CurrencyUID: "cur-eur-uid", ClassificationUID: "class-fees-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("50.25")},
				},
			},
			effectiveAt: time.Date(2026, 8, 21, 14, 0, 0, 0, time.UTC),
			want:        "c5919fa74a867d8eb72c3e6d001ba474aa95e4addda0148c29293075660c22d1",
		},
		{
			name: "negative actor_id + smallest representable amount",
			input: JournalInput{
				JournalTypeUID: "jt-tiny-uid", IdempotencyKey: "idem-key-005", ActorID: -1, Source: "system",
				Entries: []EntryInput{
					{AccountHolder: -9001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-custodial-uid", EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("0.000000000000000001")},
					{AccountHolder: 5001, CurrencyUID: "cur-usd-uid", ClassificationUID: "class-mainwallet-uid", EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("0.000000000000000001")},
				},
			},
			effectiveAt: time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC),
			want:        "3f597d9f61c1eccdd0c5e84690b05c4a87ab2ced74d9764c3d15bda0b0b576ff",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJournalDigest(tc.input, tc.effectiveAt)
			if err != nil {
				t.Fatalf("CanonicalJournalDigest: unexpected error: %v", err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Errorf("CanonicalJournalDigest = %s, want %s", hex.EncodeToString(got), tc.want)
			}
		})
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
// VerifyJournalAuth
//
// Note: there is no AuthPolicy / AttestorFailureMode / SignatureRequirement
// / WithdrawalSignatureThreshold in this package (Team Lead's 2026-08-21
// simplification pass removed them from the original task brief -- see
// auth.go's package doc comment). Every journal is signed whenever an
// Attestor is configured; a remote KMS's retry/degrade semantics, if one
// is ever wired in, belong inside that Attestor implementation.
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
