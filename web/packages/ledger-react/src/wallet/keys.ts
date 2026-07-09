// Single source of truth for the wallet surface's React Query keys — the
// wallet counterpart of `hooks/keys.ts` (separate namespace: separate client,
// separate trust domain, so admin invalidations never collide with wallet
// caches sharing one host QueryClient).
//
// Every key carries `scope` (WalletClientConfig.scope, "" by default): the
// per-user cache identity. Without it, a host reusing one QueryClient across
// an account switch would serve holder A's cached balances to holder B until
// the refetch lands.

export const walletKeys = {
  balances: (scope: string, currencyUid?: string) =>
    ["ledger-wallet", scope, "balances", currencyUid ?? ""] as const,
  transactions: (scope: string, limit: number) =>
    ["ledger-wallet", scope, "transactions", limit] as const,
  holds: (scope: string) => ["ledger-wallet", scope, "holds"] as const,
} as const;

export const walletKeyPrefix = {
  all: ["ledger-wallet"] as const,
  scoped: (scope: string) => ["ledger-wallet", scope] as const,
} as const;
