export { ApiRequestError, createLedgerClient } from "./client/client";
export type { LedgerClient, LedgerClientConfig } from "./client/client";
export type * from "./client/types";

export { LedgerProvider } from "./provider/provider";
export type { LedgerProviderConfig } from "./provider/provider";
export { useLedgerClient } from "./provider/context";

// Hooks
export { useLedgerMutation } from "./hooks/use-ledger-mutation";
export {
  useJournals,
  useJournal,
  usePostJournal,
  usePostTemplateJournal,
  useReverseJournal,
  useEntries,
} from "./hooks/use-journals";
export { useBalances, useBalancesByCurrency } from "./hooks/use-balances";
export {
  useDepositClassificationId,
  useDeposits,
  useConfirmingDeposit,
  useConfirmDeposit,
  useFailDeposit,
} from "./hooks/use-deposits";
export {
  useWithdrawClassificationId,
  useWithdrawals,
  useReserveWithdraw,
  useReviewWithdraw,
  useProcessWithdraw,
  useConfirmWithdraw,
  useFailWithdraw,
  useRetryWithdraw,
} from "./hooks/use-withdrawals";
export {
  useClassifications,
  useCreateClassification,
  useDeactivateClassification,
  useJournalTypes,
  useCreateJournalType,
  useDeactivateJournalType,
  useTemplates,
  useCreateTemplate,
  useDeactivateTemplate,
  usePreviewTemplate,
  useCurrencies,
  useCreateCurrency,
  useDeactivateCurrency,
} from "./hooks/use-metadata";
export {
  useReservations,
  useSettleReservation,
  useReleaseReservation,
} from "./hooks/use-reservations";
export {
  useHealth,
  useSystemBalances,
  useReconcileGlobal,
  useReconcileAccount,
  useSnapshots,
} from "./hooks/use-system";

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
export { WithdrawalsPage } from "./components/pages/WithdrawalsPage";
export { ClassificationsPage } from "./components/pages/ClassificationsPage";
export { JournalTypesPage } from "./components/pages/JournalTypesPage";
export { TemplatesPage } from "./components/pages/TemplatesPage";
export { CurrenciesPage } from "./components/pages/CurrenciesPage";
export { ReconciliationPage } from "./components/pages/ReconciliationPage";
export { SnapshotsPage } from "./components/pages/SnapshotsPage";

// All-in-one admin shell (convenience fallback for hosts that don't wire
// routes). It lazy-loads the chart pages so recharts stays out of index.js.
export { LedgerAdmin } from "./components/LedgerAdmin";

// Toast surface — hosts wiring individual pages mount <Toaster/> once at their
// app root; <LedgerAdmin/> mounts its own. Re-exported so consumers don't need
// a direct sonner dependency.
export { Toaster } from "sonner";
