# Ledger Web Dashboard

Next.js 16 admin dashboard for the Go ledger backend, and the **reference
integration** (dogfood) for [`@azex/ledger-react`](packages/ledger-react/) —
the package is this app's only ledger UI/data source; there is no local ledger
UI code.

What it demonstrates:

- **Client provider setup** — [`src/components/ledger-providers.tsx`](src/components/ledger-providers.tsx)
  mounts `<LedgerProvider>` + `<Toaster>` once, with config resolved by the
  composition root (the package never reads env).
- **Composition-root env resolution** — [`src/lib/ledger-env.ts`](src/lib/ledger-env.ts)
  is the single place that reads environment variables, with fail-loud
  production guards for both server and client configs.
- **RSC server prefetch** — each route under [`src/app/`](src/app/) prefetches
  via `@azex/ledger-react/server` and hydrates the client hooks through
  `HydrationBoundary` (see [`src/app/journals/page.tsx`](src/app/journals/page.tsx)).
- **Router adapter** — [`src/components/next-link.tsx`](src/components/next-link.tsx)
  wraps `next/link` to satisfy the package's `LinkComponent` contract, so
  package pages navigate via the Next router.

Full package documentation: [docs/frontend.md](../docs/frontend.md) (guide +
complete API reference) and the [package README](packages/ledger-react/README.md)
(condensed consumer docs + release procedure).

## Configuration

The dashboard talks to the Go ledger backend via HTTP:

All configuration is server-only — the browser never holds ledger
credentials. Browser API calls hit the same-origin BFF proxy
(`/api/v1/*`), which checks the dashboard session cookie and attaches the
server-held key.

| Var | Required | Description |
|-----|----------|-------------|
| `LEDGER_API_URL_INTERNAL` | yes (production) | Server-only base URL of ledgerd (internal/private network), e.g. `http://ledger.internal:8080`. Used by RSC prefetch and the BFF proxy. Falls back to `http://localhost:8080` in dev. |
| `LEDGER_API_KEY` | yes (production) | Server-only bearer key the BFF proxy and RSC prefetch attach to every upstream call. Issue a dedicated `name:scope:secret` key for the dashboard (see `docs/api.md`); `read` scope unless operators trigger writes from the UI. |
| `DASHBOARD_PASSWORD` | yes (production) | Operator password for the dashboard login. Unset in dev = login disabled; unset in a production build = the dashboard refuses to serve (503). |
| `DASHBOARD_SESSION_SECRET` | no | Explicit HMAC key for session cookies; derived from `DASHBOARD_PASSWORD` when unset. Set it when running multiple dashboard replicas so sessions survive uneven rollouts. |

## Getting Started

Start the backend (from the repo root: `docker compose up --build`, or run
`cmd/ledgerd` against a local PostgreSQL), then:

```bash
npm install
npm run dev
```

Open <http://localhost:3000>. Without env config the app targets
`http://localhost:8080` in development.

The `@azex/ledger-react` package itself lives in
[`packages/ledger-react/`](packages/ledger-react/) and is consumed here via
the workspace; see its README for the publish flow.
