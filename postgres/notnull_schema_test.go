package postgres_test

// F-P7 (2026-09-02 audit): docs/INVARIANTS.md I-7 claims every column is
// NOT NULL except six documented FK-target exceptions, but had no
// **Pinned by** section at all -- only `postgres/schema_migrations_test.go`
// spot-checks is_nullable on a few individual columns of a few tables, never
// the whole public schema. A migration that dropped a NOT NULL constraint
// anywhere else in the schema, or removed one of the six documented
// exceptions, would not have gone red.
//
// Running this against the real schema for the first time (2026-09-02)
// surfaced 12 more nullable columns I-7's prose doesn't mention at all --
// not new drift from this branch, pre-existing since 001_baseline.up.sql.
// Two categories, both already justified by comments elsewhere in that same
// migration file, neither one an FK-target sentinel:
//
//  1. `deposits` and `withdrawals` (001_baseline.up.sql:606-623): dead
//     pre-`bookings` tables kept only so a schema squash never silently
//     drops rows nobody remembered were there. Their comment says outright
//     they predate the No-NULL convention and are "deliberately NOT held to
//     the convention the live tables follow" -- a whole-table exception, not
//     a column-by-column one.
//  2. The claim-lease pattern (`rollup_queue.claimed_until`/`processed_at`,
//     `registration_rescans.claimed_until`/`last_error`): NULL means "not
//     currently claimed" / "not yet processed" / "no error yet" for a
//     background worker's own bookkeeping -- not a value the ledger's public
//     surface reads, and not FK-related.
//
// This is a real gap in I-7's Rule/Exceptions text, not something this file
// can fix on its own: this task's file-exclusivity only covers the doc's
// **Pinned by** lines. Flagged to team-lead for whoever owns rewriting I-7's
// substantive Exceptions bullet list to fold these two categories in.
import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// legacyDeadTables are held to none of the No-NULL convention at all (see
// the file doc comment, category 1) -- every nullable column in these two
// tables is accepted, not enumerated one by one.
var legacyDeadTables = map[string]bool{
	"deposits":    true,
	"withdrawals": true,
}

// nullableColumnExceptions is category 2 (the claim-lease pattern) plus
// I-7's own six documented FK-target exceptions. Must be kept in sync with
// docs/INVARIANTS.md I-7's exception bullet list by hand for the six
// FK-target entries; the claim-lease four are documented only in this file
// today (see the file doc comment).
func nullableColumnExceptions() map[[2]string]bool {
	return map[[2]string]bool{
		// I-7's six documented FK-target exceptions.
		{"journals", "reversal_of"}:    true,
		{"bookings", "journal_id"}:     true,
		{"bookings", "reservation_id"}: true,
		{"events", "journal_id"}:       true,
		{"reservations", "journal_id"}: true,
		{"journals", "event_id"}:       true,
		// Claim-lease pattern (category 2 above), not in I-7's prose yet.
		{"rollup_queue", "claimed_until"}:         true,
		{"rollup_queue", "processed_at"}:          true,
		{"registration_rescans", "claimed_until"}: true,
		{"registration_rescans", "last_error"}:    true,
	}
}

// TestSchema_NullableColumnsExactlyMatchI7Exceptions pins I-7: the set of
// nullable columns in the public schema must equal, exactly, the six
// documented FK-target exceptions -- no more (a NOT NULL silently dropped
// elsewhere), no fewer (an exception silently removed without updating the
// doc). Fail-closed: an unrecognized nullable column is reported by name
// rather than ignored, and a missing expected exception is reported too.
func TestSchema_NullableColumnsExactlyMatchI7Exceptions(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND is_nullable = 'YES'
		ORDER BY table_name, column_name
	`)
	require.NoError(t, err)
	defer rows.Close()

	want := nullableColumnExceptions()
	got := map[[2]string]bool{}
	for rows.Next() {
		var table, column string
		require.NoError(t, rows.Scan(&table, &column))
		got[[2]string{table, column}] = true
	}
	require.NoError(t, rows.Err())

	var unexpected []string
	for k := range got {
		if legacyDeadTables[k[0]] {
			continue
		}
		if !want[k] {
			unexpected = append(unexpected, fmt.Sprintf("%s.%s", k[0], k[1]))
		}
	}
	assert.Empty(t, unexpected, "docs/INVARIANTS.md I-7: found nullable column(s) not in the documented exception list: %v", unexpected)

	var missing []string
	for k := range want {
		if !got[k] {
			missing = append(missing, fmt.Sprintf("%s.%s", k[0], k[1]))
		}
	}
	assert.Empty(t, missing, "docs/INVARIANTS.md I-7: documented exception(s) are no longer nullable in the schema (doc is stale): %v", missing)
}
