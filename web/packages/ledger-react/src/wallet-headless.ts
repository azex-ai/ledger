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
  WalletDepositAddress,
} from "./wallet/client";

export { WalletProvider } from "./wallet/provider";
export type { WalletProviderConfig } from "./wallet/provider";
export { useWalletClient } from "./wallet/context";

export { useWalletBalance, useWalletTransactions, useWalletHolds } from "./wallet/hooks";
export {
  useWalletDepositAddress,
  useEnsureWalletDepositAddress,
} from "./wallet/use-deposit-address";
export { walletKeys, walletKeyPrefix } from "./wallet/keys";

// Display / decimal utilities (J-9, 2026-09-02 web audit) — same rationale
// as the root headless.ts: a host building its own UI on WalletBalance /
// WalletTransaction / WalletHold's NUMERIC(30,18) strings needs these to
// avoid reimplementing financial.md's banding table.
export {
  formatAmount,
  formatSignedAmount,
  formatCompact,
  validateAmount,
  formatUTC,
  formatDateUTC,
  parseUnits,
  formatUnits,
  parseEther,
  formatEther,
  parseGwei,
  formatGwei,
  leadingZeros,
  significantDigits,
  addAmounts,
  subAmounts,
  gtAmount,
  gteAmount,
  isZeroAmount,
  getAddress,
  isAddress,
  isAddressEqual,
  shortenAddress,
  shortenHash,
} from "./lib/utils";
