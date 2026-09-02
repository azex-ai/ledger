package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time interface assertions.
var (
	_ core.PlatformBalanceReader = (*PlatformBalanceStore)(nil)
	_ core.SolvencyChecker       = (*PlatformBalanceStore)(nil)
)

// PlatformBalanceStore reads structured platform-wide balance breakdowns in
// real time.
//
// GetPlatformBalances computes `checkpoint.balance + delta` where delta is the
// net of journal_entries past the checkpoint's last_entry_id, so reads reflect
// every committed write immediately — no waiting for the rollup worker.
//
// The two figures behind SolvencyCheck (and GetTotalLiabilityByAsset, which
// shares the liability query) do NOT read balance_checkpoints at all: they are
// recomputed from journal_entries alone. A checkpoint is a derived cache the
// app credential can INSERT into, and one forged row used to move solvency
// from insolvent to solvent (w3-review/money-path.md M-2, the sibling of
// I-49's finding about the withdrawal gate). Full scan per call, on a periodic
// report — see platform_balances.sql's header.
//
// Single-statement queries rely on PostgreSQL statement-level snapshot
// consistency. Multi-statement reads (SolvencyCheck) wrap in REPEATABLE READ
// to keep the liability and custodial figures from drifting against each
// other.
type PlatformBalanceStore struct {
	pool           *pgxpool.Pool
	db             DBTX
	q              *sqlcgen.Queries
	dims           *dimCache
	custodialCodes []string
	// custodialScopeDeclared distinguishes a scope the CONSUMER named
	// (WithCustodialClassCodes) from DefaultCustodialClassCodes. Only the
	// first is held to "every code must exist" -- see validateCustodialScope.
	custodialScopeDeclared bool
}

// DefaultCustodialClassCodes is the classification scope SolvencyCheck treats
// as the platform's custodied asset position when the consumer does not name
// one explicitly.
//
// It is a set, not the single literal "custodial" it used to be hardcoded as
// in SQL, for two reasons the 2026-09-02 audit measured:
//
//   - "settlement" holds the platform's per-currency FX inventory
//     (presets/fx.go). Leaving it out made every currency a holder bought
//     report solvent=false forever, on a position that was in fact perfectly
//     backed -- an alarm nailed to ON, which is worse than no alarm
//     (working-agreements.md §3). It is also the transit account for
//     transfers, where it nets to zero, so including it is free there.
//   - A deployment naming its custody classification something else got
//     Custodial = 0 with no error at all; the coupling to a string literal
//     was not visible from any interface.
//
// Deliberately NOT everything system-side: "equity", "fees", "fee_revenue"
// and "spread" are the platform's own money rather than assets backing holder
// claims, and "dev_credit" is by design an UNBACKED counterparty -- counting
// it would make the shortfall it exists to expose disappear
// (presets/devcredit.go, TestDevCredit_SolvencyShortfallEqualsDevCreditBalance).
var DefaultCustodialClassCodes = []string{"custodial", "settlement"}

// NewPlatformBalanceStore creates a new PlatformBalanceStore bound to a pool,
// with the default custodial scope (see DefaultCustodialClassCodes).
func NewPlatformBalanceStore(pool *pgxpool.Pool) *PlatformBalanceStore {
	return &PlatformBalanceStore{
		pool:           pool,
		db:             pool,
		q:              sqlcgen.New(pool),
		dims:           dimCacheFor(pool),
		custodialCodes: append([]string(nil), DefaultCustodialClassCodes...),
	}
}

// WithCustodialClassCodes returns a clone whose solvency reports treat exactly
// these classification codes as the custodied asset position. Deployments that
// name their custody accounts differently, or that hold reserves across more
// than the shipped presets' classifications, declare the scope here rather
// than discovering it as a permanent, silent zero.
//
// A scope that matches no classification is rejected at read time, not
// reported as an empty custody position.
func (s *PlatformBalanceStore) WithCustodialClassCodes(codes ...string) *PlatformBalanceStore {
	clone := *s
	clone.custodialCodes = append([]string(nil), codes...)
	clone.custodialScopeDeclared = true
	return &clone
}

// WithDB returns a clone bound to db (a *pgxpool.Pool or pgx.Tx). When passed
// a tx the store reads inside the caller's transaction and SolvencyCheck
// skips its own REPEATABLE READ wrap (the caller's isolation applies).
func (s *PlatformBalanceStore) WithDB(db DBTX) *PlatformBalanceStore {
	return &PlatformBalanceStore{
		pool:           nil, // tx mode — disables inner BeginTx
		db:             db,
		q:              sqlcgen.New(db),
		dims:           dimCacheForTx(s.dims),
		custodialCodes: s.custodialCodes,

		custodialScopeDeclared: s.custodialScopeDeclared,
	}
}

