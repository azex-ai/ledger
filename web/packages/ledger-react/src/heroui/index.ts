/*
 * HeroUI skin — `@azex/ledger-react/heroui`.
 *
 * The same admin surface as the package root, rendered with HeroUI v3
 * components instead of the shadcn set. Shares the headless core (client +
 * hooks + LedgerProvider) with every other skin.
 *
 * Host contract:
 *   - `@heroui/react` (optional peer) is installed and the host runs
 *     Tailwind v4 with `@import "@heroui/styles"` — the premise of choosing
 *     this skin.
 *   - Import `@azex/ledger-react/heroui.css` once for the skin's structural
 *     layout classes.
 *   - Wrap the app in <LedgerProvider> (re-exported here for convenience).
 */

// Headless core (client + provider + hooks) — same as the root barrel does,
// so a HeroUI host gets every hook from one import instead of also reaching
// into `@azex/ledger-react/headless` (N10, web audit).
export * from "../headless";

export { LedgerProvider } from "../provider/provider";
export type { LedgerProviderConfig } from "../provider/provider";

// Shell (all-in-one, internal section switching — no host router needed)
export { LedgerAdmin } from "./LedgerAdmin";

// Navigation — the HeroUI Sidebar (desktop only; no mobile drawer, by design —
// see heroui/sidebar.tsx). Exported so a HeroUI host wiring its own routes can
// use the sidebar without pulling in the whole LedgerAdmin shell.
export { Sidebar } from "./sidebar";

// Shared presentational primitives
export { PageHeader, EmptyState, ErrorState, StatusChip, TableSkeleton } from "./shared";

// Pages — host extracts route params and wires routing, same contract as the
// shadcn skin's pages (linkComponent injection; JournalDetailPage takes `id`).
export { JournalsPage } from "./pages/JournalsPage";
export { JournalDetailPage } from "./pages/JournalDetailPage";
export { ReservationsPage } from "./pages/ReservationsPage";
export { DepositsPage } from "./pages/DepositsPage";
export { DepositReviewsPage } from "./pages/DepositReviewsPage";
export { WithdrawalsPage } from "./pages/WithdrawalsPage";
export { ClassificationsPage } from "./pages/ClassificationsPage";
export { JournalTypesPage } from "./pages/JournalTypesPage";
export { TemplatesPage } from "./pages/TemplatesPage";
export { CurrenciesPage } from "./pages/CurrenciesPage";
export { ReconciliationPage } from "./pages/ReconciliationPage";
export { SnapshotsPage } from "./pages/SnapshotsPage";
export { SweepMonitorPage } from "./pages/SweepMonitorPage";

// Chart-bearing pages (DashboardPage, BalancesPage) live on the
// `@azex/ledger-react/heroui/charts` subpath instead — they statically import
// recharts, so keeping them off this barrel keeps recharts out of
// dist/heroui.js (N9, web audit). Mirrors the root `./charts` split.
