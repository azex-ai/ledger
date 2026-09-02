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
// vacuously green. Plants fixture structs carrying a known-banned key
// (classification_id) in a temp directory and asserts the scan catches them.
//
// H-m1: the untagged fixture below is the one that matters. The old scan was
// a regex over `json:"..."`, so it could only ever see the TAGGED case --
// and the fixture it planted was tagged, which is why the pin "proved" a
// scanner that was blind to every untagged exported field. An untagged
// exported field still serializes (pkg/httpx snake_cases it, encoding/json
// names it), and core.AttestedEntry sat in exactly that blind spot in-tree
// with the gate green.
func TestNoInternalIDFieldsInCoreTypes_CatchesPlantedViolation(t *testing.T) {
	banned := bannedInternalIDKeysForCore(t)
	if !banned["classification_id"] {
		t.Fatal("classification_id must be in the schema-derived banned set for this regression test to mean anything")
	}

	cases := []struct {
		name    string
		fixture string
	}{
		{
			name: "tagged",
			fixture: "package fixture\n\n" +
				"type leakedInternalID struct {\n" +
				"\tClassificationID int64 `json:\"classification_id\"`\n" +
				"}\n",
		},
		{
			// No tag at all: the shape the previous regex-based scan could
			// not see.
			name: "untagged",
			fixture: "package fixture\n\n" +
				"type LeakedInternalID struct {\n" +
				"\tClassificationID int64\n" +
				"}\n",
		},
		{
			// A tag that names something else entirely, with no json entry.
			name: "tagged without a json entry",
			fixture: "package fixture\n\n" +
				"type LeakedInternalID struct {\n" +
				"\tClassificationID int64 `db:\"classification_id\"`\n" +
				"}\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(tc.fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			hits := scanCoreGoFilesForBannedJSONKeys(t, dir, banned)
			if len(hits) != 1 || hits[0].Key != "classification_id" {
				t.Errorf("scan did not flag the planted classification_id violation -- got %v", hits)
			}
		})
	}
}

// TestNoInternalIDsInCoreInterfaceSignatures pins the half of I-18 its own
// text always claimed ("core types AND INTERFACES speak uids exclusively")
// and no gate ever checked: an interface method parameter is not a struct
// field and carries no json tag, so a tag-based scan is structurally unable
// to see it. core.Metrics took currencyID int64 on four methods -- an
// internal BIGSERIAL primary key handed to every consumer implementing the
// interface, and published by the library's own implementation as a
// Prometheus label (H-M9).
// knownInterfaceInternalIDLeaks is empty, which is the end state and not an
// accident of never having been used.
//
// It held the four core.Metrics methods that took `currencyID int64` on the
// day this gate was written (H-M9): recorded rather than silently tolerated,
// because fixing them was a signature change to core.Metrics -- breaking for
// every consumer implementing it -- and therefore owned by a different task
// in this wave (contracts §4, D-ops). When that fix landed, the list's OTHER
// direction went red naming all four ("no longer leaks -- delete the entry"),
// which is how it came to be empty instead of quietly outliving the problem
// it described.
//
// Both directions, restated because the value is entirely in the second one:
// a new violation is red (it is not listed), and a listed one that stops
// leaking is red too (the entry must be deleted in the same commit as the
// fix). Never add to this map to quiet a gate -- an internal id in a core
// interface parameter is I-18's subject, not a style preference.
var knownInterfaceInternalIDLeaks = map[string]string{}

func TestNoInternalIDsInCoreInterfaceSignatures(t *testing.T) {
	banned := bannedInternalIDKeysForCore(t)

	hits, err := idschema.ScanInterfaceParamsForBannedKeys(".", banned)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]string{}
	for _, hit := range hits {
		if knownInterfaceInternalIDLeaks[hit.Owner] == hit.Key {
			found[hit.Owner] = hit.Key
			continue
		}
		t.Errorf("%s:%d: %s takes parameter %q, an internal storage id (I-18: core interfaces speak uids exclusively) -- take the uid instead",
			hit.File, hit.Line, hit.Owner, hit.Key)
	}

	for owner, key := range knownInterfaceInternalIDLeaks {
		if found[owner] != key {
			t.Errorf("knownInterfaceInternalIDLeaks records %s taking %q, which no longer leaks -- delete the entry (the whole point of the list is that it shrinks to nothing)", owner, key)
		}
	}
}

// TestNoInternalIDsInCoreInterfaceSignatures_CatchesPlantedViolation is the
// regression pin for the scan above: it must actually flag a planted
// interface parameter, or the test is decorative.
func TestNoInternalIDsInCoreInterfaceSignatures_CatchesPlantedViolation(t *testing.T) {
	banned := bannedInternalIDKeysForCore(t)

	dir := t.TempDir()
	fixture := "package fixture\n\n" +
		"type Leaky interface {\n" +
		"\tObserve(classCode string, currencyID int64)\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	hits, err := idschema.ScanInterfaceParamsForBannedKeys(dir, banned)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Key != "currency_id" {
		t.Errorf("scan did not flag the planted currencyID parameter -- got %v", hits)
	}
}

// TestInternalIDAllowlistIsAccurate keeps idschema.AllowedInternalIDTypes
// from outliving the types it exempts: a stale entry would silently exempt a
// future type that reuses the name. Every allowlisted type must exist in
// core, and every entry must carry a reason.
func TestInternalIDAllowlistIsAccurate(t *testing.T) {
	missing, err := idschema.VerifyAllowlist(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("idschema.AllowedInternalIDTypes exempts type(s) %v that core no longer declares -- delete the entries", missing)
	}
	for name, reason := range idschema.AllowedInternalIDTypes {
		if reason == "" {
			t.Errorf("allowlisted type %q carries no reason -- an exemption without a stated reason is indistinguishable from an oversight", name)
		}
	}
}
