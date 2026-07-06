# Frontend Guide — `@azex/ledger-react`

React UI + data-layer for the ledger HTTP API. Ships typed TanStack Query
hooks, a router-agnostic sidebar, dashboard widgets, ready-made admin page
components, and an all-in-one `<LedgerAdmin/>` shell.

This is the long-form guide with the **full API reference**. The
[package README](../web/packages/ledger-react/README.md) is the condensed
consumer version published with the npm package (it also documents the release
procedure).

Source lives at [`web/packages/ledger-react/`](../web/packages/ledger-react/).
The repo's [`web/`](../web/) app is the working reference integration.

---

## Overview

The package exposes **three entry points**, each with a deliberate isolation
boundary:

| Entry | Import path | Contains | Why separate |
|-------|-------------|----------|--------------|
| Root | `@azex/ledger-react` | Client factory, provider, all hooks, sidebar/widgets, 11 non-chart page components, `LedgerAdmin`, `Toaster` | The default surface |
| Charts | `@azex/ledger-react/charts` | `DashboardPage`, `BalancesPage`, `BalanceTrend` | These statically import `recharts` (~275 KB); keeping them off the root barrel keeps recharts out of consumers' main bundle |
| Server | `@azex/ledger-react/server` | `createServerLedgerClient`, `prefetch*` helpers, `ledgerKeys` | Server-only (no `"use client"`); takes the **server API key**, which must never enter the client bundle |

