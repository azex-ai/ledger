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
//     across every query that asks it, AND every holder-facing query that
//     reads journal_entries actually applies it. That is not a sign
//     question, but it is the same failure mode -- four copies of one
//     predicate, one of them updated (2026-08-26's M-4) and three not, which
//     is how withdrawal fees vanished from user statements (audit A-M3).
//
// Three findings from the W3 adversarial review of this gate shaped the
// implementation below, all three reproduced by mutation:
//
//   - M-2: the SQL check matched entry_type + amount + case on ONE line, so
//     writing the same CASE across four lines -- the natural SQL layout --
//     made it invisible. The gate's own error message said "do not skip this
//     by making the line unmatchable"; a line break did exactly that. It now
//     works on whole `-- name:` blocks with whitespace flattened.
//   - M-3: the predicate check pinned the spelling of the copies that
//     already existed, not the presence of the predicate where it belongs.
//     Adding a new holder aggregate that simply forgot it -- A-M3's original
//     shape -- passed. There is now a coverage half.
//   - M-4: the Go check only recognized the named constants, so
//     `if string(side) == "debit"` was a complete second implementation the
//     regexp could not see, and the file-level exemptions pre-approved
//     anything later written into those three files. Literal comparisons now
//     count, and each exemption pins how many matches its file may contain.
//   - m-5: neither half scanned postgres/sqlcgen/, where the generated copy
//     of every query lives. The SQL half now reads it too (the generated
//     consts carry the same `-- name:` header), so reintroducing a bare CASE
//     there -- which only `sqlc diff` would otherwise catch, and only
//     indirectly -- is red here as well.

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
	// Unanchored on purpose: in postgres/sqlcgen the same header sits inside
	// a Go const declaration (`const q = `+"`"+`-- name: X :many`+"`"+`), so
	// requiring column 0 would have filed every generated query under
	// "(preamble)" (m-5).
	sqlQueryNameRE        = regexp.MustCompile(`--\s*name:\s*(\S+)`)
	signAuthoritySQLFuncs = []string{"ledger_signed_amount", "ledger_signed_delta"}

	// A normal_side constant only reimplements the sign convention when it is
	// BRANCHED ON. Declaring a classification's polarity
	// (`NormalSide: core.NormalSideCredit`) is the input to the convention,
	// not a copy of it, and presets/ is nothing but such declarations.
	//
	// The last two alternatives are M-4: `core.NormalSide` is a string type,
	// so `if string(side) == "debit"` is a complete second implementation of
	// the convention that the constant-name patterns above cannot see. The
	// reviewer wrote exactly that (a mutSignedAmount in rollup_adapter.go)
	// and the gate stayed green. Comparing against the bare literal is now
	// itself the offence -- use core.NormalSideDebit / core.EntryTypeDebit,
	// whichever the value is, and let core.Sign do the branching.
	normalSideBranchRE = regexp.MustCompile(
		`(==|!=)\s*core\.NormalSide(Debit|Credit)` +
			`|core\.NormalSide(Debit|Credit)\s*(==|!=)` +
			`|\bcase\s+core\.NormalSide(Debit|Credit)` +
			`|\bswitch\s+[^\n]*\bnormalSide\b` +
			`|(==|!=)\s*"(debit|credit)"` +
			`|"(debit|credit)"\s*(==|!=)` +
			`|\bcase\s+"(debit|credit)"`)
)

// sqlBlock is one `-- name: X` query (or a file's preamble), with comments
// stripped, lowercased, and all whitespace flattened to single spaces -- so a
// CASE expression reads the same whether its author wrote it on one line or
// on five (M-2).
type sqlBlock struct {
	file  string // basename, e.g. "reconcile.sql" or "reconcile.sql.go"
	query string
	text  string // normalized
	line  int    // 1-based line where the block starts, for the error message
}

func (b sqlBlock) key() string { return b.file + ":" + b.query }

// signAuthoritySQLPaths are every file that carries query text: the sources
// and postgres/sqlcgen's generated copies of them (m-5). The generated consts
// begin with the same `-- name: X :many` header, so one parser reads both.
func signAuthoritySQLPaths(t *testing.T) []string {
	t.Helper()
	queries, err := filepath.Glob(filepath.Join("sql", "queries", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, queries, "the gate must actually find the queries it claims to scan")
	generated, err := filepath.Glob(filepath.Join("sqlcgen", "*.sql.go"))
	require.NoError(t, err)
	require.NotEmpty(t, generated, "the gate must actually find the generated copies it claims to scan")
	return append(queries, generated...)
}

// commentStrippedSQLLine removes a trailing `-- ...` comment. In a generated
// .go file the `-- name:` header itself is a comment, so the caller must read
// the query name BEFORE calling this.
func commentStrippedSQLLine(line string) string {
	if i := strings.Index(line, "--"); i >= 0 {
		return line[:i]
	}
	return line
}

func parseSQLBlocks(t *testing.T, paths []string) []sqlBlock {
	t.Helper()
	var out []sqlBlock
	for _, path := range paths {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		base := filepath.Base(path)

		cur := sqlBlock{file: base, query: "(preamble)", line: 1}
		var sb strings.Builder
		flush := func() {
			cur.text = strings.Join(strings.Fields(strings.ToLower(sb.String())), " ")
			out = append(out, cur)
			sb.Reset()
		}
		for i, line := range strings.Split(string(body), "\n") {
			if m := sqlQueryNameRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				flush()
				cur = sqlBlock{file: base, query: m[1], line: i + 1}
				continue
			}
			sb.WriteString(commentStrippedSQLLine(line))
			sb.WriteByte(' ')
		}
		flush()
	}
	return out
}

