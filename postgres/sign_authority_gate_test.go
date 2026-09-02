package postgres_test

// I-50: the sign convention has exactly one implementation per runtime, and
// THAT FACT IS CHECKED BY MACHINE.
//
// I-43 already said the convention is collapsed into core.Sign (Go) and
// ledger_signed_amount / ledger_signed_delta (SQL). What it did not have was
// any way to notice a new implementation appearing next to them: its
// "Enforced by" was a hand-written list of five SQL files in
// docs/INVARIANTS.md. Two separate hand counts -- the 2026-08-25 audit's "10
// SQL expressions" and commit 15d110e's "9" -- both missed
// balance_trends.sql, which was the eighteenth copy AND the only one whose
// answer was wrong: it reported a 500 deposit as a 500 OUTFLOW for a year,
// because it tested entry_type without joining classifications at all.
//
// A hand-maintained list cannot catch the file nobody thought to look at.
// This gate is the machine-checked replacement, in the shape
// grant_coverage_test.go uses for tables: enumerate every candidate
// expression in the tree, require each one to be explicitly classified, and
// FAIL on anything unclassified. The list in docs/INVARIANTS.md is now the
// gate's output, not its source of truth.
//
// Three things are enforced:
//
//  1. SQL -- no query may derive a signed amount from entry_type outside
//     ledger_signed_amount / ledger_signed_delta, unless the (file, query)
//     it lives in is classified below with a reason.
//  2. Go -- no non-test file may branch on core.NormalSide* outside
//     core.Sign, unless classified below.
//  3. The "what money can the holder see" predicate has exactly one spelling
//     across every query that asks it. That is not a sign question, but it is
//     the same failure mode -- four copies of one predicate, one of them
//     updated (2026-08-26's M-4) and three not, which is how withdrawal fees
//     vanished from user statements (audit A-M3).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqlSignExemptions maps "<file>:<query name>" to the reason the bare
// entry_type arithmetic inside it is NOT a copy of the normal_side sign
// convention, plus how many such expressions that query is allowed to
// contain. Both halves matter: an unlisted query fails, and so does a listed
// query that grew an extra expression.
//
// Every member here has the same justification in different words -- the
// expression computes DEBITS MINUS CREDITS, or splits the two into separate
// columns, which is a statement about a journal balancing rather than about
// which direction an account's balance moved. normal_side is irrelevant to
// it by construction. Anything that is not that must go through
// ledger_signed_amount.
var sqlSignExemptions = map[string]struct {
	count  int
	reason string
}{
	"integrity_balance.sql:IntegrityUnbalancedJournalsCount": {1,
		"per-journal debit-minus-credit: finds journals that do not balance, which is true or false regardless of any classification's normal_side"},
	"integrity_balance.sql:IntegrityUnbalancedJournalsSample": {2,
		"same check as the count query, plus the drift figure it reports"},
	"journals.sql:VerifyJournalBalanced": {1,
		"the write path's own balance assertion on a single journal"},
	"reconcile.sql:ReconcileAccountingEquation": {2,
		"splits debit and credit into two columns and hands both to the caller, which applies ledger_signed_delta"},
	"reconcile.sql:ReconcileNonNegativeBalances": {4,
		"same split-into-two-columns shape; the sign is applied by ledger_signed_delta in the HAVING clause"},
	"reconcile.sql:ReconcileRoleLessLiabilities": {4,
		"same split-into-two-columns shape; the sign is applied by ledger_signed_delta in the HAVING clause"},
	"trial_balance.sql:TrialBalanceRows": {2,
		"a trial balance IS the debit and credit columns; presenting them separately is the report"},
}