Plus `@azex/ledger-react/styles.css` — the bundled stylesheet, scoped under
`.ledger-root` (see [Theming](#theming)).

**Peer dependencies**: `react@^19`, `react-dom@^19`, `@tanstack/react-query@^5`.
The package is framework-agnostic — no Next.js coupling; routing is injected
via a `linkComponent` prop.

All amounts are **strings** (backend `NUMERIC(30,18)`), never `number`. The
frontend only displays; the backend does all arithmetic.

## Install

The package is published to the **public npm registry** — install it directly,
no registry config or auth token required:

```bash
npm install @azex/ledger-react @tanstack/react-query
```

## Setup

1. **Wrap your app in `<LedgerProvider>`**. In the browser, do NOT put the
   ledger API key in the config — anything the provider holds ships in the
   public JS bundle. Point the client at a same-origin BFF proxy that holds
   the key server-side (see `web/src/app/api/v1/[...path]/route.ts` for the
   reference implementation):

   ```tsx
   import { LedgerProvider } from "@azex/ledger-react";

   // Browser: same-origin BFF, no credentials in the bundle.
   <LedgerProvider config={{ baseUrl: "" }}>
     {children}
   </LedgerProvider>
   ```

   The `apiKey` field is for **server-side** clients only
   (`createServerLedgerClient`, RSC prefetch, the BFF proxy itself).

2. **Import the stylesheet once** at your app root:

   ```ts
   import "@azex/ledger-react/styles.css";
   ```

3. **Mount `<Toaster/>` once** so page actions can surface toast feedback
   (re-exported from sonner — no direct sonner dependency needed). Skip this if
   you use `<LedgerAdmin/>`, which mounts its own:

   ```tsx
   import { Toaster } from "@azex/ledger-react";

   <Toaster theme="dark" position="bottom-right" />
   ```

Keep environment reads in **your** composition root — the package never reads
env. Resolve `baseUrl`/`apiKey` in one place and hand them to the provider
(see [`web/src/lib/ledger-env.ts`](../web/src/lib/ledger-env.ts) for the
reference pattern, including fail-loud production guards).

## Usage Modes

### Mode 1 — `<LedgerAdmin/>` (zero routing)

The convenience shell renders sidebar + content area, switches sections via
internal state (no URL), and self-mounts `<Toaster/>`. Chart-bearing pages are
lazy-loaded behind `<Suspense>` so recharts loads on demand as a separate
chunk.

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

### Mode 2 — individual pages wired to your router

Import the `*Page` components and wire them to your host router. Each is a
`"use client"` component. Pages that link out accept an injectable
`linkComponent` (defaults to a plain `<a>`); `JournalDetailPage` takes the
journal `id` as a prop (extract it from your route param).

```tsx
// components/next-link.tsx — Next.js adapter for the LinkComponent contract
"use client";
import Link from "next/link";
import type { ReactNode } from "react";

export function NextLink({ href, className, children }: {
  href: string; className?: string; children: ReactNode;
}) {
  return <Link href={href} className={className}>{children}</Link>;
}
```

```tsx
// app/journals/page.tsx
import { JournalsPage } from "@azex/ledger-react";
import { NextLink } from "@/components/next-link";

export default function Page() {
  return <JournalsPage linkComponent={NextLink} />;
}
```

Chart-bearing pages import from the `/charts` subpath instead:

```tsx
import { DashboardPage, BalancesPage } from "@azex/ledger-react/charts";
```

The repo's `web/` app wires all 13 pages this way — one route file per page
(see [`web/src/app/`](../web/src/app/)).

### Mode 3 — headless (hooks only)

Skip the shipped UI entirely and build your own on the hooks + client:

```tsx
import { useBalances, useJournals, useLedgerClient } from "@azex/ledger-react";

function MyBalances({ holder }: { holder: number }) {
  const { data, isLoading } = useBalances(holder); // polls every 15 s
  // ...render your own UI
}

// For endpoints without a dedicated hook, call the client directly:
function MyBookingButton() {
  const client = useLedgerClient();
  // client.createBooking(...), client.listEvents(...), client.batchBalances(...)
}
```

## Server Prefetch (RSC)

For React Server Components / Route Handlers, prefetch ledger data on the
server and hydrate the client hooks with no client-side waterfall. The
`/server` entry has **no `"use client"` directive** and is server-only.

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

export const dynamic = "force-dynamic";

export default async function Page() {
  const queryClient = new QueryClient();
  // Resolve config OUTSIDE any try/catch — a misconfig must fail loudly, not
  // be swallowed as a "best-effort prefetch" failure.
  const client = createServerLedgerClient({ baseUrl, apiKey }); // server-side key

  try {
    await prefetchJournals(queryClient, client, 20);
  } catch (err) {
    console.warn("[ledger] server prefetch failed, falling back to client fetch:", err);
  }

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <JournalsPage linkComponent={NextLink} />
    </HydrationBoundary>
  );
}
```

Each `prefetch*` helper seeds the cache under the **same `ledgerKeys` key and
with the same client method** its matching hook uses, so hydration hits with
zero refetch. The infinite-query helpers (`prefetchJournals`,
`prefetchEntries`) mirror the hook's `initialPageParam`/`getNextPageParam` so
the hydrated shape matches `useInfiniteQuery`.

**Intentional omission — no `prefetchBookings`/`prefetchDeposits`/`prefetchWithdrawals`.**
The `useDeposits`/`useWithdrawals` hooks key on a numeric `classificationId`
resolved at runtime from a separate `classifications(true)` query. Prefetching
bookings is therefore a two-step server flow (prefetch classifications →
resolve the id → list bookings under that id) that the caller must
orchestrate — it is left to the host rather than hidden behind a single helper
that would mask the dependency.

## Theming

The provider (and `<LedgerAdmin/>`) render a `<div className="ledger-root">`
wrapper. All design tokens are scoped under `.ledger-root` (default **dark**;
add `.light` for the light variant) so importing the stylesheet never leaks
tokens into the host app. The bundled CSS is preflight-free — package and host
styles coexist.

Re-theme by overriding the CSS custom properties:

```css
.ledger-root {
  --primary: oklch(0.6 0.2 250);
  --radius: 0.5rem;
}
```

Or pass per-instance overrides via the provider `theme` prop (applied as
inline style on the `.ledger-root` div):

```tsx
<LedgerProvider config={{ baseUrl, theme: { "--primary": "oklch(0.6 0.2 250)" } }}>
```

---

# API Reference

Everything below is source-verified against
`web/packages/ledger-react/src/`. Endpoints map 1:1 onto the HTTP API
documented in [api.md](api.md) / [openapi.yaml](openapi.yaml).

## Client Layer

### `createLedgerClient(config: LedgerClientConfig): LedgerClient`

Framework-agnostic fetch wrapper. Unwraps the backend envelope
(`{code, message, data}`) and returns `data`; throws `ApiRequestError` on
non-2xx. Sends `Authorization: Bearer <apiKey>` on every request when the
key is configured — the backend enforces auth on reads too.

### `LedgerClientConfig`

| Field | Type | Notes |
|-------|------|-------|
| `baseUrl` | `string` | Ledger API origin, e.g. `https://ledger.example.com` |
| `apiKey?` | `string` | Sent as Bearer token on every request. Server-side use only — never hand it to a browser `LedgerProvider`; route browser traffic through a same-origin BFF proxy instead |
| `fetch?` | `typeof fetch` | Override for server use / tests. **Must be a stable reference** (module-level or `useCallback`'d) — `LedgerProvider` keys its client memo on this field, so an inline arrow rebuilds the client every render |

### `ApiRequestError`

```ts
class ApiRequestError extends Error {
  status: number;          // HTTP status
  apiError: ApiError;      // { code: number; message: string } business error
}
```

### `LedgerClient` methods

The client covers the full HTTP surface — including endpoints that have no
dedicated hook (call these via `useLedgerClient()`):

| Group | Methods |
|-------|---------|
| System | `getHealth()`, `getSystemBalances()` |
| Journals | `listJournals({cursor?, limit?})`, `getJournal(id)`, `postJournal(body)`, `postTemplateJournal(body)`, `reverseJournal(id, reason)` |
| Entries | `listEntries({holder?, currency_uid?, cursor?, limit?})` |
| Balances | `getBalances(holder)`, `getBalancesByCurrency(holder, currency)`, `batchBalances(holderIds, currencyId)` |
| Reservations | `listReservations({holder?, status?, limit?})`, `createReservation(body)`, `settleReservation(id, actualAmount)`, `releaseReservation(id)` |
| Bookings | `createBooking(CreateBookingBody)`, `transitionBooking(id, TransitionBookingBody)`, `getBooking(id)`, `listBookings(ListBookingsParams)` |
| Events | `getEvent(id)`, `listEvents({classification_code?, booking_id?, to_status?, cursor?, limit?})` |
| Classifications | `listClassifications(activeOnly?)`, `createClassification(body)`, `deactivateClassification(id)` |
| Journal types | `listJournalTypes(activeOnly?)`, `createJournalType(body)`, `deactivateJournalType(id)` |
| Templates | `listTemplates(activeOnly?)`, `createTemplate(body)`, `deactivateTemplate(id)`, `previewTemplate(code, params)` |
| Currencies | `listCurrencies(activeOnly?)`, `createCurrency(body)`, `deactivateCurrency(id)` |
| Reconciliation | `reconcileGlobal()`, `reconcileAccount(holder, currencyId)` |
| Snapshots | `listSnapshots({holder?, currency_uid?, start?, end?})` |

## Provider

### `<LedgerProvider config={LedgerProviderConfig}>`

Builds the client (memoized on `baseUrl`/`apiKey`/`fetch`), provides it via
context, mounts a `QueryClientProvider`, and renders the `.ledger-root`
theming wrapper.

### `LedgerProviderConfig` (extends `LedgerClientConfig`)

| Field | Type | Notes |
|-------|------|-------|
| `queryClient?` | `QueryClient` | Reuse the host app's QueryClient. Omitted → the provider creates and owns its own |
| `onError?` | `(err: unknown) => void` | Global error sink wired into the **package-created** QueryClient's query + mutation caches. **Not applied when `queryClient` is injected** — the host owns its caches and wires its own handling |
| `theme?` | `Record<string, string>` | CSS custom-property overrides applied inline to `.ledger-root` |

### `useLedgerClient(): LedgerClient`

Returns the configured client. **Throws** if called outside `<LedgerProvider>`
(loud error rather than a silent null).

## Hooks

All query hooks return TanStack Query results (`UseQueryResult` /
`UseInfiniteQueryResult`); all mutation hooks return `UseMutationResult`.
Query keys come from the shared `ledgerKeys` factory (see
[Query keys](#query-key-factory--ledgerkeys)).

Mutation hooks built on the internal `useLedgerMutation` wrapper invalidate
their own namespace **plus** `balances` and `system-balances` on success
(any journal/booking/reservation mutation can move balances).

`enabled` gating convention: **holder `0` means "no account"** and disables
the query; *negative* holders are valid (system counterpart accounts), so
hooks gate on `holder !== 0`, never `holder > 0`.

### `useLedgerMutation(mutationFn, invalidateKeys)`

The exported wrapper itself, for building custom mutations with the same
invalidation behavior. `invalidateKeys` are bare namespace segments (e.g.
`["journals"]`), auto-prefixed under `["ledger", ...]`:

```ts
const mutation = useLedgerMutation((body) => client.postJournal(body), ["journals"]);
```

### Journals & entries — `src/hooks/use-journals.ts`

| Hook | Signature | Returns / Endpoint | Notes |
|------|-----------|--------------------|-------|
| `useJournals` | `(limit = 20)` | Infinite query → `GET /api/v1/journals` | Cursor pagination via `next_cursor` |
| `useJournal` | `(id: string)` | Query → `GET /api/v1/journals/{uid}` (`JournalWithEntries`) | `enabled: id !== ""`. Keyed `["ledger","journal",id]` (singular) so list-namespace invalidation doesn't refetch every detail page |
| `usePostJournal` | `()` | Mutation → `POST /api/v1/journals` | Body: `{journal_type_id, idempotency_key, entries[], source?, metadata?}`. Invalidates `journals` |
| `usePostTemplateJournal` | `()` | Mutation → `POST /api/v1/journals/template` | Body: `{template_code, holder_id, currency_uid, idempotency_key, amounts, source?}`. Invalidates `journals` |
| `useReverseJournal` | `()` | Mutation → `POST /api/v1/journals/{uid}/reverse` | Variables: `{id, reason}`. Invalidates `journals` |
| `useEntries` | `(params: {holder?, currency_uid?}, limit = 50)` | Infinite query → `GET /api/v1/entries` | `enabled: params.holder !== undefined && params.holder !== 0` |

### Balances — `src/hooks/use-balances.ts`

| Hook | Signature | Returns / Endpoint | Notes |
|------|-----------|--------------------|-------|
| `useBalances` | `(holder: number)` | Query → `GET /api/v1/balances/{holder}` (`Balance[]`) | `enabled: holder !== 0`; **polls every 15 s** |
| `useBalancesByCurrency` | `(holder, currency)` | Query → `GET /api/v1/balances/{holder}/{currency}` | `enabled: holder !== 0 && currency > 0`; polls every 15 s |

### Deposits — `src/hooks/use-deposits.ts`

Deposits are bookings under the classification with code `"deposit"`. The
classification id is resolved at runtime from a shared, long-cached
(`staleTime: 5 min`) classifications query.

| Hook | Signature | Returns / Endpoint | Notes |
|------|-----------|--------------------|-------|
| `useDepositClassificationId` | `()` | `number` | Returns `0` (falsy) until classifications load |
| `useDeposits` | `(params: {holder?, status?})` | Query → `GET /api/v1/bookings?classification_uid=…` (`Booking[]`) | `enabled: classificationId > 0` |
| `useConfirmingDeposit` | `()` | Mutation → transition `pending → confirming` | Variables: `{id, channelRef}` (external tx ref) |
| `useConfirmDeposit` | `()` | Mutation → transition `confirming → confirmed` | Variables: `{id, actual_amount, channel_ref}` — actual amount may differ from expected within tolerance |
| `useFailDeposit` | `()` | Mutation → transition `→ failed` | Variables: `{id, reason}` (stored in metadata) |

All transitions hit `POST /api/v1/bookings/{uid}/transition` and invalidate
the `bookings` namespace.

### Withdrawals — `src/hooks/use-withdrawals.ts`

Withdrawals are bookings under classification code `"withdraw"`. Note the
preset lifecycle has an explicit `failed → reserved` retry edge — `failed` is
not terminal.

| Hook | Signature | Transition | Notes |
|------|-----------|-----------|-------|
| `useWithdrawClassificationId` | `()` | — | Returns `0` until classifications load |
| `useWithdrawals` | `(params: {holder?, status?})` | — | Query (`Booking[]`), `enabled: classificationId > 0` |
| `useReserveWithdraw` | `()` | `→ reserved` | Variables: `id: number` |
| `useReviewWithdraw` | `()` | `→ processing` (approved) / `→ failed` (rejected) | Variables: `{id, approved: boolean}` |
| `useProcessWithdraw` | `()` | `→ processing` | Variables: `{id, channelRef}` |
| `useConfirmWithdraw` | `()` | `→ confirmed` | Variables: `id: number` |
| `useFailWithdraw` | `()` | `→ failed` | Variables: `{id, reason}` |
| `useRetryWithdraw` | `()` | `failed → reserved` | Variables: `id: number` |

### Metadata — `src/hooks/use-metadata.ts`

CRUD trios for the four metadata resources. Each `useCreate*`/`useDeactivate*`
invalidates its own namespace only (metadata changes don't move balances).

| Hook | Signature | Endpoint |
|------|-----------|----------|
| `useClassifications` | `(activeOnly?: boolean)` | `GET /api/v1/classifications` |
| `useCreateClassification` | `()` — body `{code, name, normal_side, is_system}` | `POST /api/v1/classifications` |
| `useDeactivateClassification` | `()` — variables `id: string` | `POST /api/v1/classifications/{uid}/deactivate` |
| `useJournalTypes` | `(activeOnly?)` | `GET /api/v1/journal-types` |
| `useCreateJournalType` | `()` — body `{code, name}` | `POST /api/v1/journal-types` |
| `useDeactivateJournalType` | `()` — variables `id` | `POST /api/v1/journal-types/{uid}/deactivate` |
| `useTemplates` | `(activeOnly?)` | `GET /api/v1/templates` |
| `useCreateTemplate` | `()` — body `{code, name, journal_type_id, lines[]}` | `POST /api/v1/templates` |
| `useDeactivateTemplate` | `()` — variables `id` | `POST /api/v1/templates/{uid}/deactivate` |
| `usePreviewTemplate` | `()` — variables `{code, holder_id, currency_uid, ...amounts}` | `POST /api/v1/templates/{code}/preview` (returns `PreviewResult`, no invalidation — read-only preview) |
| `useCurrencies` | `(activeOnly?)` | `GET /api/v1/currencies` |
| `useCreateCurrency` | `()` — body `{code, name, exponent}` | `POST /api/v1/currencies` |
| `useDeactivateCurrency` | `()` — variables `id` | `POST /api/v1/currencies/{uid}/deactivate` |

### Reservations — `src/hooks/use-reservations.ts`

| Hook | Signature | Endpoint | Notes |
|------|-----------|----------|-------|
| `useReservations` | `(params: {holder?, status?})` | `GET /api/v1/reservations` | |
| `useSettleReservation` | `()` — variables `{id, actualAmount: string}` | `POST /api/v1/reservations/{uid}/settle` | Invalidates `reservations` (+ balances) |
| `useReleaseReservation` | `()` — variables `id: string` | `POST /api/v1/reservations/{uid}/release` | Invalidates `reservations` (+ balances) |

### System — `src/hooks/use-system.ts`

| Hook | Signature | Endpoint | Notes |
|------|-----------|----------|-------|
| `useHealth` | `()` | `GET /api/v1/system/health` (`HealthStatus`) | **Polls every 10 s** |
| `useSystemBalances` | `()` | `GET /api/v1/system/balances` (`SystemBalance[]`) | |
| `useReconcileGlobal` | `()` | Mutation → `POST /api/v1/reconcile` (`ReconcileResult`) | Plain mutation, no invalidation |
| `useReconcileAccount` | `()` — variables `{holder, currencyId}` | Mutation → `POST /api/v1/reconcile/account` | Plain mutation, no invalidation |
| `useSnapshots` | `(params: {holder?, currency_uid?, start?, end?})` | `GET /api/v1/snapshots` (`Snapshot[]`) | `enabled: params.holder !== undefined && params.holder !== 0` |

## Components

### Navigation

| Export | Kind | Details |
|--------|------|---------|
| `Sidebar` | Component | Props `SidebarProps { pathname: string; linkComponent?: LinkComponent }`. Responsive: desktop rail + mobile drawer. Active state: exact match for `/`, prefix match otherwise |
| `LEDGER_NAV_ITEMS` | Const | `readonly LedgerNavItem[]` — the 13 sections + 2 separators (Metadata, Operations), each with href, label, lucide icon |
| `DefaultLink` | Component | Plain `<a>` renderer — the default when no `linkComponent` is injected |
| `LinkComponent` | Type | `ComponentType<{ href: string; className?: string; children: ReactNode }>` — the router-injection contract |
| `LedgerNavItem` | Type | `{ href, label, icon } \| { type: "separator", label }` |

### Dashboard widgets

| Export | Props | Details |
|--------|-------|---------|
| `HealthCards` | none | Renders status cards from `useHealth()` (status, rollup queue depth, checkpoint age, active reservations) |
| `RecentJournals` | `RecentJournalsProps { linkComponent? }` | Latest 10 journals via `useJournals(10)` |
| `StatusBadge` | `{ status: string }` | Color-coded badge; knows booking/reservation/journal statuses (`pending`, `confirming`, `confirmed`, `failed`, `reserved`, `processing`, `settled`, `released`, `reversed`, …) and `debit`/`credit` |
| `BalanceTrend` (from `/charts`) | none | recharts bar chart of system balances from `useSystemBalances()` |

### Page components

Eleven non-chart pages on the root barrel, two chart pages on `/charts`. All
are `"use client"` components that fetch their own data through the hooks —
mount inside `<LedgerProvider>` and they work.

| Component | Entry | Props |
|-----------|-------|-------|
| `JournalsPage` | root | `{ linkComponent?: LinkComponent }` |
| `JournalDetailPage` | root | `{ id: number; linkComponent?: LinkComponent }` — host extracts `id` from its route param |
| `ReservationsPage` | root | none |
| `DepositsPage` | root | none |
| `WithdrawalsPage` | root | none |
| `ClassificationsPage` | root | none |
| `JournalTypesPage` | root | none |
| `TemplatesPage` | root | none |
| `CurrenciesPage` | root | none |
| `ReconciliationPage` | root | none |
| `SnapshotsPage` | root | none |
| `DashboardPage` | `/charts` | `{ linkComponent?: LinkComponent }` — composes `HealthCards` + `BalanceTrend` + `RecentJournals` |
| `BalancesPage` | `/charts` | none — balance lookup by holder + 30-day trend chart |

### `LedgerAdmin`

| Export | Props | Details |
|--------|-------|---------|
| `LedgerAdmin` | none | All-in-one shell: Sidebar + content area, section switching via internal state (no URL; an internal `linkComponent` intercepts nav clicks, `/journals/{uid}` links open `JournalDetailPage`). Self-mounts `<Toaster/>`. Lazy-loads `DashboardPage`/`BalancesPage` so recharts ships as an async chunk |

### `Toaster`

Re-export of sonner's `Toaster` so consumers don't need a direct sonner
dependency. Mount once at the app root (unless using `LedgerAdmin`).

## Server entry — `@azex/ledger-react/server`

### `createServerLedgerClient(config: LedgerClientConfig): LedgerClient`

Same factory as `createLedgerClient` — the distinct name + subpath exist to
signal server-only usage and keep server config (internal base URL, server API
key) off the client barrel.

### Prefetch helpers

All take `(queryClient: QueryClient, client: LedgerClient, ...params)` and
return `Promise<void>`. Each mirrors its hook exactly (same key, same client
method):

| Helper | Params after `(qc, client, …)` | Mirrors |
|--------|-------------------------------|---------|
| `prefetchJournals` | `limit = 20` | `useJournals(limit)` (infinite shape) |
| `prefetchEntries` | `params: {holder?, currency_uid?}, limit = 50` | `useEntries(params, limit)` (infinite shape) |
| `prefetchBalances` | `holder: number` | `useBalances(holder)` |
| `prefetchSystemHealth` | — | `useHealth()` |
| `prefetchSystemBalances` | — | `useSystemBalances()` |
| `prefetchReservations` | `params: {holder?, status?}` | `useReservations(params)` |
| `prefetchClassifications` | `activeOnly?: boolean` | `useClassifications(activeOnly)` |
| `prefetchCurrencies` | `activeOnly?` | `useCurrencies(activeOnly)` |
| `prefetchJournalTypes` | `activeOnly?` | `useJournalTypes(activeOnly)` |
| `prefetchTemplates` | `activeOnly?` | `useTemplates(activeOnly)` |
| `prefetchSnapshots` | `params: {holder?, currency_uid?, start?, end?}` | `useSnapshots(params)` |

(No bookings prefetch — see [Server Prefetch](#server-prefetch-rsc) for why.)

### Query-key factory — `ledgerKeys`

Exported for advanced cache seeding/reading. The **single source of truth**
for query keys — hooks and prefetch helpers both build keys here, so they can
never drift (drift = silent hydration miss + client refetch).

```ts
ledgerKeys.health()                              // ["ledger","health"]
ledgerKeys.systemBalances()                      // ["ledger","system-balances"]
ledgerKeys.journals(limit)                       // ["ledger","journals",limit]
ledgerKeys.journal(id)                           // ["ledger","journal",id]
ledgerKeys.entries(params)                       // ["ledger","entries",params]
ledgerKeys.balances(holder)                      // ["ledger","balances",holder]
ledgerKeys.balancesByCurrency(holder, currency)  // ["ledger","balances",holder,currency]
ledgerKeys.reservations(params)                  // ["ledger","reservations",params]
ledgerKeys.snapshots(params)                     // ["ledger","snapshots",params]
ledgerKeys.classifications(activeOnly)           // ["ledger","classifications",activeOnly]
ledgerKeys.journalTypes(activeOnly)              // ["ledger","journal-types",activeOnly]
ledgerKeys.templates(activeOnly)                 // ["ledger","templates",activeOnly]
ledgerKeys.currencies(activeOnly)                // ["ledger","currencies",activeOnly]
ledgerKeys.bookings(code, params)                // ["ledger","bookings",code,params]
```

## Domain types

All types are re-exported from the root barrel (`export type * from
"./client/types"`). Field-level detail lives in the TypeScript definitions —
this is the orientation map:

| Group | Types | Key fields |
|-------|-------|-----------|
| Journals | `Journal`, `Entry`, `JournalWithEntries` | `total_debit`/`total_credit`/`amount` are strings; `Journal.reversal_of: number \| null` |
| Balances | `Balance`, `SystemBalance` | `(account_holder, currency_uid, classification_uid)` dimensions; `balance: string` |
| Bookings | `Booking`, `Event`, `CreateBookingBody`, `TransitionBookingBody`, `ListBookingsParams` | `Booking.reservation_id`/`journal_id` are `number \| null` (not yet linked); lifecycle governed by the classification |
| Reservations | `Reservation` | `status: "active" \| "settling" \| "settled" \| "released"` |
| Metadata | `Classification`, `Lifecycle`, `JournalType`, `EntryTemplate`, `TemplateLine`, `Currency` | `Classification.lifecycle: Lifecycle \| null` (null = label-only) |
| System | `HealthStatus`, `ReconcileResult`, `Snapshot` | |
| Plumbing | `ApiError`, `PaginatedResponse<T>`, `PreviewResult` | `PaginatedResponse = { data: T[]; next_cursor: string }` |

## Design notes

- **Query-key alignment** — `ledgerKeys` is defined once and shared by hooks
  and server prefetch. Namespace-wide invalidation goes through prefix keys
  derived from the same factory, so a key rename can't silently break
  invalidation.
- **Automatic balance invalidation** — `useLedgerMutation` invalidates
  `balances` + `system-balances` after every namespaced mutation; any money
  movement refreshes balances without per-callsite wiring.
- **Router agnosticism** — no router dependency anywhere; hosts inject
  `linkComponent` (see [`web/src/components/next-link.tsx`](../web/src/components/next-link.tsx)
  for the Next.js adapter). `LedgerAdmin` falls back to internal state routing.
- **Negative holders are valid** — system counterpart accounts are negative
  holder IDs, so query gating is `holder !== 0`, never `holder > 0`.
- **Classification-driven deposit/withdraw views** — deposits and withdrawals
  are not distinct resources; they're bookings filtered by a classification id
  resolved at runtime (cached 5 min). This mirrors the backend's
  classification-driven architecture.
- **Polling** — `useBalances`/`useBalancesByCurrency` refetch every 15 s,
  `useHealth` every 10 s; everything else relies on invalidation.

## See also

- [Package README](../web/packages/ledger-react/README.md) — condensed consumer
  docs + release procedure (tag-driven publish)
- [`web/`](../web/) — reference integration (Next.js 16, RSC prefetch,
  `linkComponent` adapter)
- [api.md](api.md) / [openapi.yaml](openapi.yaml) — the HTTP API these hooks
  call
- [INVARIANTS.md](INVARIANTS.md) — what the backend guarantees about the data
  these components display
