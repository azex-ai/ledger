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

Peer deps: `react@^19`, `react-dom@^19`, `@tanstack/react-query@^5`, plus
`@heroui/react@^3` — optional, and only for the HeroUI skin below.

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
  DepositReviewsPage,
  WithdrawalsPage,
  ClassificationsPage,
  JournalTypesPage,
  TemplatesPage,
  CurrenciesPage,
  ReconciliationPage,
  SnapshotsPage,
  SweepMonitorPage,
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
import { QueryClient, dehydrate } from "@tanstack/react-query";
import {
  createServerLedgerClient,
  prefetchJournals,
} from "@azex/ledger-react/server";
import { requireAdminSession } from "@/lib/auth"; // host authentication
import { serverLedgerConfig } from "@/lib/server-ledger-config";
import { LedgerSession } from "./ledger-session";

export default async function Page() {
  const session = await requireAdminSession();
  const cacheScope = {
    backend: "production-ledger-tenant-east",
    identity: session.cacheIdentity, // non-secret principal/session/permission revision
  };
  const queryClient = new QueryClient(); // request-local; never a module singleton
  const client = createServerLedgerClient({ ...serverLedgerConfig, cacheScope });
  await prefetchJournals(queryClient, client, 20);

  return (
    <LedgerSession
      key={JSON.stringify(cacheScope)}
      cacheScope={cacheScope}
      state={dehydrate(queryClient)}
    />
  );
}
```

```tsx
// app/journals/ledger-session.tsx (client component)
"use client";

import { useState } from "react";
import Link from "next/link";
import { HydrationBoundary, QueryClient, type DehydratedState } from "@tanstack/react-query";
import { JournalsPage, LedgerProvider, type LedgerCacheScope } from "@azex/ledger-react";

