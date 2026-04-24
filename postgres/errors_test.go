package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/azex-ai/ledger/core"
)

func TestNormalizeStoreError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "journal duplicate",
			err: &pgconn.PgError{
				Code:           "23505",
				ConstraintName: "journals_idempotency_key_key",
			},
			want: core.ErrDuplicateJournal,
		},
		{
			name: "journal unbalanced",
			err: &pgconn.PgError{
				Code:           "23514",
				ConstraintName: "chk_journal_currency_balance",
			},
			want: core.ErrUnbalancedJournal,
		},
		{
			name: "fk violation",
			err: &pgconn.PgError{
				Code: "23503",
			},
			want: core.ErrInvalidInput,
		},
		{
			name: "other unique",
			err: &pgconn.PgError{
				Code:           "23505",
				ConstraintName: "entry_templates_code_key",
			},
			want: core.ErrConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeStoreError(tc.err)
			assert.True(t, errors.Is(got, tc.want), "expected %v, got %v", tc.want, got)
		})
	}
}
