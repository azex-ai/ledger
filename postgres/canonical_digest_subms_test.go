package postgres_test

// C-R2 (2026-09-02 audit): I-46's three existing pins (core package) already
// use a deterministic `time.Date(..., 123456789, time.UTC)` fixture rather
// than `time.Now()`, so the clock-resolution dependency flagged by C-R2 does
// not actually exist there. What was still missing is a genuine Postgres
// round trip: TestVerifyJournalAuth_SurvivesTimestamptzRoundTrip
// (core/auth_test.go) only *simulates* the TIMESTAMPTZ floor with
// `.Truncate(time.Microsecond)` in Go -- it never signs, stores, and reads
// back through a real `TIMESTAMPTZ` column. This file closes that gap: sign
// with a genuinely sub-microsecond `EffectiveAt`, post it for real, then
// recompute the digest from what `JournalAuthMaterial` reads back from
// Postgres and require it verify.

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestPostJournal_SignedAtSubMicrosecondEffectiveAtStillVerifies pins I-46
// end-to-end against a real database: PostJournal with an EffectiveAt
// carrying a genuine nanosecond remainder (what a real nanosecond-resolution
// clock produces -- unreachable via time.Now() on this development
// machine's clock, so constructed explicitly, same as core's pins) must
// still pass core.VerifyJournalAuth once read back through Postgres's
// TIMESTAMPTZ column, which can only ever persist microsecond precision.
func TestPostJournal_SignedAtSubMicrosecondEffectiveAtStillVerifies(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9501

	attestor, verifier := newTestAttestor(t, "ed25519-i46-subms-roundtrip")
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)

	input := f.journalInput(holder, postgrestest.UniqueKey("i46-subms-roundtrip"), decimal.NewFromInt(75))
	// A genuine nanosecond remainder -- the exact shape a real Linux clock
	// produces and this development machine's time.Now() cannot.
	input.EffectiveAt = time.Date(2026, 8, 21, 12, 0, 0, 123456789, time.UTC)

	journal, err := ledgerStore.PostJournal(ctx, input)
	require.NoError(t, err)

	journalID := postgrestest.InternalID(t, pool, "journals", journal.UID)
	attestStore := postgres.NewAttestationStore(pool)
	materials, err := attestStore.JournalAuthMaterial(ctx, []int64{journalID})
	require.NoError(t, err)
	m, ok := materials[journalID]
	require.True(t, ok, "JournalAuthMaterial must return the posted journal")

	// Sanity: Postgres actually discarded the nanosecond remainder -- this
	// is the floor the fix depends on, not an assumption.
	require.Equal(t, m.EffectiveAt, m.EffectiveAt.Truncate(time.Microsecond),
		"sanity: TIMESTAMPTZ must not have round-tripped the raw nanosecond value")

	err = core.VerifyJournalAuth(ctx, verifier, m.Input, m.EffectiveAt, m.AuthDigest, m.AuthSignature, m.AuthKeyID)
	require.NoError(t, err, "a journal signed with a sub-microsecond EffectiveAt must still verify after a real TIMESTAMPTZ round trip")
}