export function LedgerSession({ cacheScope, state }: {
  cacheScope: LedgerCacheScope;
  state: DehydratedState;
}) {
  const [queryClient] = useState(() => new QueryClient({
    defaultOptions: { queries: { staleTime: 60_000 } },
  }));
  return (
    <LedgerProvider config={{ baseUrl: "", cacheScope, queryClient }}>
      <HydrationBoundary state={state}>
        <JournalsPage linkComponent={Link} />
      </HydrationBoundary>
    </LedgerProvider>
  );
}
```

The browser's empty `baseUrl` addresses the host's `/api/v1` BFF. The server
may use an internal URL and private API key. Their **logical** `cacheScope`
must match even when their URLs differ; credentials stay server-side.
The example's `requireAdminSession`, `session.cacheIdentity`, and
`serverLedgerConfig` belong to the host, not this SDK. A nonzero `staleTime`
avoids the normal background revalidation of freshly hydrated data.

Available `prefetch*` helpers: `prefetchJournals`, `prefetchEntries`,
`prefetchBalances`, `prefetchSystemHealth`, `prefetchSystemBalances`,
`prefetchReservations`, `prefetchClassifications`, `prefetchCurrencies`,
`prefetchJournalTypes`, `prefetchTemplates`, `prefetchSnapshots`. The shared
`ledgerKeys` query-key factory is also exported for advanced cache seeding.
It now requires the resolved scope as its first argument, for example
`ledgerKeys.balances(client.cacheScope, 42)` or `ledgerKeys.all(client.cacheScope)`.
Unscoped keys from earlier versions no longer match hooks or prefetch helpers.

### Admin cache ownership and identity changes

`LedgerClientConfig` / `LedgerProviderConfig` accept an optional
`cacheScope: { backend: string; identity: string }`. Both IDs must be non-empty
and non-secret: never use API keys, access tokens, cookie values, or credentials
embedded in URLs. Query keys and dehydrated state are observable by the host.
The SDK rejects a known `apiKey` copied into scope, but cannot recognize every
secret a host might supply. Scope is cache partitioning, not authorization.

- **Default:** every constructed client gets a stable, opaque instance scope.
  A provider retains its client while `baseUrl`, `apiKey`, and the `fetch`
  reference are unchanged. Observable configuration changes create a new
  client/scope; separately mounted providers do not share data even if their
  configuration is equal. Keep custom `fetch` references stable. Construct a
  new client instead of mutating its original configuration object.
- **Explicit scope:** equal logical backend + identity IDs intentionally share
  data, including between server and browser clients. The host asserts that
  these IDs represent the same backend, principal and permission revision.
  If an API key changes while the explicit identity stays the same, the SDK
  continues sharing that cache; it cannot infer the real principal from a key.
- **Hidden BFF cookies:** changing a cookie behind an unchanged URL/config is
  not observable by the SDK. On login, logout, account/tenant switch or permission
  changes, the host must change the non-secret scope or remount the provider
  with a new QueryClient. Rerendering the same configuration is insufficient.
- **Identity boundaries:** provider scope changes reset local child/mutation
  state. In-flight old requests can finish, but their cache writes, optimistic
  rollback and invalidation stay in the original scope. An injected QueryClient
  can retain old entries; hosts that need to discard them on logout should
  remove them or replace that QueryClient. A scope does not purge memory.

Server prefetch helpers take their scope from the supplied client automatically.
SSR hydration across separate clients requires an explicit matching scope;
default instance scopes intentionally do not match. Create server QueryClients
per request and dehydrate only data authorized for the response's identity.

### Bundled Next.js dashboard deployment

The repository's `web/` host now requires **`LEDGER_CACHE_BACKEND_ID` in
production**, in addition to its existing backend/auth configuration. Set a
stable, public logical ID such as `production-ledger-east`; change it when
switching the actual backend. Never put an internal URL or credential in this
value. Development falls back to `local-ledger`. This setting is resolved at
request time, so `next build` does not require deployment secrets or this ID.

The host's server-only `getDashboardCacheScope()` verifies the session cookie
before deriving `operator-session:<expiresAtMs>` from its public expiry.
Missing, invalid, or expired sessions use `anonymous`; auth-off development
uses `development-open`. The existing signed token format has no session nonce:
minting at the same millisecond produces the same token and cache identity.
This identifies the existing session boundary, not every individual login event.
Token/signature/key values and private backend URLs are never included in scope.

The async root layout reads request cookies and passes the scope to the client
provider; both prefetched pages resolve the same scope. The existing login and
logout `router.refresh()` updates that prop and resets the provider's scoped
subtree even when the root layout remains mounted. Cookie access makes pages
under the root layout dynamic. The host still uses TanStack Query's default
stale time, so hydrated data appears immediately and may revalidate in the
background. Authentication continues to be enforced by the proxy and BFF.

## HeroUI skin — `@azex/ledger-react/heroui`

Building on HeroUI v3 instead of the default shadcn-style components? The same
admin surface (provider, `<LedgerAdmin/>`, all pages) ships as a second skin
on the `./heroui` subpath, backed by the identical headless core — with the
same chart split as the root barrel, so `DashboardPage` / `BalancesPage` come
from `@azex/ledger-react/heroui/charts`:

```tsx
import { LedgerAdmin, LedgerProvider } from "@azex/ledger-react/heroui";
```

Host contract: `@heroui/react` + `@heroui/styles` installed (optional peer —
consumers of the default skin never need it), Tailwind v4 configured, and one
stylesheet import for the skin's layout classes:

```css
/* globals.css */
@import "tailwindcss";
@import "@heroui/styles";
@import "@azex/ledger-react/heroui.css";
```

Theme (light/dark, palette) is owned by the host's HeroUI setup — the skin
renders with whatever theme the surrounding app defines.

### Headless — `@azex/ledger-react/headless`

The UI-free core both skins build on (typed client + provider + every
TanStack Query hook) is importable directly for hosts that bring their own
components:

```tsx
import { LedgerProvider, useJournals } from "@azex/ledger-react/headless";
```

Every amount on the wire is a raw `NUMERIC(30,18)` string (`financial.md` —
never parse it as a `number` yourself). This entry also re-exports the same
formatting helpers the shipped UI uses, so a headless host's own components
stay on the same banding table instead of reimplementing it:
`formatAmount`, `formatSignedAmount`, `formatCompact`, `validateAmount`,
`formatUTC`, `formatDateUTC`, `shortenAddress`, `shortenHash`, viem's
`parseUnits`/`formatUnits` (and friends), plus the amount-string arithmetic
helpers (`addAmounts`, `subAmounts`, `gtAmount`, `gteAmount`, `isZeroAmount`).
Full reference: [`docs/frontend.md`](../../../docs/frontend.md#display-and-decimal-utilities).

## End-user wallet — `@azex/ledger-react/wallet`

The holder-scoped wallet surface for YOUR users (not operators): balances
(available / pending / on hold), translated transaction history, refund
markers. Fund-changing flows stay in the host product and slot in via
`actions`. `DepositAddressCard` can issue the holder's deposit address; the
wallet does not initiate withdrawals.

```tsx
import { WalletPanel, WalletProvider } from "@azex/ledger-react/wallet";

<WalletProvider config={{ baseUrl: "/api/v1", getToken: fetchWalletToken }}>
  <WalletPanel actions={<TopUpButton />} kindLabels={{ deposit: "Top up" }} />
