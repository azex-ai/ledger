import type { QueryClient } from "@tanstack/react-query";
import type { LedgerClient } from "../client/client";
import { ledgerKeys } from "../hooks/keys";

type JournalsPage = Awaited<ReturnType<LedgerClient["listJournals"]>>;
type EntriesPage = Awaited<ReturnType<LedgerClient["listEntries"]>>;

// Server prefetch helpers. Each takes (queryClient, client, ...params) and
// seeds the QueryClient cache under the SAME key + with the SAME client method
// the matching hook uses, so an RSC host can prefetch on the server and the
// client hook hydrates with zero refetch.
//
// The key MUST come from `ledgerKeys` (shared with the hooks) — an inline key
// here would silently drift and break hydration. The infinite-query helpers
// mirror the hook's `initialPageParam` / `getNextPageParam` so the hydrated
// shape matches `useInfiniteQuery`.
//
// INTENTIONAL OMISSION — bookings (deposit / withdraw): there is deliberately
// no `prefetchBookings`. The `useDeposits` / `useWithdrawals` hooks key on a
// resolved numeric `classificationId`, which itself comes from a separate
// `classifications(true)` query. Prefetching bookings is therefore a two-step
// server flow (resolve the classification id from prefetched classifications,
// THEN list bookings under that id) that the caller must orchestrate — so it's
// left to the host rather than hidden behind a single helper.

/** Mirrors `useJournals(limit)` (infinite query). */
export function prefetchJournals(
  queryClient: QueryClient,
  client: LedgerClient,
  limit = 20,
): Promise<void> {
  return queryClient.prefetchInfiniteQuery({
    queryKey: ledgerKeys.journals(limit),
    queryFn: ({ pageParam }: { pageParam: string }) =>
      client.listJournals({ cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage: JournalsPage) =>
      lastPage.next_cursor || undefined,
  });
}

/** Mirrors `useEntries(params, limit)` (infinite query). */
export function prefetchEntries(
  queryClient: QueryClient,
  client: LedgerClient,
  params: { holder?: number; currency_uid?: string },
  limit = 50,
): Promise<void> {
  return queryClient.prefetchInfiniteQuery({
    queryKey: ledgerKeys.entries(params),
    queryFn: ({ pageParam }: { pageParam: string }) =>
      client.listEntries({ ...params, cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage: EntriesPage) =>
      lastPage.next_cursor || undefined,
  });
}

/** Mirrors `useBalances(holder)`. */
export function prefetchBalances(
  queryClient: QueryClient,
  client: LedgerClient,
  holder: number,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.balances(holder),
    queryFn: () => client.getBalances(holder),
  });
}

/** Mirrors `useHealth()`. */
export function prefetchSystemHealth(
  queryClient: QueryClient,
  client: LedgerClient,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.health(),
    queryFn: () => client.getHealth(),
  });
}

/** Mirrors `useSystemBalances()`. */
export function prefetchSystemBalances(
  queryClient: QueryClient,
  client: LedgerClient,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.systemBalances(),
    queryFn: () => client.getSystemBalances(),
  });
}

/** Mirrors `useReservations(params)`. */
export function prefetchReservations(
  queryClient: QueryClient,
  client: LedgerClient,
  params: { holder?: number; status?: string },
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.reservations(params),
    queryFn: () => client.listReservations(params),
  });
}

/** Mirrors `useClassifications(activeOnly)`. */
export function prefetchClassifications(
  queryClient: QueryClient,
  client: LedgerClient,
  activeOnly?: boolean,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.classifications(activeOnly),
    queryFn: () => client.listClassifications(activeOnly),
  });
}

/** Mirrors `useCurrencies(activeOnly)`. */
export function prefetchCurrencies(
  queryClient: QueryClient,
  client: LedgerClient,
  activeOnly?: boolean,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.currencies(activeOnly),
    queryFn: () => client.listCurrencies(activeOnly),
  });
}

/** Mirrors `useJournalTypes(activeOnly)`. */
export function prefetchJournalTypes(
  queryClient: QueryClient,
  client: LedgerClient,
  activeOnly?: boolean,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.journalTypes(activeOnly),
    queryFn: () => client.listJournalTypes(activeOnly),
  });
}

/** Mirrors `useTemplates(activeOnly)`. */
export function prefetchTemplates(
  queryClient: QueryClient,
  client: LedgerClient,
  activeOnly?: boolean,
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.templates(activeOnly),
    queryFn: () => client.listTemplates(activeOnly),
  });
}

/** Mirrors `useSnapshots(params)`. */
export function prefetchSnapshots(
  queryClient: QueryClient,
  client: LedgerClient,
  params: {
    holder?: number;
    currency_uid?: string;
    start?: string;
    end?: string;
  },
): Promise<void> {
  return queryClient.prefetchQuery({
    queryKey: ledgerKeys.snapshots(params),
    queryFn: () => client.listSnapshots(params),
  });
}
