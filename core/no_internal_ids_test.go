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
// The banned-key derivation and the JSON-tag scan live in internal/idschema
// (board #28: this file and server/contract_pin_test.go used to each carry
// an independent ~55-line copy of the same regex plumbing -- core cannot
// import server's test file, since server already imports core and the
// reverse would cycle, but neither core nor server needs to import EACH
// OTHER's test file: both can import the dependency-free internal/idschema
// package instead, with zero cycle risk). See that package's doc comment
// for the full derivation rationale.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/azex-ai/ledger/internal/idschema"
)

// bannedInternalIDKeysForCore is a thin *testing.T-friendly wrapper around
// idschema.BannedKeys, kept so this file's tests read the same as before the
// board #28 dedup.
func bannedInternalIDKeysForCore(t *testing.T) map[string]bool {
	t.Helper()
	banned, err := idschema.BannedKeys("../postgres/sql/migrations")
	if err != nil {
		t.Fatal(err)
	}
	return banned
}

// scanCoreGoFilesForBannedJSONKeys is a thin *testing.T-friendly wrapper
// around idschema.ScanGoFilesForBannedKeys.
func scanCoreGoFilesForBannedJSONKeys(t *testing.T, dir string, banned map[string]bool) []idschema.Hit {
	t.Helper()
	hits, err := idschema.ScanGoFilesForBannedKeys(dir, banned)
	if err != nil {
		t.Fatal(err)
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
		t.Errorf("%s:%d exposes internal id key %q on a core type (I-18)", hit.File, hit.Line, hit.Key)
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
	if len(hits) != 1 || hits[0].Key != "classification_id" {
		t.Errorf("scan did not flag the planted classification_id violation -- got %v", hits)
	}
}
