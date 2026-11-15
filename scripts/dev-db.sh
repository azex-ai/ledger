#!/usr/bin/env bash
# `make db`: the local development database on the machine's shared
# PostgreSQL (127.0.0.1:5432, Homebrew postgresql@18), not a container.
# Idempotent -- safe to re-run at any time:
#
#   - database `ledger`, owned by the admin (bootstrap) user, as
#     docs/RUNBOOK.md's install shape has it;
#   - roles ledger_owner / ledger_app / ledger_ro, created when missing with
#     the attributes the baseline migration gives them. Existing roles keep
#     their attributes; ledger_owner's password is never touched (Migrate
#     runs as the admin and switches to it with SET ROLE);
#   - LOGIN password on ledger_app / ledger_ro: the one value the integration
#     suite also sets on them (postgrestest.SharedRolePassword, pinned equal
#     by internal/postgrestest/devdb_script_test.go), so this script and a
#     suite running on the same server never break each other's logins;
#   - CONNECT on `ledger` for the three roles. Besides being what they need,
#     the grant is a dependency in pg_shdepend, which is what makes the test
#     cleanup (pgtestclean) keep the roles instead of dropping them as unused
#     (docs/TESTING.md).
#
# It does not run migrations: this repository ships no server, and each host
# (or example) runs ledger.Migrate itself.
#
# LEDGER_DEV_ADMIN_URL overrides the admin connection (default: the shared
# instance's `dev` superuser). `make db-docker` is the explicit container
# alternative (docker-compose.yml).
set -euo pipefail

admin_url=${LEDGER_DEV_ADMIN_URL:-postgres://dev:dev@127.0.0.1:5432/postgres?sslmode=disable}
password=ledger-test-shared-role-password-not-a-real-secret # test-only, not a secret

psql "$admin_url" -v ON_ERROR_STOP=1 -q -v pw="$password" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', r)
FROM unnest(ARRAY['ledger_owner', 'ledger_app', 'ledger_ro']) AS r
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r)
\gexec
-- Same lock as postgrestest.SetSharedRolePassword (pgtestenv.
-- SharedRolePasswordLockKey): a concurrent test's ALTER of the same role
-- would otherwise make one of the two fail with "tuple concurrently updated".
BEGIN;
SELECT pg_advisory_xact_lock(2573143717);
SELECT format('ALTER ROLE %I PASSWORD %L', r, :'pw')
FROM unnest(ARRAY['ledger_app', 'ledger_ro']) AS r
\gexec
COMMIT;
SELECT 'CREATE DATABASE ledger'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'ledger')
\gexec
GRANT CONNECT ON DATABASE ledger TO ledger_owner, ledger_app, ledger_ro;
SQL

owner=$(psql "$admin_url" -Atc "SELECT datdba::regrole FROM pg_database WHERE datname = 'ledger'")
admin_user=$(psql "$admin_url" -Atc "SELECT current_user")
if [ "$owner" != "$admin_user" ]; then
  echo "warning: database ledger already existed, owned by $owner, not $admin_user -- someone else's?" >&2
fi

# The printed URLs follow admin_url: its database path becomes /ledger, and
# for DATABASE_URL its user:password becomes ledger_app's.
migrate_url=$(printf '%s' "$admin_url" | sed -E 's#/[^/?]*(\?|$)#/ledger\1#')
app_url=$(printf '%s' "$migrate_url" | sed -E "s#://[^@/]*@#://ledger_app:${password}@#")
cat <<EOF
Local database ready: ledger.
  MIGRATE_DATABASE_URL=$migrate_url
  DATABASE_URL=$app_url
EOF
