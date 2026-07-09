import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useWalletClient } from "./context";
import { walletKeys } from "./keys";

/**
 * The token-bound holder's balances, one row per currency (or a single
 * currency when `currencyUid` is passed). total = available + pending +
 * locked — safe to assert in UI.
 */
export function useWalletBalance(currencyUid?: string) {
  const client = useWalletClient();
  return useQuery({
    queryKey: walletKeys.balances(client.scope, currencyUid),
    queryFn: () => client.listBalances(currencyUid),
  });
}

/**
 * Cursor-paginated wallet transactions, newest first. Pages carry
 * `{list, next_cursor}` — flatten with `data?.pages.flatMap((p) => p.list)`
 * and drive "Load more" from `hasNextPage`/`fetchNextPage` (same contract as
 * the admin surface's useJournals).
 */
export function useWalletTransactions(limit = 20) {
  const client = useWalletClient();
  return useInfiniteQuery({
    queryKey: walletKeys.transactions(client.scope, limit),
    queryFn: ({ pageParam }: { pageParam: string | undefined }) =>
      client.listTransactions({ cursor: pageParam, limit }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

/** The holder's outstanding holds (locked amounts with expiry). */
export function useWalletHolds() {
  const client = useWalletClient();
  return useQuery({
    queryKey: walletKeys.holds(client.scope),
    queryFn: () => client.listHolds(),
  });
}
