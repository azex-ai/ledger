#!/usr/bin/env bash
# `make db`: the local development database on the machine's shared
# PostgreSQL (127.0.0.1:5432, Homebrew postgresql@18), not a container.
# Idempotent -- safe to re-run at any time:
#
#   - roles ledger_owner / ledger_app / ledger_ro, with the same attributes
#     the baseline migration gives them, LOGIN with the one password the
#     integration suite also sets on them (postgrestest.SharedRolePassword,
#     pinned equal by internal/postgrestest/devdb_script_test.go);
#   - database `ledger`, owned by ledger_owner.
#
# It does not run migrations: this repository ships no server, and each host
# (or example) runs ledger.Migrate itself.
#
# The roles are cluster-wide, so the integration suite on the same server
# shares them. Using the suite's own password is what makes this script and a
# running suite interchangeable: either may (re)set the password at any time
# without breaking the other's logins. The suite's cleanup (pgtestclean)
# drops the roles only while nothing on the server depends on them -- i.e.
# until `ledger` is migrated; re-run this script if that happened
# (docs/TESTING.md).
#
# LEDGER_DEV_ADMIN_URL overrides the superuser connection (default: the shared
# instance's `dev` superuser). `make db-docker` is the explicit container
# alternative (docker-compose.yml).
set -euo pipefail

admin_url=${LEDGER_DEV_ADMIN_URL:-postgres://dev:dev@127.0.0.1:5432/postgres?sslmode=disable}
password=ledger-test-shared-role-password-not-a-real-secret # test-only, not a secret

psql "$admin_url" -v ON_ERROR_STOP=1 -q -v pw="$password" <<'SQL'
SELECT format(
  CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r)
       THEN 'ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD %L'
       ELSE 'CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD %L'
  END, r, :'pw')
FROM unnest(ARRAY['ledger_owner', 'ledger_app', 'ledger_ro']) AS r
\gexec
SELECT 'CREATE DATABASE ledger OWNER ledger_owner'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'ledger')
\gexec
SQL

cat <<EOF
Local database ready: ledger on 127.0.0.1:5432.
  MIGRATE_DATABASE_URL=postgres://dev:dev@127.0.0.1:5432/ledger?sslmode=disable
  DATABASE_URL=postgres://ledger_app:${password}@127.0.0.1:5432/ledger?sslmode=disable
EOF
