# Local authorization acceptance

`role_matrix.py` checks the role-bound host in `examples/admin-acceptance`
against its actual ledger HTTP server. It uses Python 3's standard library;
no browser, database driver or additional package is required. Both URLs must
be loopback origins on `127.0.0.1`.

Start that host with its isolated fixture database first. The fixture must
contain USD and the `deposit_confirm` template. Keep deposit review and
dead-letter add-ons disabled for this matrix: `reviewer` reaching 503/18102
proves authorization, while the other three identities must receive 403.

Provide the host's private, flat JSON environment file (mode 0600 or stricter)
through `--env-file`. It must contain `ADMIN_SESSION_SECRET` and
`ADMIN_PASSWORD_READER`, `ADMIN_PASSWORD_WRITER`, `ADMIN_PASSWORD_ADMIN`,
`ADMIN_PASSWORD_REVIEWER`, plus the matching `LEDGER_KEY_*` values. Never commit
this file. The script logs neither those values nor cookies/raw response bodies.

```sh
python3 scripts/admin-acceptance/role_matrix.py \
  --base-url "http://127.0.0.1:${AUTH_HOST_PORT}" \
  --api-url "http://127.0.0.1:${AUTH_API_PORT}" \
  --env-file "$AUTH_FIXTURE_ENV" \
  --fixture-holder 990051 > "$AUTH_MATRIX_OUTPUT"
```

Use a dedicated positive `--fixture-holder`. The script creates uniquely named
currency canaries and deactivates those it creates. It also creates one pending
deposit booking for each of writer/admin, then replays the same key through the
direct Go API. It does not transition those bookings, post journals, settle
holds, close periods or modify existing policies. Fixture rows remain for
inspection; this command belongs only on disposable acceptance data.

The checks compare normal session/BFF requests with direct API requests,
including semantic POST reads, configuration versus business scope, independent
`deposit_review`, protected templates, invalid sessions and rejected Origins.
Credentials used for direct API checks remain in this local test process.

Output is JSON Lines. Exit 0 means all assertions passed; 1 means an assertion
failed; 2 means the probe could not complete. Do not treat a 400 from malformed
business input or a disabled add-on as successful business execution. This is
an authorization matrix, not a substitute for the browser UI, all endpoint
behaviors or deployment validation. Historical results and limits are recorded
in `docs/audits/2026-10-11-admin-acceptance/permissions.md`.
