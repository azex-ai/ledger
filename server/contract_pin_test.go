package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/azex-ai/ledger/internal/idschema"
)

// bannedInternalIDKeys mechanically derives, from the schema itself, the set
// of JSON keys that would leak an internal BIGSERIAL/IDENTITY primary key.
//
// The derivation itself lives in internal/idschema (board #28: this used to
// be an independent ~55-line copy of the same regex plumbing duplicated
// against core/no_internal_ids_test.go's bannedInternalIDKeysForCore --
// nothing enforced the two stayed in sync, so improving one side's rule
// silently left the other behind. server cannot import core's test file and
// vice versa without a cycle (server imports core), but both can import the
// dependency-free internal/idschema package with no cycle at all). See that
// package's doc comment for the full derivation rationale (surrogate-key
// tables, REFERENCES lookups, the partitioned journal_entries naming-shape
// rule, and what is deliberately NOT banned).
func bannedInternalIDKeys(t *testing.T) map[string]bool {
	t.Helper()
	banned, err := idschema.BannedKeys("../postgres/sql/migrations")
	if err != nil {
		t.Fatal(err)
	}
	return banned
}

// TestContract_NoInternalIDKeysInJSON pins invariant I-18: no HTTP request or
// response body may carry an internal bigint identifier. External identity is
// the uid (UUIDv7) exclusively; internal ids exist only inside storage.
//
// The pin is a mechanical source scan of every non-test file in this package:
// any struct json tag naming an internal id column -- as derived from the
// schema by bannedInternalIDKeys, not a hand-maintained list -- is a
// contract violation.
func TestContract_NoInternalIDKeysInJSON(t *testing.T) {
	banned := bannedInternalIDKeys(t)
	hits, err := idschema.ScanGoFilesForBannedKeys(".", banned)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range hits {
		t.Errorf("%s:%d exposes internal id key %q in a JSON body (I-18)", v.File, v.Line, v.Key)
	}
}

// scanGoFilesForBannedJSONKeys is a thin *testing.T-friendly wrapper around
// idschema.ScanGoFilesForBannedKeys, kept so this package's other pin test
// (TestContract_NoInternalIDKeysInJSON_CatchesSchemaColumnsMissedByOldWordList)
// reads the same as before the board #28 dedup.
func scanGoFilesForBannedJSONKeys(t *testing.T, dir string, banned map[string]bool) []idschema.Hit {
	t.Helper()
	hits, err := idschema.ScanGoFilesForBannedKeys(dir, banned)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// TestContract_NoInternalIDKeysInJSON_CatchesSchemaColumnsMissedByOldWordList
// regression-pins the actual gap test-credibility.md:140 found: the previous
// hardcoded word list did not contain policy_id, entry_id, or
// last_entry_id, so a handler using any of them in a JSON tag passed the old
// gate silently. This proves the schema-derived set now flags all three
// (and, for last_entry_id specifically, that the entry_id naming rule -- not
// just a REFERENCES lookup -- is doing real work, since journal_entries is
// partitioned and carries no literal REFERENCES to catch it by).
func TestContract_NoInternalIDKeysInJSON_CatchesSchemaColumnsMissedByOldWordList(t *testing.T) {
	banned := bannedInternalIDKeys(t)

	for _, key := range []string{"policy_id", "entry_id", "last_entry_id"} {
		if !banned[key] {
			t.Errorf("schema-derived banned set does not contain %q -- test-credibility.md:140's gap is still open", key)
		}
	}

	dir := t.TempDir()
	fixture := "package fixture\n\n" +
		"type missedIDs struct {\n" +
		"\tPolicyID    int64 `json:\"policy_id\"`\n" +
		"\tEntryID     int64 `json:\"entry_id\"`\n" +
		"\tLastEntryID int64 `json:\"last_entry_id\"`\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	hits := scanGoFilesForBannedJSONKeys(t, dir, banned)
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Key] = true
	}
	for _, key := range []string{"policy_id", "entry_id", "last_entry_id"} {
		if !got[key] {
			t.Errorf("scan did not flag %q in the fixture -- the gate would have missed it in a real handler too", key)
		}
	}
}