// caseSpanRE matches one flattened `case ... end` expression, non-greedily so
// sibling CASEs in the same block are separate matches.
var caseSpanRE = regexp.MustCompile(`case\b.*?\bend\b`)

// bareEntryTypeSpans returns the `case ... end` expressions in a normalized
// block that turn entry_type into an amount without going through the sign
// authority.
func bareEntryTypeSpans(text string) []string {
	var out []string
	for _, span := range caseSpanRE.FindAllString(text, -1) {
		if !strings.Contains(span, "entry_type") || !strings.Contains(span, "amount") {
			continue
		}
		authoritative := false
		for _, fn := range signAuthoritySQLFuncs {
			if strings.Contains(span, fn) {
				authoritative = true
			}
		}
		if !authoritative {
			out = append(out, span)
		}
	}
	return out
}

// expandedSQLSignExemptions mirrors every exemption onto the generated copy
// of the same query (`reconcile.sql:Q` also covers `reconcile.sql.go:Q`), so
// scanning sqlcgen does not mean maintaining the classification twice.
func expandedSQLSignExemptions() map[string]struct {
	count  int
	reason string
} {
	out := map[string]struct {
		count  int
		reason string
	}{}
	for key, exempt := range sqlSignExemptions {
		out[key] = exempt
		file, query, _ := strings.Cut(key, ":")
		out[file+".go:"+query] = exempt
	}
	return out
}

