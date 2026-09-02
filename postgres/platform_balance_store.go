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
// real time. Every query computes `checkpoint.balance + delta` where delta is
// the net of journal_entries past the checkpoint's last_entry_id, so reads
// reflect every committed write immediately — no waiting for the rollup
// worker.
//
// Single-statement queries (GetPlatformBalances, GetTotalLiabilityByAsset)
// rely on PostgreSQL statement-level snapshot consistency. Multi-statement
// reads (SolvencyCheck) wrap in REPEATABLE READ to keep the liability and
// custodial figures from drifting against each other.
type PlatformBalanceStore struct {
	pool           *pgxpool.Pool
	db             DBTX
	q              *sqlcgen.Queries
	dims           *dimCache
	custodialCodes []string
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
		dims:           s.dims,
		custodialCodes: s.custodialCodes,
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

// GetTotalLiabilityByAsset returns the realtime sum of all user-side
// (holder > 0) balances for the given currency, across all classifications.
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
// Both figures come from one REPEATABLE READ transaction so they describe a
// single point in time. Comparing the custodial figure to an off-chain custody
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

	// Fail-loud on a scope that cannot possibly resolve. Custodial = 0 from a
	// misconfigured scope is indistinguishable from Custodial = 0 on an empty
	// ledger, and it reports as total insolvency either way.
	matched, err := q.CountClassificationsWithCodes(ctx, s.custodialCodes)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency custodial scope currency=%s: %w", currencyUID, err)
	}
	if matched == 0 {
		return nil, fmt.Errorf(
			"postgres: platform balance: solvency: custodial scope %v matches no classification, so the custodial figure could only ever be zero: %w",
			s.custodialCodes, core.ErrInvalidInput,
		)
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
