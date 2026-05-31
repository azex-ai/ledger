# @azex/ledger-react

React UI + data-layer for the [azex-ai/ledger](https://github.com/azex-ai/ledger)
double-entry ledger engine. Ships typed hooks (TanStack Query), a router-agnostic
sidebar, dashboard widgets, ready-made admin **page** components, and an
all-in-one `<LedgerAdmin/>` shell.

## Install

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

## Exports

- **Root (`@azex/ledger-react`)** — `LedgerProvider`, `useLedgerClient`,
  `createLedgerClient`, all hooks (`useJournals`, `useBalances`,
  `useReservations`, …), `Sidebar`, `LEDGER_NAV_ITEMS`, `HealthCards`,
  `RecentJournals`, `StatusBadge`, the 11 non-chart `*Page` components,
  `LedgerAdmin`, and `Toaster`.
- **`@azex/ledger-react/charts`** — `DashboardPage`, `BalancesPage`,
  `BalanceTrend` (recharts-backed).
- **`@azex/ledger-react/styles.css`** — bundled Tailwind styles.
