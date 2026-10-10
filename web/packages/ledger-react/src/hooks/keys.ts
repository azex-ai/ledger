import type { LedgerQueryScope } from "../client/types";

// Shared by hooks and server prefetch. Every key and invalidation prefix must
// include the client's non-secret namespace; no unscoped fallback is provided.

export const ledgerKeys = {
  all: (scope: LedgerQueryScope) => ["ledger", scope] as const,

  // System
  health: (scope: LedgerQueryScope) => ["ledger", scope, "health"] as const,
  systemBalances: (scope: LedgerQueryScope) => ["ledger", scope, "system-balances"] as const,

  // Journals + entries
  journals: (scope: LedgerQueryScope, limit: number) => ["ledger", scope, "journals", limit] as const,
  journal: (scope: LedgerQueryScope, id: string) => ["ledger", scope, "journal", id] as const,
  entries: (scope: LedgerQueryScope, params: { holder?: number; currency_uid?: string }) =>
    ["ledger", scope, "entries", params] as const,

  // Balances
  balances: (scope: LedgerQueryScope, holder: number) => ["ledger", scope, "balances", holder] as const,
  balanceBreakdown: (scope: LedgerQueryScope, holder: number, currency: string) =>
    ["ledger", scope, "balances", holder, currency, "breakdown"] as const,
  balancesByCurrency: (scope: LedgerQueryScope, holder: number, currency: string) =>
    ["ledger", scope, "balances", holder, currency] as const,

  // Reservations
  reservations: (scope: LedgerQueryScope, params: { holder?: number; status?: string; limit?: number }) =>
    ["ledger", scope, "reservations", params] as const,

  // Snapshots
  snapshots: (scope: LedgerQueryScope, params: {
    holder?: number;
    currency_uid?: string;
    start?: string;
    end?: string;
  }) => ["ledger", scope, "snapshots", params] as const,

  // Metadata
  classifications: (scope: LedgerQueryScope, activeOnly?: boolean) =>
    ["ledger", scope, "classifications", activeOnly] as const,
  journalTypes: (scope: LedgerQueryScope, activeOnly?: boolean) =>
    ["ledger", scope, "journal-types", activeOnly] as const,
  templates: (scope: LedgerQueryScope, activeOnly?: boolean) =>
    ["ledger", scope, "templates", activeOnly] as const,
  currencies: (scope: LedgerQueryScope, activeOnly?: boolean) =>
    ["ledger", scope, "currencies", activeOnly] as const,

  // Bookings (deposit / withdraw / sweep views). After the scoped namespace
  // comes the classification code, then params with the resolved UID.
  bookings: (
    scope: LedgerQueryScope,
    code: string,
    params: {
      holder?: number;
      status?: string;
      classificationUid: string;
      limit?: number;
    },
  ) => ["ledger", scope, "bookings", code, params] as const,

  // Crypto deposit
  depositAddress: (scope: LedgerQueryScope, holder: number) =>
    ["ledger", scope, "deposit-address", holder] as const,
  depositReviews: (scope: LedgerQueryScope, limit: number) =>
    ["ledger", scope, "deposit-reviews", limit] as const,
} as const;

// Prefix matching remains within this backend + identity, including optimistic
// review-queue writes, rollbacks, and mutation invalidation.
export const ledgerKeyPrefix = {
  all: ledgerKeys.all,
  balances: (scope: LedgerQueryScope) => ["ledger", scope, "balances"] as const,
  systemBalances: (scope: LedgerQueryScope) => ["ledger", scope, "system-balances"] as const,
  journals: (scope: LedgerQueryScope) => ["ledger", scope, "journals"] as const,
  bookings: (scope: LedgerQueryScope) => ["ledger", scope, "bookings"] as const,
  classifications: (scope: LedgerQueryScope) => ["ledger", scope, "classifications"] as const,
  journalTypes: (scope: LedgerQueryScope) => ["ledger", scope, "journal-types"] as const,
  templates: (scope: LedgerQueryScope) => ["ledger", scope, "templates"] as const,
  currencies: (scope: LedgerQueryScope) => ["ledger", scope, "currencies"] as const,
  reservations: (scope: LedgerQueryScope) => ["ledger", scope, "reservations"] as const,
  depositReviews: (scope: LedgerQueryScope) => ["ledger", scope, "deposit-reviews"] as const,
} as const;
