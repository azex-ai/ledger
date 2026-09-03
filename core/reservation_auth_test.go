package core_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// baseDischargeIntent is the intent every case below perturbs one field of.
func baseDischargeIntent() core.ReservationDischargeIntent {
	return core.ReservationDischargeIntent{
		ReservationUID: "0192f3d4-5678-7abc-8def-0123456789ab",
		Operation:      core.ReservationOpSettle,
		Amount:         decimal.RequireFromString("123.456"),
		IdempotencyKey: "settle-key-1",
		RecordedAt:     time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
}

// TestCanonicalReservationDischargeDigest_GoldenVector pins the exact byte
// layout (docs/INVARIANTS.md I-65). Changing it is a breaking encoding
// change that requires a new domain separator, not a silent fix -- the same
// contract core.TestCanonicalJournalDigest_GoldenVector holds for 0x10.
func TestCanonicalReservationDischargeDigest_GoldenVector(t *testing.T) {
	digest, err := core.CanonicalReservationDischargeDigest(baseDischargeIntent())
	require.NoError(t, err)
	// Derived from the DOCUMENTED layout by an independent implementation (a
	// throwaway Python script following CanonicalReservationDischargeDigest's
	// doc comment byte for byte), not copied from this package's output --
	// otherwise the vector would pin whatever the code happens to do,
	// including a bug, which is the failure mode a golden vector exists to
	// prevent.
	assert.Equal(t,
		"97bcbc98ba3a5326d10d3234437c521daa5f29fa4ffe3f112ccd07249f66e398",
		hex.EncodeToString(digest),
		"the discharge digest's byte layout is a wire contract; a change here needs a new domain separator (see core/auth.go's allocation table)")
}

// TestCanonicalReservationDischargeDigest_EveryFieldIsCovered is the reason
// a "tampered claim" pin can exist at all: if any field were left out of the
// preimage, an attacker could change it on the persisted row and the
// signature would still verify. So every field must move the digest.
//
// Enumerated explicitly rather than by reflection: the point is that a
// future author who ADDS a field to ReservationDischargeIntent and forgets
// to encode it gets no failure here -- but the golden vector above also
// would not move, so the pair is what covers it. This test covers the
// fields that exist.
func TestCanonicalReservationDischargeDigest_EveryFieldIsCovered(t *testing.T) {
	base, err := core.CanonicalReservationDischargeDigest(baseDischargeIntent())
	require.NoError(t, err)

	mutations := map[string]func(*core.ReservationDischargeIntent){
		"reservation uid": func(in *core.ReservationDischargeIntent) {
			in.ReservationUID = "0192f3d4-5678-7abc-8def-0123456789ac"
		},
		"operation": func(in *core.ReservationDischargeIntent) {
			in.Operation = core.ReservationOpRelease
		},
		"amount": func(in *core.ReservationDischargeIntent) {
			in.Amount = decimal.RequireFromString("123.457")
		},
		"idempotency key": func(in *core.ReservationDischargeIntent) {
			in.IdempotencyKey = "settle-key-2"
		},
		"recorded at": func(in *core.ReservationDischargeIntent) {
			in.RecordedAt = in.RecordedAt.Add(time.Microsecond)
		},
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			in := baseDischargeIntent()
			mutate(&in)
			other, err := core.CanonicalReservationDischargeDigest(in)
			require.NoError(t, err)
			assert.NotEqual(t, base, other, "changing the %s must change the digest, or a persisted row could be edited there without breaking its signature", name)
		})
	}
}

// TestCanonicalReservationDischargeDigest_SeparatesRecordKinds pins the one
// reason core.ReservationOpSettlePartial exists: settlement legs have no
// `operation` column, so if the two record kinds shared a preimage, a
// signature minted for a 'settle' receipt would verify as a settlement leg
// of the same amount on the same reservation (and vice versa) -- letting one
// signed claim be counted twice.
func TestCanonicalReservationDischargeDigest_SeparatesRecordKinds(t *testing.T) {
	receipt := baseDischargeIntent()
	receipt.Operation = core.ReservationOpSettle
	leg := baseDischargeIntent()
	leg.Operation = core.ReservationOpSettlePartial

	a, err := core.CanonicalReservationDischargeDigest(receipt)
	require.NoError(t, err)
	b, err := core.CanonicalReservationDischargeDigest(leg)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "a settlement receipt and a settlement leg must not share a digest preimage")
}

