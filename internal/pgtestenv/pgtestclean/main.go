// Command pgtestclean removes what ledger integration-test runs left on the
// test PostgreSQL server (DATABASE_URL, or pgtestenv.DefaultDatabaseURL):
// databases and per-test roles of test processes that are no longer running,
// and the shared ledger_owner / ledger_app / ledger_ro roles once nothing on
// the cluster depends on them. `make test` runs it after `go test`. It never
// touches a live run's objects or another project's databases and roles; see
// pgtestenv.Clean.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/azex-ai/ledger/internal/pgtestenv"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := pgtestenv.Clean(ctx, pgtestenv.BaseURL(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pgtestclean:", err)
		os.Exit(1)
	}
}
