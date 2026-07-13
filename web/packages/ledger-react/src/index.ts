// Headless core (client + provider + hooks) — single source of truth in
// headless.ts; the root barrel adds the shadcn skin on top of it.
export * from "./headless";

// Navigation
export { Sidebar } from "./components/sidebar";
export type { SidebarProps } from "./components/sidebar";
export {
  LEDGER_NAV_ITEMS,
  DefaultLink,
} from "./components/nav";
export type { LedgerNavItem, LinkComponent } from "./components/nav";

// Dashboard widgets (chart widgets live on the ./charts subpath to keep
// recharts out of the root barrel)
export { HealthCards } from "./components/dashboard/health-cards";
export { RecentJournals } from "./components/dashboard/recent-journals";
export type { RecentJournalsProps } from "./components/dashboard/recent-journals";

// Shared presentational components used by pages
export { StatusBadge } from "./components/status-badge";

// Page components — host extracts route params and wires routing. Each keeps
// its own "use client" boundary. Pages that link out accept an injectable
// `linkComponent` (default plain <a>); JournalDetailPage takes the journal `id`.
//
// NOTE: the chart-bearing pages (DashboardPage, BalancesPage) live on the
// `./charts` subpath instead — they statically import recharts, so keeping them
// off this barrel keeps recharts out of dist/index.js.
export { JournalsPage } from "./components/pages/JournalsPage";
export type { JournalsPageProps } from "./components/pages/JournalsPage";
export { JournalDetailPage } from "./components/pages/JournalDetailPage";
export type { JournalDetailPageProps } from "./components/pages/JournalDetailPage";
export { ReservationsPage } from "./components/pages/ReservationsPage";
export { DepositsPage } from "./components/pages/DepositsPage";
export { DepositReviewsPage } from "./components/pages/DepositReviewsPage";
export { WithdrawalsPage } from "./components/pages/WithdrawalsPage";
export { ClassificationsPage } from "./components/pages/ClassificationsPage";
export { JournalTypesPage } from "./components/pages/JournalTypesPage";
export { TemplatesPage } from "./components/pages/TemplatesPage";
export { CurrenciesPage } from "./components/pages/CurrenciesPage";
export { ReconciliationPage } from "./components/pages/ReconciliationPage";
export { SnapshotsPage } from "./components/pages/SnapshotsPage";
export { SweepMonitorPage } from "./components/pages/SweepMonitorPage";

// All-in-one admin shell (convenience fallback for hosts that don't wire
// routes). It lazy-loads the chart pages so recharts stays out of index.js.
export { LedgerAdmin } from "./components/LedgerAdmin";

// Toast surface — hosts wiring individual pages mount <Toaster/> once at their
// app root; <LedgerAdmin/> mounts its own. Re-exported so consumers don't need
// a direct sonner dependency.
export { Toaster } from "sonner";
