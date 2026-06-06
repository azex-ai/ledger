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

| Var | Required | Description |
|-----|----------|-------------|
| `NEXT_PUBLIC_API_URL` | yes (production) | Base URL of the ledger API, e.g. `https://ledger.internal.example.com`. In production builds the client throws on the first API call if this is unset. |
| `NEXT_PUBLIC_API_KEY` | yes (any mutating call) | Bearer token sent on `POST` / `PUT` / `PATCH` / `DELETE` requests in `Authorization: Bearer <key>`. The backend rejects mutations without a valid key. |
| `LEDGER_API_URL_INTERNAL` | no | Server-only base URL for RSC prefetch (internal/private network). Falls back to `NEXT_PUBLIC_API_URL`. |
| `LEDGER_API_KEY` | no | Server-only API key for RSC prefetch — never enters the client bundle. |

In production deployments use a dedicated dashboard API key (don't reuse a
worker / service key). Read-only access (GET) does not require a key.

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
