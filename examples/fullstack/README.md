# Full-stack quickstart — chi scaffold + Next.js scaffold

The smallest end-to-end integration: a plain **chi** application that imports
the ledger Go library and serves its complete HTTP API, plus a plain
**Next.js** application that imports [`@azex/ledger-react`](https://www.npmjs.com/package/@azex/ledger-react)
from npm and renders the admin dashboard against it. Start from either
scaffold, add our packages, and the ledger service runs.

Two frontend flavors share the one backend — pick the one matching your
component library:

- **`web/`** (:3090) — default skin (`@azex/ledger-react`, shadcn-style,
  self-contained CSS, zero Tailwind setup required)
- **`web-heroui/`** (:3091) — HeroUI skin (`@azex/ledger-react/heroui`, for
  hosts already on HeroUI v3 + Tailwind v4)

```
┌──────────────────────────────┐        ┌──────────────────────────────┐
│ web/         (Next.js :3090) │  HTTP  │ backend/  (chi :8090)        │
│ web-heroui/  (Next.js :3091) │ ─────▶ │ r.Handle("/api/v1/*",        │
│ <LedgerProvider>             │        │   ledger HTTP API)           │
│   <LedgerAdmin/>             │        │ + your own routes            │
└──────────────────────────────┘        └──────────────┬───────────────┘
                                                       │ pgx
                                                 PostgreSQL 17
```

## Prerequisites

- PostgreSQL 17 reachable from your shell (any empty database works)
- Go 1.26+, Node 20+

## 1. Backend — chi app importing the ledger

What a fresh chi scaffold plus the ledger amounts to (see
[`backend/main.go`](backend/main.go), ~350 lines including seed data):

```go
svc, _ := ledger.New(pool)                     // the library facade
_ = presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), presets.DepositBundle())

r := chi.NewRouter()                           // your scaffold's router
r.Get("/", yourOwnHandler)                     // your routes stay yours
r.Handle("/api/v1/*", newLedgerAPI(svc))       // ledger API mounted alongside
```

Run it:

```bash
export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_example?sslmode=disable"
go run ./examples/fullstack/backend
```

On boot it migrates the schema, installs only the deposit presets, seeds
three demo USDC deposits (idempotent — restart-safe), starts the background worker
(rollups, snapshots, reconciliation), and listens on **:8090**.

Smoke-check:

```bash
curl -s localhost:8090/api/v1/system/health
curl -s localhost:8090/api/v1/journals | head -c 400
curl -s localhost:8090/api/v1/system/balances
```

The example runs with dev config: no API keys, wildcard CORS. For production,
switch `newLedgerAPI` to `server.LoadConfig()` and set `API_KEYS` +
`CORS_ALLOWED_ORIGIN`.

## 2. Frontend — Next.js app importing @azex/ledger-react

The whole integration is one page ([`web/app/page.tsx`](web/app/page.tsx)):

```tsx
"use client";

import { LEDGER_NAV_ITEMS, LedgerAdmin, LedgerProvider } from "@azex/ledger-react";

const navItems = LEDGER_NAV_ITEMS.filter(
  (item) => item.type === "separator" || item.href !== "/withdrawals",
);

const baseUrl =
  process.env.NEXT_PUBLIC_LEDGER_API_URL ?? "http://localhost:8090";

export default function Home() {
  return (
    <LedgerProvider config={{ baseUrl }}>
      <LedgerAdmin navItems={navItems} />
    </LedgerProvider>
  );
}
```

plus the package stylesheet import in `app/layout.tsx`
(`import "@azex/ledger-react/styles.css"`). The scaffold's `app/globals.css`
sets the host page height, margins and wallet width using plain CSS. No Tailwind
setup is required — the package ships compiled component CSS.

Run it:

```bash
cd examples/fullstack/web
npm install
npm run dev            # http://localhost:3090
```

Open http://localhost:3090 — the full admin dashboard (journals, balances,
reservations, bookings, classifications, reconciliation…) renders against your
backend, showing the three seeded deposits.

Point it at a different backend with
`NEXT_PUBLIC_LEDGER_API_URL=http://host:port npm run dev`.

Working from a clone with unreleased package changes? Build and pack the
workspace package, then install the tarball:

```bash
(cd ../../../web/packages/ledger-react && npm install && npm run build && npm pack)
npm install --no-save --package-lock=false ../../../web/packages/ledger-react/azex-ledger-react-*.tgz
```

## 2b. Frontend, HeroUI flavor — `web-heroui/`

For hosts already on HeroUI v3: same one-page integration, importing from the
`/heroui` subpath instead. `app/globals.css` carries the host-owned HeroUI
setup plus the skin's layout classes:

```css
@import "tailwindcss";
@import "@heroui/styles";
@import "@azex/ledger-react/heroui.css";
```

```bash
cd examples/fullstack/web-heroui
npm install
npm run dev            # http://localhost:3091
```

## 3. End-user wallet — `/wallet` (topology A)

The backend enables the holder wallet surface (`SetHolderSurface`) and adds a
demo session endpoint (`POST /api/session/wallet-token`) that mints a
short-lived, read-only holder token in-process via `server.MintHolderToken`
— stand-in for "session auth resolves user → holder". The web app's
`/wallet` page then renders `<WalletPanel/>` from
`@azex/ledger-react/wallet` with `getToken` pointed at that endpoint: the
ledger API key never reaches the browser, and an expired token is refreshed
automatically (401 → one `getToken` retry).

A host that doesn't want the admin API in its process would mount
`server.HolderHandler(...)` instead — the same three read endpoints, zero
admin routes.

## Deposit-only scope and credits

The shadcn demo omits Withdrawals from its configurable navigation. The backend
installs `DepositBundle` only. The admin API is still privileged bookkeeping
infrastructure: navigation filtering is not an authorization control. Expose the
holder surface and authenticated host routes to customers, keeping admin keys
server-side. The HeroUI example has not been polished in this work.

`/wallet` renders the real `DepositAddressCard` when
`NEXT_PUBLIC_LEDGER_DEPOSIT_NETWORK` is set. Before enabling it, wire the backend's
`SetDepositAddressProvider(onchain)` using the chain configuration demonstrated in
`examples/crypto-deposit`, and configure that provider to accept USDC on exactly
the network displayed. This scaffold does not guess a network or deploy a deposit
factory. Without configuration it displays an honest unavailable state.

For **1 USDC → 1,000 credits**, fixed/metered/streamed charging, zero-cost release,
and replay-safe ledger composition, run [credits-topup](../credits-topup/README.md).
The UI is currency-agnostic: a host that posts USDC and CREDITS journals will see
them as separate balances. `WalletPanel.actions(balance)` can offer USDC funding
without presenting CREDITS as an onchain deposit asset; `renderItem` and `slots`
allow local UI replacement while retaining the wallet hooks.

When testing unreleased UI changes, install the packed local package using
`npm install --no-save --package-lock=false <tarball>` before `npm run build`.
A registry install of the same version does not include unpublished checkout
changes.

## What this example deliberately skips

- **Auth** — see `server.LoadConfig` (API keys with read/write/admin scopes)
- **Webhook channels** — see `examples/crypto-deposit`
- **Event delivery** — see `examples/event-subscribe`
- **Composing ledger writes with your own tables** — see `examples/tx-compose`
