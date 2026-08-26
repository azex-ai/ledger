// Package idschema mechanically derives, from the schema itself, the set of
// JSON/struct field key names that would leak an internal BIGSERIAL/IDENTITY
// primary key (I-18: external identity is the uid exclusively; internal ids
// exist only inside storage).
//
// This is the single implementation shared by two independent pins:
//   - server.TestContract_NoInternalIDKeysInJSON scans server/*.go's HTTP
//     request/response bodies.
//   - core.TestNoInternalIDFieldsInCoreTypes scans core/*.go's own type
//     definitions, catching a leak before it is ever wired into a handler.
//
// Board #28 (docs/audits/2026-08-25-financial-engineering/): before this
// package existed, both pins carried an independent ~55-line copy of this
// exact derivation logic (server cannot import core's test file and vice
// versa without a cycle -- server imports core). Two copies meant nothing
// enforced that improving one side's derivation rule also improved the
// other's; the second gate could silently fall behind. Neither `core` nor
// `server` package (non-test code) imports this package or is imported by
// it, so both test files can depend on it with zero cycle risk.
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
//     its own column name.
//  4. journal_entries is partitioned, and this schema does not FK into it
//     (checkpoint_rebuilds.previous_last_entry_id,
//     balance_checkpoints.last_entry_id, entry_attestations.entry_id all
//     carry a journal_entries row id with no literal REFERENCES). Any BIGINT
//     column whose name is exactly "entry_id" or ends in "_entry_id" is
//     banned by that naming convention instead.
//
// Deliberately NOT banned: account_holder / actor_id / holder_id / chain_id
// and similar external-namespace int64 identifiers -- none of them
// REFERENCES an internalPKTables entry and none match the entry_id shape
// (these are the caller's own namespace, not a storage detail).
package idschema

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// BannedKeys parses every *.up.sql file directly in migrationsDir and
// returns the mechanically-derived set of banned key names. Returns an error
// (never silently an empty/incomplete set) if no migration files are found,
// if zero surrogate-key tables are found (migration-parsing regression), or
// if the derived set falls below a sanity floor of 10 entries (the size of
// the hand-maintained word list this replaced) -- any of these would mean
// the gate that depends on this function has silently gone blind.
func BannedKeys(migrationsDir string) (map[string]bool, error) {
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		return nil, fmt.Errorf("idschema: glob %s: %w", migrationsDir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("idschema: no migration files found in %s -- schema-derivation would silently ban nothing (I-18 gate would go blind)", migrationsDir)
	}
	sort.Strings(files)

	var corpus strings.Builder
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("idschema: read %s: %w", f, err)
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
		return nil, fmt.Errorf("idschema: schema-derivation found zero surrogate-key tables -- migration parsing regressed, not that the schema lost every BIGSERIAL table")
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
		return nil, fmt.Errorf("idschema: schema-derived internal-id set only has %d entries (%v) -- migration parsing regressed", len(banned), banned)
	}

	return banned, nil
}

// Hit is one JSON-tagged field whose key is in a BannedKeys set.
type Hit struct {
	File string
	Line int
	Key  string
}

// ScanGoFilesForBannedKeys scans every non-test *.go file directly in dir
// (not recursive -- matches each pin's own package directory) for a
// `json:"<key>"` struct tag whose key is in banned.
func ScanGoFilesForBannedKeys(dir string, banned map[string]bool) ([]Hit, error) {
	jsonKey := regexp.MustCompile(`json:"([a-z0-9_]+)[,"]`)

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("idschema: glob %s: %w", dir, err)
	}
	var hits []Hit
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("idschema: read %s: %w", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, m := range jsonKey.FindAllStringSubmatch(line, -1) {
				if banned[m[1]] {
					hits = append(hits, Hit{File: f, Line: i + 1, Key: m[1]})
				}
			}
		}
	}
	return hits, nil
}
