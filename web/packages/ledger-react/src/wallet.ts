/*
 * Wallet entry (shadcn skin) — `@azex/ledger-react/wallet`.
 *
 * End-user wallet components on the self-contained shadcn-style skin (import
 * `@azex/ledger-react/styles.css` once, wrap in <WalletProvider>). Read-only
 * by design: top-up / cash-out actions come from the host via the `actions`
 * slot. The HeroUI variant lives at ./wallet/heroui; the UI-free core at
 * ./wallet/headless.
 */

export * from "./wallet-headless";

export { WalletBalanceCard, WalletBalances } from "./wallet/components/balance-card";
export type {
  WalletBalanceCardProps,
  WalletBalancesProps,
} from "./wallet/components/balance-card";
export { TransactionList } from "./wallet/components/transaction-list";
export type { TransactionListProps } from "./wallet/components/transaction-list";
export { WalletPanel } from "./wallet/components/wallet-panel";
export type { WalletPanelProps } from "./wallet/components/wallet-panel";
