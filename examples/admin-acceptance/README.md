# Authenticated shadcn admin acceptance host

This example consumes the Go library and the built npm package in **separate
projects outside this checkout**. It hosts the package's shadcn `LedgerAdmin` in a
production Next.js build, with real session cookies and a server-side BFF. It is
an integration and acceptance harness, not a production identity service.

## Build and run

Requirements: Go 1.26.6+, Node 24+, npm, Python 3.11+, and a dedicated PostgreSQL 17
database. Install the repository's web workspace dependencies once (`npm ci` in
`web/`). The runner builds the SDK from this candidate checkout, creates an npm
archive, and installs that archive into an independent Next project. Its Go
consumer uses `GOWORK=off` and an explicit local `replace` for the candidate Go
module. This proves local candidate consumption; it does not publish packages or
prove availability of a release tag.

```sh
# From the repository root. Use a dedicated acceptance database, never production.
export DATABASE_URL='postgres://user:password@127.0.0.1:5432/ledger_admin_acceptance?sslmode=disable'
python3 examples/admin-acceptance/stage.py --start
```

Default loopback ports are API `48110` and Next `43110`; override them with
`--api-port` and `--web-port`. `--output /absolute/empty/directory` chooses a new
consumer directory; it must be outside the checkout. Without `--start`, the runner
builds and verifies both consumers without opening the database or starting
servers. It never creates/drops databases, invokes destructive tests, or publishes
an artifact. Backend startup applies library migrations and idempotent seed data,
so the database **must** be dedicated to this example.

The runner prints the exact staging directory immediately, even before a build
failure. `stage.json` records that directory and completion state. A successful
run writes `runtime.json` with source commit/dirty status, a host source digest,
tarball digest, package ID, consumer paths, logs, and process IDs. `private-env.json`
has mode `0600`; read it locally to obtain the four generated login passwords.
Never copy its values to reports, client bundles, screenshots, or version control.

To restart the same database, reuse that private environment:

```sh
python3 examples/admin-acceptance/stage.py --start --env-file /exact/previous/directory/private-env.json
```

The ports must match the reused environment (pass the same port flags when using
nondefault ports). **Do not generate a new signing seed for existing seeded data.**
The local Ed25519 attestor is a harness fixture, not a remote production signing
service. Go runs with `ENV=production`; Next runs `next build` then `next start`.
Build installs explicitly include development dependencies because Tailwind's
PostCSS plugin and TypeScript are build tools even when runtime uses production
mode.

Stop only the API/Next PIDs recorded in your exact `runtime.json`, after checking
their commands. Remove only the exact directory listed in that manifest when no
longer needed; do not use a wildcard across other acceptance runtimes. Database
lifecycle remains the caller's responsibility.

## Session and authorization boundary

Login accepts one of four fixed test accounts. Passwords and API keys stay on the
server. The signed, one-hour, HttpOnly, SameSite=Strict cookie contains only the
account, an independent public random session nonce, and expiry. Each request
verifies it and selects that account's configured backend key. Arbitrary browser
`Authorization`, cookies, proxy IP headers, and hop-by-hop headers are not forwarded.

| Account | Go API grant | Intended checks |
| --- | --- | --- |
| `reader` | `read` | Reads, POST balance batch, POST template preview |
| `writer` | `write` | Reads and business mutations, no metadata administration |
| `admin` | `admin` | Reads, business mutations, metadata/operations administration |
| `reviewer` | `read+deposit_review` | Reads plus the independent deposit-review capability |

`admin` does **not** imply `deposit_review`. This host leaves the optional review
provider unconfigured: the reviewer reaches a `503` disabled-feature response;
an account without that capability is rejected with `403` before that boundary.
Navigation is not authorization. Read-only accounts can see operation controls,
but Go rejects unauthorized requests.

The BFF and session endpoints require an exact configured `Origin` on mutations,
including requests with no Origin. Same-origin browser fetches satisfy this check.
Do not copy a cookie into a nonbrowser client without also providing the origin.
The BFF uses fixed upstream configuration, a small request/response header
allowlist, no caching, manual redirects, and a 15-second upstream timeout.

Cookies are Secure on HTTPS. The only HTTP exception is the explicit
`127.0.0.1` acceptance origin; do not expose this harness on an external HTTP host.
Passwords are fixed environment fixtures: there is no user enrollment, MFA,
password reset, or login throttling here. Logout clears the browser cookie but
does not centrally revoke a copied stateless token before expiry.

The SDK cache scope uses a nonsecret logical backend ID plus the verified account
and random session nonce. Login and logout use full navigation, remounting the
provider and clearing page/mutation state as well as the QueryClient. Raw tokens,
API keys, signing keys, and internal URLs never enter query keys.

## Wired sections and seed data

The example exposes Dashboard, Journals, Balances, Reservations, Deposits,
Classifications, Journal Types, Templates, Currencies, Reconciliation, and
Snapshots. Deposit Reviews, Withdrawals, and Sweep Monitor are omitted because
this host does not install their review provider, withdrawal lifecycle, or sweep
executor. It does not expose a wallet, development credit faucet, or holder-token
issuer. No background worker is started; scheduled snapshots/rollups are not
promised, and empty history remains empty.

The seed uses exact decimal strings and the library's signed posting path:

| Holder | Currency (exponent) | Booked quantity |
| --- | --- | --- |
| `1001` | USD (2) | `1234.56` |
| `1001` | USDC (6) | `1.234567` |
| `1001` | BTC (8) | `0.00000001` |
| `1001` | POINTS (0) | `42` |
| `1001` | ATOM (18), precision fixture | `999999999999.123456789012345678` |
| `1002` | ATOM (18) | `0.000000000000000001` |

Holder `1001` additionally has a USD `34.56` reservation (24-hour expiry), giving
USD booked `1234.56`, held `34.56`, and available `1200` before any acceptance
mutations. A pending USDC deposit booking of `5.000001` has not been credited.
Settling a reservation only discharges its hold; it does not itself post an
expense. A separate journal for holder `1003` contains USD `1.23` and BTC
`0.00000002`, balanced independently per currency. Their scalar arithmetic sum is
not a USD valuation and must not be presented as one.

The maximum ATOM fixture fits `NUMERIC(30,18)` and tests exact decimal precision;
it is not an unsafe JavaScript integer holder ID. Test writes should use a
separate holder so the seed oracle stays predictable. The reserve is not renewed
when an existing idempotency key is replayed after expiry or settlement.

## Verification and provenance

The runner checks Go build/vet and the production dependency graph; SDK build;
independent npm tarball installation (not a symlink); absence of `@heroui/react`;
Next production build and typecheck; and host authentication tests. Runtime
browser acceptance and the API permission matrix are separate real-stack checks;
a successful build alone does not claim they passed.

Host login controls are official shadcn base-nova components generated with the
installed `shadcn add button input label` CLI. The package supplies the admin UI.
See official [Button](https://ui.shadcn.com/docs/components/base/button) and
[Input](https://ui.shadcn.com/docs/components/base/input) documentation. The sole
TypeScript alias `@/*` resolves host-local files; no alias resolves SDK source.