// TestCanonicalReservationDischargeDigest_IsDomainSeparatedFromJournals
// pins that a journal signature can never be presented as a discharge
// signature. Both digests are SHA-256 over length-prefixed fields; only the
// leading domain byte guarantees they cannot collide by construction.
func TestCanonicalReservationDischargeDigest_IsDomainSeparatedFromJournals(t *testing.T) {
	// Same textual content in both preimages, as far as the two layouts
	// allow: if the domain byte were shared, these would be the closest two
	// digests in the package.
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	discharge, err := core.CanonicalReservationDischargeDigest(core.ReservationDischargeIntent{
		ReservationUID: "x",
		Operation:      "y",
		IdempotencyKey: "z",
		Amount:         decimal.Zero,
		RecordedAt:     at,
	})
	require.NoError(t, err)
	journal, err := core.CanonicalJournalDigest(core.JournalInput{
		JournalTypeUID: "x",
		IdempotencyKey: "y",
		Source:         "z",
	}, at)
	require.NoError(t, err)
	assert.NotEqual(t, journal, discharge)
}

// TestCanonicalReservationDischargeDigest_TruncatesToMicroseconds pins the
// same sub-microsecond fix canonicalTimestamp exists for: TIMESTAMPTZ stores
// microseconds, so a digest signed over a nanosecond-resolution instant
// could never be recomputed from the persisted row. macOS clocks hide this;
// Linux (production) does not.
func TestCanonicalReservationDischargeDigest_TruncatesToMicroseconds(t *testing.T) {
	in := baseDischargeIntent()
	withNanos := in
	withNanos.RecordedAt = in.RecordedAt.Add(999 * time.Nanosecond)

	a, err := core.CanonicalReservationDischargeDigest(in)
	require.NoError(t, err)
	b, err := core.CanonicalReservationDischargeDigest(withNanos)
	require.NoError(t, err)
	assert.Equal(t, a, b, "sub-microsecond digits must not reach the digest -- the database cannot store them")

	// And the location must not matter either: the same instant expressed in
	// a different zone is the same instant.
	shifted := in
	shifted.RecordedAt = in.RecordedAt.In(time.FixedZone("SGT", 8*3600))
	c, err := core.CanonicalReservationDischargeDigest(shifted)
	require.NoError(t, err)
	assert.Equal(t, a, c, "the digest must depend on the instant, not on the time.Time's location")
}

// stubVerifier accepts exactly one (digest, signature, keyID) triple.
type stubVerifier struct {
	digest    []byte
	signature []byte
	keyID     string
}

func (v stubVerifier) Verify(_ context.Context, digest, signature []byte, keyID string) error {
	if string(digest) == string(v.digest) && string(signature) == string(v.signature) && keyID == v.keyID {
		return nil
	}
	return assertVerifyMismatch
}

var assertVerifyMismatch = errStub("stubVerifier: mismatch")

type errStub string

func (e errStub) Error() string { return string(e) }

// TestVerifyReservationDischargeAuth pins the fail-closed cases. Every one
// of them makes the gate keep holding the reservation in full, which is why
// none of them may be reported as success.
func TestVerifyReservationDischargeAuth(t *testing.T) {
	ctx := context.Background()
	in := baseDischargeIntent()
	digest, err := core.CanonicalReservationDischargeDigest(in)
	require.NoError(t, err)
	good := stubVerifier{digest: digest, signature: []byte("sig"), keyID: "k1"}

	require.NoError(t, core.VerifyReservationDischargeAuth(ctx, good, in, digest, []byte("sig"), "k1"),
		"the happy path must verify, or every pin below passes for the wrong reason")

	t.Run("nil verifier", func(t *testing.T) {
		require.Error(t, core.VerifyReservationDischargeAuth(ctx, nil, in, digest, []byte("sig"), "k1"))
	})
	t.Run("no stored digest", func(t *testing.T) {
		require.Error(t, core.VerifyReservationDischargeAuth(ctx, good, in, nil, []byte("sig"), "k1"))
	})
	t.Run("stored digest does not match the row", func(t *testing.T) {
		tampered := in
		tampered.Amount = decimal.RequireFromString("999")
		require.Error(t, core.VerifyReservationDischargeAuth(ctx, good, tampered, digest, []byte("sig"), "k1"),
			"a row whose amount was edited must not verify against the digest that was signed")
	})
	t.Run("no signature", func(t *testing.T) {
		require.Error(t, core.VerifyReservationDischargeAuth(ctx, good, in, digest, nil, "k1"))
	})
	t.Run("no key id", func(t *testing.T) {
		require.Error(t, core.VerifyReservationDischargeAuth(ctx, good, in, digest, []byte("sig"), ""))
	})
	t.Run("verifier rejects", func(t *testing.T) {
		require.Error(t, core.VerifyReservationDischargeAuth(ctx, good, in, digest, []byte("other"), "k1"))
	})
}