var (
	sqlQueryNameRE        = regexp.MustCompile(`^--\s*name:\s*(\S+)`)
	signAuthoritySQLFuncs = []string{"ledger_signed_amount", "ledger_signed_delta"}

	// A normal_side constant only reimplements the sign convention when it is
	// BRANCHED ON. Declaring a classification's polarity
	// (`NormalSide: core.NormalSideCredit`) is the input to the convention,
	// not a copy of it, and presets/ is nothing but such declarations.
	normalSideBranchRE = regexp.MustCompile(
		`(==|!=)\s*core\.NormalSide(Debit|Credit)` +
			`|core\.NormalSide(Debit|Credit)\s*(==|!=)` +
			`|\bcase\s+core\.NormalSide(Debit|Credit)` +
			`|\bswitch\s+[^\n]*\bnormalSide\b`)
)

func TestSignAuthorityGate_SQLHasNoUnclassifiedEntryTypeArithmetic(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("sql", "queries", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "the gate must actually find the queries it claims to scan")

	found := map[string]int{}

	for _, path := range files {
		body, err := os.ReadFile(path)
		require.NoError(t, err)

		base := filepath.Base(path)
		query := "(preamble)"
		for i, line := range strings.Split(string(body), "\n") {
			if m := sqlQueryNameRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				query = m[1]
				continue
			}
			if !isBareEntryTypeArithmetic(line) {
				continue
			}
			key := base + ":" + query
			found[key]++
			if _, ok := sqlSignExemptions[key]; !ok {
				t.Errorf(
					"%s:%d (query %s) derives an amount from entry_type without going through %s:\n\t%s\n\n"+
						"Either use ledger_signed_amount(c.normal_side, je.entry_type, je.amount), or -- if this really is a\n"+
						"normal_side-independent debit-minus-credit check -- add %q to sqlSignExemptions with a reason.\n"+
						"Do not skip this by making the line unmatchable: the last query that tested entry_type directly\n"+
						"reported deposits as withdrawals to every consumer of /holders/{h}/trends.",
					path, i+1, query, strings.Join(signAuthoritySQLFuncs, " / "), strings.TrimSpace(line), key,
				)
			}
		}
	}

	for key, exempt := range sqlSignExemptions {
		got := found[key]
		if got == 0 {
			t.Errorf("stale exemption %q (%s): no matching expression exists any more -- delete the entry", key, exempt.reason)
			continue
		}
		assert.Equalf(t, exempt.count, got,
			"%q is exempted for %d expression(s) (%s) but now has %d; review the new one on its own merits rather than inheriting the exemption",
			key, exempt.count, exempt.reason, got)
	}
}

// isBareEntryTypeArithmetic reports whether a SQL line turns entry_type into a
// signed or filtered amount by hand. It deliberately does not try to parse
// SQL: the point is to be noisy about anything shaped like the bug and force
// a human classification, not to be exact.
func isBareEntryTypeArithmetic(line string) bool {
	low := strings.ToLower(line)
	if i := strings.Index(low, "--"); i >= 0 {
		low = low[:i]
	}
	if !strings.Contains(low, "entry_type") || !strings.Contains(low, "amount") {
		return false
	}
	if !strings.Contains(low, "case") {
		return false
	}
	for _, fn := range signAuthoritySQLFuncs {
		if strings.Contains(low, fn) {
			return false
		}
	}
	return true
}

// goSignExemptions maps "<file>:<line content fragment>" to why a non-test Go
// file may compare against core.NormalSide* outside core.Sign.
var goSignExemptions = map[string]string{
	"core/types.go":        "the declaration of the constants themselves, plus NormalSide.IsValid",
	"service/rollup.go":    "asks whether a NEGATIVE balance is anomalous for this account, which is a different question from what sign an entry carries; the balance it tests was already produced by core.Delta",
	"service/reconcile.go": "buckets an already-signed net (from core.Delta, which rejects invalid input two lines earlier) into debit-normal and credit-normal groups for reporting",
}

