// Single source of truth for the wallet surface's React Query keys — the
// wallet counterpart of `hooks/keys.ts` (separate namespace: separate client,
// separate trust domain, so admin invalidations never collide with wallet
// caches sharing one host QueryClient).

export const walletKeys = {
  balances: (currencyUid?: string) =>
    ["ledger-wallet", "balances", currencyUid ?? ""] as const,
  transactions: (limit: number) =>
    ["ledger-wallet", "transactions", limit] as const,
  holds: () => ["ledger-wallet", "holds"] as const,
} as const;

export const walletKeyPrefix = {
  all: ["ledger-wallet"] as const,
} as const;
