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

export { LedgerProvider } from "../provider/provider";
export type { LedgerProviderConfig } from "../provider/provider";

// Shell (all-in-one, internal section switching — no host router needed)
export { LedgerAdmin } from "./LedgerAdmin";

// Shared presentational primitives
export { PageHeader, EmptyState, ErrorState, StatusChip, TableSkeleton } from "./shared";

// Pages — host extracts route params and wires routing, same contract as the
// shadcn skin's pages (linkComponent injection; JournalDetailPage takes `id`).
export { JournalsPage } from "./pages/JournalsPage";
export { JournalDetailPage } from "./pages/JournalDetailPage";
export { BalancesPage } from "./pages/BalancesPage";
export { ReservationsPage } from "./pages/ReservationsPage";
export { DepositsPage } from "./pages/DepositsPage";
export { WithdrawalsPage } from "./pages/WithdrawalsPage";
export { ClassificationsPage } from "./pages/ClassificationsPage";
export { JournalTypesPage } from "./pages/JournalTypesPage";
export { TemplatesPage } from "./pages/TemplatesPage";
export { CurrenciesPage } from "./pages/CurrenciesPage";
export { ReconciliationPage } from "./pages/ReconciliationPage";
export { SnapshotsPage } from "./pages/SnapshotsPage";
export { DashboardPage } from "./pages/DashboardPage";