func TestSignAuthorityGate_GoHasNoUnclassifiedNormalSideBranch(t *testing.T) {
	var offenders []string

	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "web", "sqlcgen", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, "../"))
		if rel == "core/account_policy.go" {
			return nil // core.Sign itself: the authority.
		}
		for i, line := range strings.Split(string(body), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			if !normalSideBranchRE.MatchString(code) {
				continue
			}
			if _, ok := goSignExemptions[rel]; ok {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
		}
		return nil
	})
	require.NoError(t, err)

	sort.Strings(offenders)
	assert.Emptyf(t, offenders,
		"these non-test files branch on a normal_side constant outside core.Sign:\n\t%s\n\n"+
			"Use core.Sign / core.SignedAmount / core.Delta. If the branch genuinely asks a different question\n"+
			"(\"is a negative balance anomalous here?\", \"which reporting bucket does this net belong in?\"),\n"+
			"add the file to goSignExemptions with that reason.",
		strings.Join(offenders, "\n\t"))

	// A stale exemption is as bad as a missing one: it silently pre-approves
	// whatever gets written into that file next.
	for rel := range goSignExemptions {
		body, err := os.ReadFile(filepath.Join("..", rel))
		require.NoErrorf(t, err, "exempted file %s does not exist", rel)
		assert.Containsf(t, string(body), "NormalSide",
			"stale exemption: %s no longer mentions NormalSide", rel)
	}
}

// TestSignAuthorityGate_HolderVisibleMoneyPredicateHasOneSpelling is A-N4's
// pin. `balance_role NOT IN (”, 'memo')` answers two questions the ledger
// treats as one: what the platform OWES (solvency's liability side) and what
// money the HOLDER can see (their statement, their currency list). Four
// copies existed; the 2026-08-26 M-4 fix updated one of them.
//
// The consequence of the three that were missed: retagging fee_expense to
// 'memo' pulled it INTO the holder aggregate ('memo' <> ” is true), where
// withdraw_fee's +5 memo leg and -5 locked leg netted to zero and the row was
// dropped as empty. A user's balance fell by 5 with no line item anywhere to
// account for it.
//
// So the copies are pinned to be character-for-character identical, and a new
// core.BalanceRole value cannot be added without both meanings being reviewed
// at once.
func TestSignAuthorityGate_HolderVisibleMoneyPredicateHasOneSpelling(t *testing.T) {
	const canonical = "balance_role NOT IN ('', 'memo')"

	// file -> how many copies of the predicate it must contain.
	want := map[string]int{
		"holder.sql":            3, // page_journals, the statement projection, ListHolderCurrencies
		"platform_balances.sql": 1, // GetTotalUserSideBalance (the liability side)
		"reconcile.sql":         1, // the untagged-holder_kind sweep
	}

	files, err := filepath.Glob(filepath.Join("sql", "queries", "*.sql"))
	require.NoError(t, err)

	// Any spelling of a balance_role filter that is not the canonical one, and
	// not the deliberate inverse (`balance_role = ''`, the untagged-
	// classification detector in ReconcileRoleLessLiabilities), is a fork.
	variantRE := regexp.MustCompile(`balance_role\s*(<>|!=|NOT IN|IN)\s*[^\n]*`)

	got := map[string]int{}
	for _, path := range files {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		base := filepath.Base(path)

		for i, line := range strings.Split(string(body), "\n") {
			code := line
			if j := strings.Index(code, "--"); j >= 0 {
				code = code[:j]
			}
			m := variantRE.FindString(code)
			if m == "" {
				continue
			}
			if !strings.Contains(m, canonical) {
				t.Errorf("%s:%d spells the holder-visible-money predicate differently:\n\t%s\nwant exactly: %s",
					path, i+1, strings.TrimSpace(line), canonical)
				continue
			}
			got[base]++
		}
	}

	assert.Equal(t, want, got,
		"the set of queries asking \"what money can the holder see / what does the platform owe\" changed; "+
			"a new one must use the canonical predicate and be counted here, and a removed one must be dropped from the map")
}