// GetPlatformBalances returns a structured per-classification balance breakdown
// for the given currency. UserSide and SystemSide maps are keyed by
// classification code. Classifications with no checkpoints are absent from the
// maps (not present with a zero value).
func (s *PlatformBalanceStore) GetPlatformBalances(ctx context.Context, currencyUID string) (*core.PlatformBalance, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.GetPlatformBalancesByHolder(ctx, cur.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: get by holder: %w", err)
	}

	pb := &core.PlatformBalance{
		CurrencyUID: currencyUID,
		UserSide:    make(map[string]decimal.Decimal),
		SystemSide:  make(map[string]decimal.Decimal),
	}

	for _, row := range rows {
		bal, err := numericToDecimal(row.TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: platform balance: convert %s/%s: %w",
				row.ClassificationCode, row.HolderSide, err)
		}
		switch row.HolderSide {
		case "user":
			pb.UserSide[row.ClassificationCode] = bal
		case "system":
			pb.SystemSide[row.ClassificationCode] = bal
		}
	}

	return pb, nil
}

// GetTotalLiabilityByAsset returns the sum of all user-side (holder > 0)
// liability balances for the given currency, recomputed from journal_entries.
// This is the aggregate liability — what the platform owes users in total.
func (s *PlatformBalanceStore) GetTotalLiabilityByAsset(ctx context.Context, currencyUID string) (decimal.Decimal, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return decimal.Zero, err
	}
	raw, err := s.q.GetTotalUserSideBalance(ctx, cur.ID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: platform balance: total liability currency=%s: %w", currencyUID, err)
	}
	total, err := numericToDecimal(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: platform balance: total liability convert: %w", err)
	}
	return total, nil
}

