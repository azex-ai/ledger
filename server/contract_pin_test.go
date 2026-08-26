package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// bannedInternalIDKeys mechanically derives, from the schema itself, the set
// of JSON keys that would leak an internal BIGSERIAL/IDENTITY primary key.
//
// This replaces a hand-maintained word list. The word list's failure mode is
// exactly what test-credibility.md:140 found: real internal id columns
// (policy_id, entry_id, last_entry_id, previous_last_entry_id,
// new_last_entry_id -- confirmed present in postgres/sqlcgen/models.go) were
// simply never added to it, so the gate never had a chance to catch a
// handler that used one. A list only protects against names someone
// remembered to type in; it does not shrink the set of ways to forget.
//
// Derivation, entirely from postgres/sql/migrations/*.up.sql:
//
//  1. Any table with a column declared `id BIGSERIAL` or
//     `id BIGINT GENERATED ALWAYS AS IDENTITY` has a surrogate integer
//     primary key -- that table joins internalPKTables.
//  2. "id" is always banned outright: it is every such table's own key,
//     whatever table it is read from.
//  3. Any column declared `... REFERENCES <table>(id)` -- inline, or via
//     `ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY (<col>) REFERENCES
//     <table>(id)` -- where <table> is in internalPKTables is banned under
//     its own column name. This alone reproduces the old list's
//     currency_id / classification_id / journal_type_id / template_id /
//     booking_id / reservation_id / journal_id / event_id / reversal_of,
//     PLUS the previously-missed policy_id (account_policy_changes.policy_id
//     REFERENCES account_policies(id)) -- with no name typed in by hand.
//  4. journal_entries is partitioned, and this schema does not FK into it
//     (checkpoint_rebuilds.previous_last_entry_id,
//     balance_checkpoints.last_entry_id, entry_attestations.entry_id all
//     carry a journal_entries row id with no literal REFERENCES). Any BIGINT
//     column whose name is exactly "entry_id" or ends in "_entry_id" is
//     banned by that naming convention instead -- a shape rule, not an
//     enumerated exception, so a future *_entry_id column is caught without
//     editing this test.
//
// Deliberately NOT banned, and NOT caught by any rule above: account_holder
// / actor_id / holder_id / chain_id and similar external-namespace int64
// identifiers -- none of them REFERENCES an internalPKTables entry and none
// match the entry_id shape (confirmed in
// docs/audits/2026-08-25-financial-engineering/structure.md's "移交": these
// are the caller's own namespace, not a storage detail).
func bannedInternalIDKeys(t *testing.T) map[string]bool {
	t.Helper()

	dir := "../postgres/sql/migrations"
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration files found in %s -- schema-derivation would silently ban nothing (I-18 gate would go blind)", dir)
	}
	sort.Strings(files)

	var corpus strings.Builder
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		corpus.Write(src)
		corpus.WriteByte('\n')
	}
	sql := corpus.String()

	// Step 1: tables with a surrogate integer primary key.
	tableBlock := regexp.MustCompile(`(?is)CREATE TABLE\s+(\w+)\s*\((.*?)\)\s*(PARTITION BY|;)`)
	idIsSurrogate := regexp.MustCompile(`(?im)^\s*id\s+(BIGSERIAL|BIGINT\s+GENERATED\s+ALWAYS\s+AS\s+IDENTITY)\b`)

	internalPKTables := map[string]bool{}
	for _, m := range tableBlock.FindAllStringSubmatch(sql, -1) {
		table, body := m[1], m[2]
		if idIsSurrogate.MatchString(body) {
			internalPKTables[table] = true
		}
	}
	if len(internalPKTables) == 0 {
		t.Fatal("schema-derivation found zero surrogate-key tables -- migration parsing regressed, not that the schema lost every BIGSERIAL table")
	}

	banned := map[string]bool{"id": true}

	// Step 3: REFERENCES <internal-pk-table>(id), inline and via ALTER TABLE.
	inlineRef := regexp.MustCompile(`(?i)(\w+)\s+BIGINT[^,\n]*REFERENCES\s+(\w+)\s*\(\s*id\s*\)`)
	for _, m := range inlineRef.FindAllStringSubmatch(sql, -1) {
		col, table := m[1], m[2]
		if internalPKTables[table] {
			banned[col] = true
		}
	}
	fkConstraint := regexp.MustCompile(`(?i)FOREIGN KEY\s*\(\s*(\w+)\s*\)\s*REFERENCES\s+(\w+)\s*\(\s*id\s*\)`)
	for _, m := range fkConstraint.FindAllStringSubmatch(sql, -1) {
		col, table := m[1], m[2]
		if internalPKTables[table] {
			banned[col] = true
		}
	}

	// Step 4: journal_entries row ids that never got a literal REFERENCES
	// because the table is partitioned -- caught by naming shape instead.
	bigintCol := regexp.MustCompile(`(?im)^\s*([a-z][a-z0-9_]*)\s+BIGINT\b`)
	for _, m := range bigintCol.FindAllStringSubmatch(sql, -1) {
		col := m[1]
		if col == "entry_id" || strings.HasSuffix(col, "_entry_id") {
			banned[col] = true
		}
	}

	// Sanity floor: the old hand list had 10 entries. Falling below that
	// means the parse broke, not that the schema shrank.
	if len(banned) < 10 {
		t.Fatalf("schema-derived internal-id set only has %d entries (%v) -- migration parsing regressed", len(banned), banned)
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
	for _, v := range scanGoFilesForBannedJSONKeys(t, ".", banned) {
		t.Errorf("%s:%d exposes internal id key %q in a JSON body (I-18)", v.file, v.line, v.key)
	}
}

type bannedKeyHit struct {
	file string
	line int
	key  string
}

// scanGoFilesForBannedJSONKeys scans every non-test *.go file directly in
// dir for a `json:"<key>"` struct tag whose key is in banned. Factored out
// of TestContract_NoInternalIDKeysInJSON so the derivation itself
// (bannedInternalIDKeys) can be pinned against a synthetic fixture directory
// below, independent of whatever server/*.go currently contains.
func scanGoFilesForBannedJSONKeys(t *testing.T, dir string, banned map[string]bool) []bannedKeyHit {
	t.Helper()

	jsonKey := regexp.MustCompile(`json:"([a-z0-9_]+)[,"]`)

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var hits []bannedKeyHit
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, m := range jsonKey.FindAllStringSubmatch(line, -1) {
				if banned[m[1]] {
					hits = append(hits, bannedKeyHit{file: f, line: i + 1, key: m[1]})
				}
			}
		}
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
		got[h.key] = true
	}
	for _, key := range []string{"policy_id", "entry_id", "last_entry_id"} {
		if !got[key] {
			t.Errorf("scan did not flag %q in the fixture -- the gate would have missed it in a real handler too", key)
		}
	}
}
