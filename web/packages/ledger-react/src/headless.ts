/*
 * Headless entry — the UI-free core shared by every skin.
 *
 * Exports the typed client, wire types, the provider (context + QueryClient
 * wiring; its only DOM output is the `.ledger-root` wrapper div), and the
 * full TanStack Query hook set. No components, no styles: both the shadcn
 * skin (package root) and the HeroUI skin (./heroui) build on exactly this
 * surface, and hosts that bring their own UI can consume it directly.
 */

export { ApiRequestError, createLedgerClient } from "./client/client";
export type { LedgerClient, LedgerClientConfig } from "./client/client";
export type * from "./client/types";

export { LedgerProvider } from "./provider/provider";
export type { LedgerProviderConfig } from "./provider/provider";
export { useLedgerClient, useLedgerAppearance } from "./provider/context";

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
export {
  useBalances,
  useBalancesByCurrency,
  useBalanceBreakdown,
} from "./hooks/use-balances";
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
  useSettlePartialReservation,
  useFinalizeReservationSettlement,
  useReleaseReservation,
} from "./hooks/use-reservations";
export {
  useHealth,
  useSystemBalances,
  useReconcileGlobal,
  useReconcileAccount,
  useSnapshots,
} from "./hooks/use-system";