// SolvencyCheck computes a solvency report for the given currency.
//
// Liability = realtime sum of user-side (holder > 0) balances.
// Custodial = realtime sum of system-side (holder < 0) balances for the
//
//	store's custodial scope (see WithCustodialClassCodes).
//
// Solvent   = Custodial >= Liability.
// Margin    = Custodial - Liability (positive = surplus, negative = shortfall).
//
// Both figures are recomputed from journal_entries -- never from
// balance_checkpoints (see the type's doc comment) -- inside one REPEATABLE
// READ transaction, so they describe a single point in time. Comparing the custodial figure to an off-chain custody
// position is the consumer's responsibility.
func (s *PlatformBalanceStore) SolvencyCheck(ctx context.Context, currencyUID string) (*core.SolvencyReport, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	if s.pool == nil {
		// Tx mode: caller's transaction provides isolation; query directly.
		return s.solvencyCheckWithQueries(ctx, s.q, currencyUID, cur.ID)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	report, err := s.solvencyCheckWithQueries(ctx, q, currencyUID, cur.ID)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (s *PlatformBalanceStore) solvencyCheckWithQueries(ctx context.Context, q *sqlcgen.Queries, currencyUID string, currencyID int64) (*core.SolvencyReport, error) {
	liabilityRaw, err := q.GetTotalUserSideBalance(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency liability currency=%s: %w", currencyUID, err)
	}
	liability, err := numericToDecimal(liabilityRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency liability convert: %w", err)
	}

	if err := s.validateCustodialScope(ctx, q); err != nil {
		return nil, err
	}

	custodialRaw, err := q.GetSystemSideCustodialBalance(ctx, sqlcgen.GetSystemSideCustodialBalanceParams{
		CurrencyID:     currencyID,
		CustodialCodes: s.custodialCodes,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency custodial currency=%d: %w", currencyID, err)
	}
	custodial, err := numericToDecimal(custodialRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency custodial convert: %w", err)
	}

	margin := custodial.Sub(liability)
	return &core.SolvencyReport{
		CurrencyUID: currencyUID,
		Liability:   liability,
		Custodial:   custodial,
		Solvent:     custodial.GreaterThanOrEqual(liability),
		Margin:      margin,
	}, nil
}

// validateCustodialScope refuses to report at all unless every code in the
// scope names a classification that can actually stand for a custodied asset.
// Two failures it did not use to catch, both from the 2026-09-02 adversarial
// re-review (w3-review/money-path.md m-1 and m-2):
//
//   - The old check was COUNT(*) > 0, so one typo in a multi-code scope
//     ("custodial", "setlement") passed with the entire settlement position --
//     FX inventory, transit -- silently absent from the asset side. §7.3
//     introduced multi-code scopes, so "matched some" is the case that
//     mattered.
//   - Nothing said a classification named as custody had to BE an asset.
//     DefaultCustodialClassCodes' doc comment explains precisely why equity,
//     fees, spread and dev_credit are excluded (they are the platform's own
//     money, or -- for dev_credit -- deliberately unbacked), but that
//     reasoning lived only in the comment: one line of consumer config could
//     move the unbacked issuance this report exists to expose onto the asset
//     side and net the shortfall away. §7.3 made "what is a custodied asset"
//     a classification property; this enforces the property.
//
// The property: is_system = true (a platform-side account, not a holder's)
// AND balance_role = ” (role-bearing classifications are the LIABILITY side
// -- available/pending/locked is a holder's own money -- and 'memo' accounts
// are reporting artifacts). That is exactly what custodial and settlement
// are, and exactly what main_wallet and fee_expense are not. dev_credit is
// is_system with no role, so it passes the shape test and is refused by name:
// its whole purpose is to be an unbacked counterparty.
func (s *PlatformBalanceStore) validateCustodialScope(ctx context.Context, q *sqlcgen.Queries) error {
	if len(s.custodialCodes) == 0 {
		return fmt.Errorf(
			"postgres: platform balance: solvency: custodial scope is empty, so the custodial figure could only ever be zero: %w",
			core.ErrInvalidInput,
		)
	}

	rows, err := q.ListClassificationScopeAttributes(ctx, s.custodialCodes)
	if err != nil {
		return fmt.Errorf("postgres: platform balance: solvency custodial scope: %w", err)
	}
	byCode := make(map[string]sqlcgen.ListClassificationScopeAttributesRow, len(rows))
	for _, r := range rows {
		byCode[r.Code] = r
	}

	var missing, notAnAsset []string
	for _, code := range s.custodialCodes {
		row, ok := byCode[code]
		if !ok {
			missing = append(missing, code)
			continue
		}
		if !row.IsSystem || row.BalanceRole != "" {
			notAnAsset = append(notAnAsset, fmt.Sprintf("%s (is_system=%t, balance_role=%q)", code, row.IsSystem, row.BalanceRole))
			continue
		}
		if _, unbacked := unbackedCustodialCodes[code]; unbacked {
			notAnAsset = append(notAnAsset, fmt.Sprintf("%s (deliberately unbacked -- see presets/devcredit.go)", code))
		}
	}

	switch {
	case s.custodialScopeDeclared && len(missing) > 0:
		// A scope the consumer wrote down: every code in it is a claim about
		// this deployment, and a claim that resolves to nothing is a typo,
		// not a position.
		return fmt.Errorf(
			"postgres: platform balance: solvency: custodial scope names %v, which match no classification -- "+
				"whatever they were meant to cover would be silently absent from the asset side: %w",
			missing, core.ErrInvalidInput,
		)
	case len(missing) == len(s.custodialCodes):
		// DefaultCustodialClassCodes is the library's guess, not the
		// consumer's declaration: a deployment that installed only the
		// deposit bundle has no `settlement` classification and is not
		// misconfigured for it. Missing ALL of them is still fatal -- that is
		// A-N3's original fail-loud, and it can only produce Custodial = 0.
		return fmt.Errorf(
			"postgres: platform balance: solvency: custodial scope %v matches no classification, so the custodial figure could only ever be zero: %w",
			s.custodialCodes, core.ErrInvalidInput,
		)
	}
	if len(notAnAsset) > 0 {
		return fmt.Errorf(
			"postgres: platform balance: solvency: custodial scope names %v, which are not custodied assets backing holder claims "+
				"(a custody classification is is_system with no balance_role) -- counting them would net away the very shortfall this report exists to expose: %w",
			notAnAsset, core.ErrInvalidInput,
		)
	}
	return nil
}

// unbackedCustodialCodes are shipped classifications that pass the structural
// test (is_system, no balance_role) but must never be counted as custody: the
// dev-credit counterparty exists precisely to make unbacked issuance show up
// as a shortfall (presets/devcredit.go), so putting it on the asset side
// deletes the signal. Named rather than derived because "unbacked" is a fact
// about what the preset MEANS, and the schema carries no column for it.
var unbackedCustodialCodes = map[string]struct{}{"dev_credit": {}}
