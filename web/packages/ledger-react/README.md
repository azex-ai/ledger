# @azex/ledger-react

React UI + data-layer for the [azex-ai/ledger](https://github.com/azex-ai/ledger)
double-entry ledger engine. Ships typed hooks (TanStack Query), a router-agnostic
sidebar, dashboard widgets, ready-made admin **page** components, and an
all-in-one `<LedgerAdmin/>` shell.

## Install

This package is published to the **public npm registry** — install it directly,
no registry config or auth token required:

```bash
npm install @azex/ledger-react @tanstack/react-query
```

Peer deps: `react@^19`, `react-dom@^19`, `@tanstack/react-query@^5`.

## Setup

1. **Wrap your app in `<LedgerProvider>`** with the ledger API base URL (and
   optional API key). It owns a TanStack QueryClient unless you pass your own.

   ```tsx
   import { LedgerProvider } from "@azex/ledger-react";

   <LedgerProvider config={{ baseUrl: "https://ledger.example.com", apiKey }}>
     {children}
   </LedgerProvider>
   ```

2. **Import the stylesheet once** at your app root:

   ```ts
   import "@azex/ledger-react/styles.css";
   ```

3. **Mount `<Toaster/>` once** so page actions can surface toast feedback. Use
   the re-exported sonner `Toaster` (no direct sonner dependency needed):

   ```tsx
   import { Toaster } from "@azex/ledger-react";

   <Toaster theme="dark" position="bottom-right" />
   ```

   If you use `<LedgerAdmin/>` (below), it mounts its own `<Toaster/>` — skip
   this step.

## Usage

### Option A — `<LedgerAdmin/>` (zero routing)

The convenience shell renders the sidebar + content area, switches sections via
internal state (no URL), and self-mounts `<Toaster/>`. Chart-bearing pages are
lazy-loaded so `recharts` never enters your initial bundle.

```tsx
import { LedgerProvider, LedgerAdmin } from "@azex/ledger-react";
import "@azex/ledger-react/styles.css";

export default function Admin() {
  return (
    <LedgerProvider config={{ baseUrl: "https://ledger.example.com" }}>
      <LedgerAdmin />
    </LedgerProvider>
  );
}
```

### Option B — individual pages wired to your router

Import the `*Page` components and wire them to your host router. Each is a
`"use client"` component. Pages that link out accept an injectable
`linkComponent` (defaults to a plain `<a>`); `JournalDetailPage` takes the
journal `id` as a prop (extract it from your route param).

```tsx
import {
  JournalsPage,
  JournalDetailPage,
  ReservationsPage,
  DepositsPage,
  WithdrawalsPage,
  ClassificationsPage,
  JournalTypesPage,
  TemplatesPage,
  CurrenciesPage,
  ReconciliationPage,
  SnapshotsPage,
} from "@azex/ledger-react";
```

#### Chart-bearing pages — import from `@azex/ledger-react/charts`

`DashboardPage` and `BalancesPage` render `recharts` charts, so they ship from
the `./charts` subpath to keep `recharts` out of the root barrel. Import them
(and the `BalanceTrend` widget) from there:

```tsx
import { DashboardPage, BalancesPage, BalanceTrend } from "@azex/ledger-react/charts";
```

### Server prefetch (RSC) — import from `@azex/ledger-react/server`

For React Server Components / Route Handlers, prefetch ledger data on the
server and hydrate the client hooks with no client-side waterfall. The `/server`
entry has **no `"use client"` directive** and is server-only:

> **Never import `@azex/ledger-react/server` from a client component.**
> `createServerLedgerClient` takes the server API key — keeping this entry off
> the client barrel ensures the server key never reaches the client bundle.

```tsx
// app/journals/page.tsx (server component)
import { QueryClient, HydrationBoundary, dehydrate } from "@tanstack/react-query";
import { JournalsPage } from "@azex/ledger-react";
import {
  createServerLedgerClient,
  prefetchJournals,
} from "@azex/ledger-react/server";

export default async function Page() {
  const queryClient = new QueryClient();
  const client = createServerLedgerClient({ baseUrl, apiKey }); // server-side key
  await prefetchJournals(queryClient, client, 20);

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <JournalsPage linkComponent={YourLink} />
    </HydrationBoundary>
  );
}
```

Available `prefetch*` helpers: `prefetchJournals`, `prefetchEntries`,
`prefetchBalances`, `prefetchSystemHealth`, `prefetchSystemBalances`,
`prefetchReservations`, `prefetchClassifications`, `prefetchCurrencies`,
`prefetchJournalTypes`, `prefetchTemplates`, `prefetchSnapshots`. The shared
`ledgerKeys` query-key factory is also exported for advanced cache seeding.

## Theming

The Provider (and `<LedgerAdmin/>`) render a `<div className="ledger-root">`
wrapper. All design tokens are scoped under `.ledger-root` (default **dark**;
add `.light` for the light variant) so importing the stylesheet never leaks
tokens into your host app. Re-theme by overriding the CSS custom properties:

```css
.ledger-root {
  --primary: oklch(0.6 0.2 250);
  --radius: 0.5rem;
}
```

Or pass per-instance overrides inline via the provider `theme` prop (applied as
inline style on the `.ledger-root` div):

```tsx
<LedgerProvider config={{ baseUrl, theme: { "--primary": "oklch(0.6 0.2 250)" } }}>
```

## Reference integration

The `web/` app in [azex-ai/ledger](https://github.com/azex-ai/ledger) is the
working reference integration — it consumes this package as its only ledger
UI/data source (dogfood) and demonstrates both the client provider setup and
the `/server` RSC prefetch pattern with a Next.js `linkComponent` adapter.

## Exports

- **Root (`@azex/ledger-react`)** — `LedgerProvider`, `useLedgerClient`,
  `createLedgerClient`, all hooks (`useJournals`, `useBalances`,
  `useReservations`, …), `Sidebar`, `LEDGER_NAV_ITEMS`, `HealthCards`,
  `RecentJournals`, `StatusBadge`, the 11 non-chart `*Page` components,
  `LedgerAdmin`, and `Toaster`.
- **`@azex/ledger-react/charts`** — `DashboardPage`, `BalancesPage`,
  `BalanceTrend` (recharts-backed).
- **`@azex/ledger-react/server`** (server-only) — `createServerLedgerClient`,
  the `prefetch*` helpers, and `ledgerKeys`. No `"use client"` directive; never
  import from a client component.
- **`@azex/ledger-react/styles.css`** — bundled Tailwind styles.

The complete API reference (per-hook signatures, endpoints, component props,
prefetch helpers, query-key factory) lives in the repo at
[docs/frontend.md](https://github.com/azex-ai/ledger/blob/main/docs/frontend.md).

## Releasing

Publishing is tag-driven (CI workflow `ledger-react-publish.yml`). The tag
version must match `package.json` — CI fails fast otherwise.

1. Bump `version` in `web/packages/ledger-react/package.json`.
2. Commit the bump.
3. Tag and push:

   ```bash
   git tag ledger-react-v<version>   # e.g. ledger-react-v0.1.0
   git push --tags
   ```

The workflow re-runs the full verify gate (typecheck, test, build, artifact
assertions), asserts the tag matches `package.json`, then publishes to GitHub
Packages.
