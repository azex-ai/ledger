/*
 * Wallet entry (HeroUI skin) — `@azex/ledger-react/wallet/heroui`.
 *
 * Same wallet surface as ./wallet, rendered with HeroUI v3 components.
 * Host contract matches the admin ./heroui skin: `@heroui/react` (optional
 * peer) installed, host owns the theme, import `@azex/ledger-react/heroui.css`
 * once for layout classes. Shares the headless core with every other skin.
 */

export * from "./wallet-headless";

export { WalletBalanceCard, WalletBalances } from "./wallet/heroui/balance-card";
export type {
  WalletBalanceCardProps,
  WalletBalancesProps,
} from "./wallet/heroui/balance-card";
export { TransactionList } from "./wallet/heroui/transaction-list";
export type { TransactionListProps } from "./wallet/heroui/transaction-list";
export { WalletPanel } from "./wallet/heroui/wallet-panel";
export type { WalletPanelProps } from "./wallet/heroui/wallet-panel";
export { DepositAddressCard } from "./wallet/heroui/deposit-address-card";