</WalletProvider>;
```

`getToken` returns a holder token your backend mints (HTTP
`POST /holder-tokens` or in-process `server.MintHolderToken`); it is called
lazily and once more after a 401. Omit it behind a same-origin BFF.
Components: `WalletPanel`, `WalletBalances`, `WalletBalanceCard`,
`TransactionList`, `DepositAddressCard`. HeroUI variant at `@azex/ledger-react/wallet/heroui`,
UI-free core (client + hooks) at `@azex/ledger-react/wallet/headless`.

The default shadcn wallet supports gradual UI replacement. `WalletPanel`,
`WalletBalances`, and `WalletBalanceCard` accept either a React node in
`actions` or a renderer receiving the current `WalletBalance`. When there
are no balances yet, that renderer receives `null`, and the resulting action
remains visible beside the empty state so a new user can make a first deposit.
For example, in a client component:

```tsx
<WalletPanel
  actions={(balance) =>
    balance?.currency_code === "CREDITS" ? <BuyCreditsButton /> : <TopUpButton />
  }
  kindLabels={{ deposit: "Top up" }}
  renderItem={(transaction) => <UsageRow transaction={transaction} />}
  limit={10}
/>
```

`renderItem` replaces each transaction row's content while the package keeps
loading, errors, empty states, and cursor pagination. To replace a complete
region, use `slots={{ balances: <CreditsSummary /> }}` or
`slots={{ transactions: <MyActivity /> }}` on `WalletPanel`. Omitted slots
keep the defaults; `null` hides the whole region. Replaced regions do not
mount their default queries. Custom components can use the exported wallet
hooks under the same `WalletProvider`.

The shadcn balance, transaction, and hold amounts retain the standard display
bands. Focus, hover, or click an amount to inspect its full decimal string
with its currency; the exact value is also the control's accessible label.
This preserves fractional USDC and credits without converting them to a
JavaScript number. These customization props are specific to the shadcn
wallet; the HeroUI wallet retains its existing API.

## Theming

The Provider (and `<LedgerAdmin/>`) render a `<div className="ledger-root">`
wrapper. The package's **business design tokens** (`--primary`, `--background`,
`--chart-*`, …) are scoped under `.ledger-root`, so importing the stylesheet
never overrides your host app's own theme. The stylesheet is also
**self-contained**: it ships its own `.ledger-root`-scoped preflight (fonts,
resets, table borders), so it renders correctly even in a host with no
Tailwind setup at all.

One caveat inherent to Tailwind v4: the **standard Tailwind theme layer**
(`--color-*`, `--spacing`, `--text-*`, `--radius-*`, …) and utility-layer
`--tw-*` initializers are emitted to the global `:root`/`:host`, not scoped —
Tailwind's utility engine depends on them being global. They carry no
host-affecting resets, but if your host also customizes Tailwind's theme,
import your own stylesheet **after** this package's so your values win. (A
build-time gate asserts only Tailwind's own namespaces reach the global scope
and no business token ever does — see `test/styles.test.ts`.) Font tokens
(`--font-sans`, `--font-mono`) are part of that global layer too, but this
package never overrides their *value* there — the global scope keeps
Tailwind's own default font stacks, so your host's own `font-sans`/`font-mono`
utility classes are never repainted with this package's Geist-aware fallback
chain. That chain is scoped to `.ledger-root` only.

Default appearance is **system** (follows the OS via
`prefers-color-scheme`); pass `appearance: "dark"` or `"light"` to force a
variant:

```tsx
<LedgerProvider config={{ baseUrl, appearance: "dark" }}>
```

Re-theme by overriding the CSS custom properties:

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
  `RecentJournals`, `StatusBadge`, the 13 non-chart `*Page` components,
  `LedgerAdmin`, and `Toaster`.
- **`@azex/ledger-react/charts`** — `DashboardPage`, `BalancesPage`,
  `BalanceTrend` (recharts-backed).
- **`@azex/ledger-react/headless`** — the UI-free core both skins build on:
  client, provider, every hook, and the display/decimal helpers.
- **`@azex/ledger-react/heroui`** — the HeroUI v3 skin: same provider,
  `LedgerAdmin` and 13 non-chart pages; **`./heroui/charts`** carries its
  `DashboardPage` / `BalancesPage`.
- **`@azex/ledger-react/wallet`** — the end-user wallet surface
  (`WalletProvider`, `WalletPanel`, …), with `./wallet/heroui` and
  `./wallet/headless` variants.
- **`@azex/ledger-react/server`** (server-only) — `createServerLedgerClient`,
  the `prefetch*` helpers, and `ledgerKeys`. No `"use client"` directive; never
  import from a client component.
- **`@azex/ledger-react/styles.css`** / **`./heroui.css`** — the bundled
  stylesheet for each skin.

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

The workflow re-runs the full verify gate (`codegen:check` against
`docs/openapi.yaml`, typecheck, test, build, artifact assertions), asserts the
tag matches `package.json`, then publishes to the public npm registry.
