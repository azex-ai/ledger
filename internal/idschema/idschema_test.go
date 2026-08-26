package idschema_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/azex-ai/ledger/internal/idschema"
)

const realMigrationsDir = "../../postgres/sql/migrations"

func TestBannedKeys_DerivesKnownInternalIDColumns(t *testing.T) {
	banned, err := idschema.BannedKeys(realMigrationsDir)
	if err != nil {
		t.Fatal(err)
	}

	// The set the old hand-maintained word list covered.
	for _, key := range []string{
		"id", "currency_id", "classification_id", "journal_type_id",
		"template_id", "booking_id", "reservation_id", "journal_id",
		"event_id", "reversal_of",
	} {
		if !banned[key] {
			t.Errorf("expected %q to be schema-derived as banned", key)
		}
	}

	// The columns test-credibility.md:140 found the old hand list missed.
	for _, key := range []string{"policy_id", "entry_id", "last_entry_id"} {
		if !banned[key] {
			t.Errorf("expected %q (test-credibility.md's gap) to be schema-derived as banned", key)
		}
	}

	// Deliberately NOT banned: external-namespace identifiers.
	for _, key := range []string{"account_holder", "actor_id", "holder_id", "chain_id"} {
		if banned[key] {
			t.Errorf("%q is an external-namespace identifier and must NOT be banned", key)
		}
	}
}

func TestBannedKeys_NoMigrationsDirIsAnError(t *testing.T) {
	_, err := idschema.BannedKeys(t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a directory with no *.up.sql files -- a silent empty set would make every downstream pin vacuously pass")
	}
}

func TestScanGoFilesForBannedKeys_FindsAndSkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	banned := map[string]bool{"classification_id": true}

	prod := "package fixture\n\ntype T struct {\n\tClassificationID int64 `json:\"classification_id\"`\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "prod.go"), []byte(prod), 0o600); err != nil {
		t.Fatal(err)
	}
	// A _test.go file carrying the same banned key must be skipped -- the
	// scan is for shipped struct definitions, not test fixtures.
	testFile := "package fixture\n\ntype TTest struct {\n\tClassificationID int64 `json:\"classification_id\"`\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "prod_test.go"), []byte(testFile), 0o600); err != nil {
		t.Fatal(err)
	}

	hits, err := idschema.ScanGoFilesForBannedKeys(dir, banned)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 hit (from prod.go only), got %d: %v", len(hits), hits)
	}
	if hits[0].Key != "classification_id" || filepath.Base(hits[0].File) != "prod.go" {
		t.Errorf("unexpected hit: %+v", hits[0])
	}
}

func TestScanGoFilesForBannedKeys_NoFalsePositiveOnUnbannedKey(t *testing.T) {
	dir := t.TempDir()
	banned := map[string]bool{"classification_id": true}

	prod := "package fixture\n\ntype T struct {\n\tAccountHolder int64 `json:\"account_holder\"`\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "prod.go"), []byte(prod), 0o600); err != nil {
		t.Fatal(err)
	}

	hits, err := idschema.ScanGoFilesForBannedKeys(dir, banned)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("account_holder is not banned, expected 0 hits, got %v", hits)
	}
}
