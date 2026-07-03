package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// accountPolicyDim identifies a (holder, currency, classification) balance
// dimension — the same granularity core.BalanceReader.GetBalance operates on.
type accountPolicyDim struct {
	holder           int64
	currencyID       int64
	classificationID int64
}

// enforceAccountPolicies checks every entry in a not-yet-posted journal
// against any account policy governing its dimension. Called from inside
// postJournalWithQueries, after the tx-scoped advisory locks for the
// journal's (holder, currency) pairs are already held (I-4) and before any
// row is written — a rejection here aborts the whole journal; nothing is
// partially applied.
//
// Two aggregations run over the entry set, for two different reasons:
//
//   - Per-dimension net delta (dimensionDelta, keyed by the exact
//     (holder,currency,classification) triple) feeds the min_balance check,
//     which is a property of one specific GetBalance dimension. Netting
//     matters here because a journal can carry two entries on the same
//     dimension (e.g. a debit and a credit that net to an increase) whose
//     combined effect must be judged once against balance-after, not
//     rejected on an intermediate per-entry read.
//
//   - Per-policy net delta (policyDelta, keyed by the resolved policy row's
//     ID) feeds the frozen check. A single policy row can be a currency- or
//     holder-wide wildcard governing several classifications at once. E.g.
//     PendingBalanceWriter.ConfirmPending posts, for the same holder in the
//     same journal, a DR to the credit-normal "pending" classification (a
//     decrease) and a DR to the debit-normal "main_wallet" classification (an
//     increase). Rejecting on the first decrease-direction entry alone would
//     block deposit finalization even though frozen semantics explicitly
//     carve out deposits (design doc §4/§9-1: frozen blocks consumption, not
//     the pending two-phase deposit flow — pinned by
//     TestLedgerStore_ConfirmPending_SucceedsWhileFrozen). Netting by
//     resolved policy row lets a same-policy journal's internal transfers
//     wash out, while a genuine net withdrawal under that policy still nets
//     negative and is rejected.
//
// closed is absolute (blocks both directions per the design doc's semantics
// table) so it is checked per-entry, fail-fast, with no netting.
//
// See docs/INVARIANTS.md I-17.
func (s *LedgerStore) enforceAccountPolicies(ctx context.Context, q *sqlcgen.Queries, entries []resolvedEntry) error {
	policies := make(map[accountPolicyDim]*sqlcgen.AccountPolicy)
	dimensionDelta := make(map[accountPolicyDim]decimal.Decimal)
	policyDelta := make(map[int64]decimal.Decimal)
	policyByID := make(map[int64]*sqlcgen.AccountPolicy)

	for _, e := range entries {
		direction := core.EntryDirection(e.EntryType, e.normalSide)
		delta := e.Amount
		if direction == core.BalanceDirectionDecrease {
			delta = delta.Neg()
		}

		dim := accountPolicyDim{holder: e.AccountHolder, currencyID: e.currencyID, classificationID: e.classificationID}
		dimensionDelta[dim] = dimensionDelta[dim].Add(delta)

		policy, ok := policies[dim]
		if !ok {
			p, err := getEffectiveAccountPolicy(ctx, q, dim.holder, dim.currencyID, dim.classificationID)
			if err != nil {
				return fmt.Errorf("postgres: post journal: %w", err)
			}
			policy = p
			policies[dim] = policy
		}
		if policy == nil {
			continue
		}

		if core.AccountPolicyStatus(policy.Status) == core.AccountPolicyStatusClosed {
			return fmt.Errorf(
				"postgres: post journal: account %d currency %d classification %d is closed (policy %d): %w",
				dim.holder, dim.currencyID, dim.classificationID, policy.ID, core.ErrAccountClosed,
			)
		}

		if core.AccountPolicyStatus(policy.Status) == core.AccountPolicyStatusFrozen {
			policyDelta[policy.ID] = policyDelta[policy.ID].Add(delta)
			policyByID[policy.ID] = policy
		}
	}

	for policyID, netDelta := range policyDelta {
		if netDelta.IsNegative() {
			p := policyByID[policyID]
			return fmt.Errorf(
				"postgres: post journal: account %d currency %d is frozen — net decrease %s under policy %d: %w",
				p.AccountHolder, p.CurrencyID, netDelta, policyID, core.ErrAccountFrozen,
			)
		}
	}

	for dim, delta := range dimensionDelta {
		policy := policies[dim]
		if policy == nil || !policy.EnforceMinBalance {
			continue
		}
		minBalance := mustNumericToDecimal(policy.MinBalance)
		before, err := s.getBalanceWithQueries(ctx, q, dim.holder, dim.currencyID, dim.classificationID)
		if err != nil {
			return fmt.Errorf("postgres: post journal: account policy: get balance: %w", err)
		}
		after := before.Add(delta)
		if after.LessThan(minBalance) {
			return fmt.Errorf(
				"postgres: post journal: account %d currency %d classification %d balance %s would fall below min_balance %s (policy %d): %w",
				dim.holder, dim.currencyID, dim.classificationID, after, minBalance, policy.ID, core.ErrInsufficientBalance,
			)
		}
	}

	return nil
}
