package core

// This file pins I-18's core-package clause directly: "not in the
// library-mode Go API (core types and interfaces speak uids exclusively)".
// server.TestContract_NoInternalIDKeysInJSON (server/contract_pin_test.go)
// already scans server/*.go's HTTP request/response bodies for banned JSON
// keys derived mechanically from the schema -- but that only catches a core
// type once someone wires it into a handler. This test scans core/*.go's
// own type definitions directly, so a core type carrying an internal id is
// caught even before it is ever put on the wire (the exact gap
// core.BalanceCheckpoint's pre-W15-B shape sat in: it never crossed into an
// HTTP body, but it was still a "core type" speaking a BIGSERIAL id, which
// I-18's original text banned outright).
//
// The banned-key derivation and the JSON-tag scan are re-implemented here,
// not imported from package server, because server already imports core --
// importing server back would be a cycle. Keeping the derivation logic
// itself mechanical (parsed from postgres/sql/migrations/*.up.sql, not a
// hand-maintained word list) is what matters; duplicating the ~40 lines of
// regex plumbing across the two packages is an acceptable cost to avoid the
// cycle. See server/contract_pin_test.go's bannedInternalIDKeys for the
// full derivation rationale, mirrored here.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// bannedInternalIDKeysForCore mirrors server.bannedInternalIDKeys exactly
// (same derivation rules, same migrations corpus) -- see that function's
// doc comment in server/contract_pin_test.go for the full rationale. Kept
// as an independent copy here rather than shared, since core must not
// import server (server imports core; the reverse would cycle).
func bannedInternalIDKeysForCore(t *testing.T) map[string]bool {
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

	bigintCol := regexp.MustCompile(`(?im)^\s*([a-z][a-z0-9_]*)\s+BIGINT\b`)
	for _, m := range bigintCol.FindAllStringSubmatch(sql, -1) {
		col := m[1]
		if col == "entry_id" || strings.HasSuffix(col, "_entry_id") {
			banned[col] = true
		}
	}

	if len(banned) < 10 {
		t.Fatalf("schema-derived internal-id set only has %d entries (%v) -- migration parsing regressed", len(banned), banned)
	}

	return banned
}

type coreBannedKeyHit struct {
	file string
	line int
	key  string
}

// scanCoreGoFilesForBannedJSONKeys scans every non-test *.go file directly
// in dir for a `json:"<key>"` struct tag whose key is in banned. Mirrors
// server.scanGoFilesForBannedJSONKeys.
func scanCoreGoFilesForBannedJSONKeys(t *testing.T, dir string, banned map[string]bool) []coreBannedKeyHit {
	t.Helper()

	jsonKey := regexp.MustCompile(`json:"([a-z0-9_]+)[,"]`)

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var hits []coreBannedKeyHit
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
					hits = append(hits, coreBannedKeyHit{file: f, line: i + 1, key: m[1]})
				}
			}
		}
	}
	return hits
}

// TestNoInternalIDFieldsInCoreTypes pins I-18: no exported core type may
// carry a field whose JSON key is an internal BIGSERIAL/IDENTITY id (as
// derived from the schema, not a hand-maintained list). This is stricter
// than server.TestContract_NoInternalIDKeysInJSON -- it holds regardless of
// whether the type is ever wired into an HTTP handler, matching I-18's
// original "core types and interfaces speak uids exclusively" wording.
func TestNoInternalIDFieldsInCoreTypes(t *testing.T) {
	banned := bannedInternalIDKeysForCore(t)
	for _, hit := range scanCoreGoFilesForBannedJSONKeys(t, ".", banned) {
		t.Errorf("%s:%d exposes internal id key %q on a core type (I-18)", hit.file, hit.line, hit.key)
	}
}

// TestNoInternalIDFieldsInCoreTypes_CatchesPlantedViolation regression-pins
// that the scan above actually works -- a scanner that silently matches
// nothing (e.g. because migration parsing regressed, or the glob pattern
// stopped matching core/*.go) would make TestNoInternalIDFieldsInCoreTypes
// vacuously green. Plants a fixture struct carrying a known-banned key
// (classification_id) in a temp directory and asserts the scan catches it.
func TestNoInternalIDFieldsInCoreTypes_CatchesPlantedViolation(t *testing.T) {
	banned := bannedInternalIDKeysForCore(t)
	if !banned["classification_id"] {
		t.Fatal("classification_id must be in the schema-derived banned set for this regression test to mean anything")
	}

	dir := t.TempDir()
	fixture := "package fixture\n\n" +
		"type leakedInternalID struct {\n" +
		"\tClassificationID int64 `json:\"classification_id\"`\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	hits := scanCoreGoFilesForBannedJSONKeys(t, dir, banned)
	if len(hits) != 1 || hits[0].key != "classification_id" {
		t.Errorf("scan did not flag the planted classification_id violation -- got %v", hits)
	}
}
