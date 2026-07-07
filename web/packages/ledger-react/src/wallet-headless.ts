/*
 * Wallet headless entry — `@azex/ledger-react/wallet/headless`.
 *
 * The UI-free core of the end-user wallet surface: typed client (holder
 * token / BFF auth via `getToken` callback), provider, and hooks. Both
 * wallet skins build on exactly this; hosts that bring their own UI consume
 * it directly. Read-only by construction — write flows (top-up, cash-out)
 * belong to the host product.
 */

export {
  ApiRequestError, // shared error type (same envelope contract)
} from "./client/client";

export { createWalletClient } from "./wallet/client";
export type {
  WalletClient,
  WalletClientConfig,
  WalletBalance,
  WalletTransaction,
  WalletTransactionsPage,
  WalletHold,
} from "./wallet/client";

export { WalletProvider } from "./wallet/provider";
export type { WalletProviderConfig } from "./wallet/provider";
export { useWalletClient } from "./wallet/context";

export { useWalletBalance, useWalletTransactions, useWalletHolds } from "./wallet/hooks";
export { walletKeys, walletKeyPrefix } from "./wallet/keys";
