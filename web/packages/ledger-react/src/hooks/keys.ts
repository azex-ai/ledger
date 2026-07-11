// Single source of truth for React Query keys.
//
// Both the hooks (`src/hooks/*`) and the server prefetch helpers
// (`src/server/prefetch.ts`) build their query keys here. If a prefetch key
// drifts from the hook key, hydration silently misses (cache key mismatch →
// the client refetches anyway), so the keys MUST be defined exactly once.
//
// Every entry returns the same array shape the corresponding hook used inline
// before this refactor — the VALUES are unchanged.

export const ledgerKeys = {
  // System
  health: () => ["ledger", "health"] as const,
  systemBalances: () => ["ledger", "system-balances"] as const,

  // Journals + entries
  journals: (limit: number) => ["ledger", "journals", limit] as const,
  journal: (id: string) => ["ledger", "journal", id] as const,
  entries: (params: { holder?: number; currency_uid?: string }) =>
    ["ledger", "entries", params] as const,

  // Balances
  balances: (holder: number) => ["ledger", "balances", holder] as const,
  balanceBreakdown: (holder: number, currency: string) =>
    ["ledger", "balances", holder, currency, "breakdown"] as const,
  balancesByCurrency: (holder: number, currency: string) =>
    ["ledger", "balances", holder, currency] as const,

  // Reservations
  reservations: (params: { holder?: number; status?: string; limit?: number }) =>
    ["ledger", "reservations", params] as const,

  // Snapshots
  snapshots: (params: {
    holder?: number;
    currency_uid?: string;
    start?: string;
    end?: string;
  }) => ["ledger", "snapshots", params] as const,

  // Metadata
  classifications: (activeOnly?: boolean) =>
    ["ledger", "classifications", activeOnly] as const,
  journalTypes: (activeOnly?: boolean) =>
    ["ledger", "journal-types", activeOnly] as const,
  templates: (activeOnly?: boolean) =>
    ["ledger", "templates", activeOnly] as const,
  currencies: (activeOnly?: boolean) =>
    ["ledger", "currencies", activeOnly] as const,

  // Bookings (deposit / withdraw / sweep views). The third segment is the
  // classification CODE; the params object carries the resolved numeric
  // `classificationId` the hook computes at runtime.
  bookings: (
    code: string,
    params: {
      holder?: number;
      status?: string;
      classificationUid: string;
      limit?: number;
    },
  ) => ["ledger", "bookings", code, params] as const,

  // Crypto deposit
  depositAddress: (holder: number) =>
    ["ledger", "deposit-address", holder] as const,
  depositReviews: (limit: number) =>
    ["ledger", "deposit-reviews", limit] as const,
} as const;

// Partial-key PREFIXES for namespace-wide invalidation. React Query's
// `invalidateQueries` does a partial (prefix) match, so invalidating
// `["ledger","balances"]` hits every `balances*` query regardless of its
// trailing params. Mutations invalidate by namespace via these prefixes so the
// namespace strings are NOT duplicated as raw literals at the call sites — a
// rename in `ledgerKeys` must not silently break invalidation.
export const ledgerKeyPrefix = {
  all: ["ledger"] as const,
  balances: ["ledger", "balances"] as const,
  systemBalances: ["ledger", "system-balances"] as const,
  journals: ["ledger", "journals"] as const,
  bookings: ["ledger", "bookings"] as const,
  classifications: ["ledger", "classifications"] as const,
  journalTypes: ["ledger", "journal-types"] as const,
  templates: ["ledger", "templates"] as const,
  currencies: ["ledger", "currencies"] as const,
  reservations: ["ledger", "reservations"] as const,
} as const;
