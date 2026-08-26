package postgres

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/azex-ai/ledger/core"
)

func wrapStoreError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, normalizeStoreError(err))
}

func normalizeStoreError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case "23505":
		if pgErr.ConstraintName == "journals_idempotency_key_key" {
			return fmt.Errorf("journal idempotency key already exists: %w", core.ErrDuplicateJournal)
		}
		return fmt.Errorf("unique constraint %q violated: %w", pgErr.ConstraintName, core.ErrConflict)
	case "23514":
		if pgErr.ConstraintName == "chk_journal_currency_balance" ||
			pgErr.ConstraintName == "chk_journal_balance" ||
			strings.Contains(pgErr.Message, "unbalanced entries by currency") {
			return fmt.Errorf("journal is unbalanced: %w", core.ErrUnbalancedJournal)
		}
		return fmt.Errorf("check constraint %q violated: %w", pgErr.ConstraintName, core.ErrInvalidInput)
	case "23503", "23502", "22P02":
		return fmt.Errorf("invalid database input: %w", core.ErrInvalidInput)
	case "40001", "40P01":
		// SQLSTATE class 40 (transaction rollback): 40001 serialization_failure
		// (SERIALIZABLE/REPEATABLE READ conflict) and 40P01 deadlock_detected
		// (Postgres picked this session as the deadlock victim). Both mean the
		// transaction was rolled back for reasons that have nothing to do with
		// the request's validity -- resubmitting the SAME request with the SAME
		// idempotency key is expected to succeed once the contending
		// transaction clears (bus #24: "接线 core.ErrTransient 到 postgres
		// adapter" -- before this, both fell through to `default: return err`
		// and only reached core.IsRetryable's `default: true` catch-all, making
		// them indistinguishable from an unclassified permanent bug at every
		// call site that inspects the wrapped error chain instead of the bare
		// bool).
		return fmt.Errorf("transient postgres error %s: %w: %w", pgErr.Code, err, core.ErrTransient)
	default:
		return err
	}
}