func TestSignAuthorityGate_SQLHasNoUnclassifiedEntryTypeArithmetic(t *testing.T) {
	exemptions := expandedSQLSignExemptions()
	found := map[string]int{}

	for _, block := range parseSQLBlocks(t, signAuthoritySQLPaths(t)) {
		spans := bareEntryTypeSpans(block.text)
		if len(spans) == 0 {
			continue
		}
		found[block.key()] += len(spans)
		if _, ok := exemptions[block.key()]; ok {
			continue
		}
		t.Errorf(
			"%s (query %s, near line %d) derives an amount from entry_type without going through %s:\n\t%s\n\n"+
				"Either use ledger_signed_amount(c.normal_side, je.entry_type, je.amount), or -- if this really is a\n"+
				"normal_side-independent debit-minus-credit check -- add %q to sqlSignExemptions with a reason.\n"+
				"Reformatting will not help: the check runs on the whole query with whitespace flattened, so a CASE\n"+
				"split across lines reads exactly the same as a CASE on one. The last query that tested entry_type\n"+
				"directly reported deposits as withdrawals to every consumer of /holders/{h}/trends.",
			block.file, block.query, block.line, strings.Join(signAuthoritySQLFuncs, " / "),
			strings.Join(spans, "\n\t"), block.file+":"+block.query,
		)
	}

	for key, exempt := range exemptions {
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

// goSignExemptions maps a non-test Go file to why it may compare against
// core.NormalSide* (or the bare "debit"/"credit" literals) outside core.Sign,
// and to HOW MANY such comparisons it is allowed to contain.
//
// The count is M-4's other half: the exemption used to be whole-file, which
// pre-approved every branch anyone wrote into those files afterwards. A file
// that grows one is now red until someone looks at the new one on its own
// merits -- the same contract sqlSignExemptions has carried for the SQL side.
var goSignExemptions = map[string]struct {
	count  int
	reason string
}{
	"core/types.go":        {0, "the declaration of the constants themselves, plus NormalSide.IsValid -- declarations are the convention's INPUT, and none of them matches a branch pattern today, so the allowance is zero: a comparison appearing here would be a new implementation"},
	"service/rollup.go":    {1, "asks whether a NEGATIVE balance is anomalous for this account, which is a different question from what sign an entry carries; the balance it tests was already produced by core.Delta"},
	"service/reconcile.go": {1, "buckets an already-signed net (from core.Delta, which rejects invalid input two lines earlier) into debit-normal and credit-normal groups for reporting"},
}

func TestSignAuthorityGate_GoHasNoUnclassifiedNormalSideBranch(t *testing.T) {
	var offenders []string
	perFile := map[string]int{}

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
			perFile[rel]++
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
		"these non-test files branch on a normal_side constant -- or on the bare \"debit\"/\"credit\" literal, which is the\n"+
			"same convention spelled around the type -- outside core.Sign:\n\t%s\n\n"+
			"Use core.Sign / core.SignedAmount / core.Delta. If the branch genuinely asks a different question\n"+
			"(\"is a negative balance anomalous here?\", \"which reporting bucket does this net belong in?\"),\n"+
			"add the file to goSignExemptions with that reason AND the number of comparisons it may contain.",
		strings.Join(offenders, "\n\t"))

	// A stale exemption is as bad as a missing one: it silently pre-approves
	// whatever gets written into that file next. The count is what keeps the
	// approval scoped to the branches somebody actually reviewed (M-4).
	for rel, exempt := range goSignExemptions {
		body, err := os.ReadFile(filepath.Join("..", rel))
		require.NoErrorf(t, err, "exempted file %s does not exist", rel)
		assert.Containsf(t, string(body), "NormalSide",
			"stale exemption: %s no longer mentions NormalSide", rel)
		assert.Equalf(t, exempt.count, perFile[rel],
			"%s is exempted for %d normal_side comparison(s) (%s) but now has %d -- "+
				"review the new one on its own merits instead of inheriting the file's exemption",
			rel, exempt.count, exempt.reason, perFile[rel])
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

// holderVisiblePredicateFiles are the query files whose journal_entries
// aggregates answer "what money can the holder see / what does the platform
// owe". Every such query must apply the canonical predicate or be exempted
// below with a reason.
var holderVisiblePredicateFiles = map[string]bool{
	"holder.sql":            true,
	"platform_balances.sql": true,
}

// holderVisiblePredicateExemptions maps "<file>:<query>" to why an aggregate
// over journal_entries in a holder-facing file legitimately does NOT filter
// on balance_role.
var holderVisiblePredicateExemptions = map[string]string{
	"platform_balances.sql:GetPlatformBalancesByHolder":   "a per-(classification, holder_side) breakdown for operators: it reports EVERY classification separately, including memo and untagged ones, and never sums them into a holder-visible or liability figure. Filtering here would hide rows the operator view exists to show",
	"platform_balances.sql:GetSystemSideCustodialBalance": "system side only (account_holder < 0) and scoped by classification CODE to the custodial set: it measures the assets the platform holds, not what any holder can see. balance_role tags the user-side liability split and has no bearing on it",
}

// TestSignAuthorityGate_HolderVisibleQueriesApplyThePredicate is M-3, the
// coverage half of the test above.
//
// That one pins the SPELLING of the copies that exist -- which is what
// A-M3's fix needed, four copies with one updated. It cannot see the next
// occurrence of the same mistake: a NEW aggregate that forgets the predicate
// entirely. The reviewer added a holder-keyed SUM over journal_entries with
// no balance_role filter at all and every gate stayed green, which is
// A-M3 in its original form (a holder aggregate that silently includes memo
// legs, so a fee both moves the balance and cancels itself out of the
// statement).
//
// So: any query in a holder-facing file that reads journal_entries must
// either apply the canonical predicate or be classified here. A new one
// defaults to red -- the same fail-closed contract as
// postgres/grant_coverage_test.go's table classification.
func TestSignAuthorityGate_HolderVisibleQueriesApplyThePredicate(t *testing.T) {
	const canonicalLower = "balance_role not in ('', 'memo')"

	sources, err := filepath.Glob(filepath.Join("sql", "queries", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, sources)

	checked := 0
	seen := map[string]bool{}
	for _, block := range parseSQLBlocks(t, sources) {
		if !holderVisiblePredicateFiles[block.file] || block.query == "(preamble)" {
			continue
		}
		if !strings.Contains(block.text, "journal_entries") {
			continue
		}
		checked++
		seen[block.key()] = true
		if strings.Contains(block.text, canonicalLower) {
			continue
		}
		if _, ok := holderVisiblePredicateExemptions[block.key()]; ok {
			continue
		}
		t.Errorf("%s (query %s, near line %d) aggregates journal_entries in a holder-facing file without %q.\n\n"+
			"Every query that answers \"what money can the holder see\" or \"what does the platform owe\" must apply that predicate: "+
			"without it, memo legs join the aggregate, and a fee whose memo and locked legs net to zero disappears from the statement "+
			"while still moving the balance (audit A-M3). If this query genuinely reports across all balance roles, add %q to "+
			"holderVisiblePredicateExemptions with the reason.",
			block.file, block.query, block.line, canonicalLower, block.key())
	}

	require.Positive(t, checked, "no holder-facing journal_entries query was found -- the gate scanned nothing")
	for key := range holderVisiblePredicateExemptions {
		assert.Truef(t, seen[key], "stale exemption %q: no such query reads journal_entries any more -- delete the entry", key)
	}
}
