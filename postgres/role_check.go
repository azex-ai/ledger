package postgres

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// AppRole is the database role a serving connection is meant to authenticate
// as. Every ACL-enforced guarantee in this schema is written against this name.
const AppRole = "ledger_app"

// CheckRuntimeRole reports whether db is connected as `want`.
//
// Why this exists (A-N2, 2026-09-02 deep audit): several of the ledger's
// hardest guarantees are enforced by GRANTs on one role name, not by anything
// the library can check for itself. I-42 -- journal_entries.id comes from the
// sequence alone, which is what makes the balance equation's "id > checkpoint"
// scan monotonic -- is a column-level INSERT grant against ledger_app.
// Migration 008's narrowing, 014's webhook_subscribers write narrowing, 021's
// function EXECUTE whitelist and the append-only guards' REVOKEs are all the
// same shape. Connect the serving pool as ledger_owner instead, and every one
// of them is simply absent: no error, no warning, identical behaviour until
// something goes wrong in a way the invariants said could not happen.
//
// Until now the only thing standing behind that was a sentence in README and
// RUNBOOK. This makes the prerequisite checkable, and
// (*ledger.Service).AssertRuntimeRole is the entry point a composition root
// calls to check it.
//
// Deliberately NOT called from ledger.New. Development, migrations, tests and
// the emergency-recovery procedures in the runbook all connect as something
// else on purpose, and a library that refused to start for them would be
// wrong; a library that logged a warning nobody configured a logger for would
// be worse, because it would look like a check while being one (the
// NopLogger-swallows-everything shape this audit round found in three other
// places). Whether a deployment treats a mismatch as fatal is the deployment's
// call, made where it has the context to make it.
func CheckRuntimeRole(ctx context.Context, db DBTX, want string) error {
	var actual string
	if err := db.QueryRow(ctx, "SELECT current_user").Scan(&actual); err != nil {
		return fmt.Errorf("postgres: role check: read current_user: %w", err)
	}
	if actual != want {
		return fmt.Errorf("postgres: role check: connected as %q, expected %q -- "+
			"the ACL-enforced invariants (I-22, I-42, and the append-only guards) constrain %q and nothing else, "+
			"so on this connection they are not in force: %w",
			actual, want, want, core.ErrInvalidInput)
	}
	return nil
}
