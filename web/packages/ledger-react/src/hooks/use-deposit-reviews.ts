import {
  useInfiniteQuery,
  useMutation,
  useQueryClient,
  type InfiniteData,
} from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import type { Booking, PaginatedResponse } from "../client/types";
import { ledgerKeyPrefix, ledgerKeys } from "./keys";

/**
 * Cursor-paginated deposit review queue (M3 compensating controls) -- the
 * `review` booking status IS the queue, zero ledger effect until approved.
 * Same paging contract as useJournals: flatten
 * `data?.pages.flatMap((p) => p.list)`, page via `fetchNextPage`.
 */
export function useDepositReviews(limit = 20) {
  const client = useLedgerClient();
  return useInfiniteQuery({
    queryKey: ledgerKeys.depositReviews(limit),
    queryFn: ({ pageParam }: { pageParam: string | undefined }) =>
      client.listDepositReviews({ cursor: pageParam, limit }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

type ReviewQueueData = InfiniteData<PaginatedResponse<Booking>>;

/**
 * Optimistically drop `uid` from every cached review-queue page. Approve
 * posts a journal (confirmed) and reject never does (failed) -- both cases
 * the booking leaves the `review` status, so the queue entry disappears
 * either way. This is UI-list-membership only (safe to roll back), NOT a
 * financial state claim -- the booking's real status/settlement still comes
 * from the server via the invalidation below.
 */
function optimisticallyRemoveFromQueue(
  qc: ReturnType<typeof useQueryClient>,
  uid: string,
) {
  const previous = qc.getQueriesData<ReviewQueueData>({
    queryKey: ["ledger", "deposit-reviews"],
  });
  qc.setQueriesData<ReviewQueueData>(
    { queryKey: ["ledger", "deposit-reviews"] },
    (data) =>
      data && {
        ...data,
        pages: data.pages.map((page) => ({
          ...page,
          list: page.list.filter((b) => b.uid !== uid),
        })),
      },
  );
  return previous;
}

function rollback(
  qc: ReturnType<typeof useQueryClient>,
  previous: Array<[readonly unknown[], ReviewQueueData | undefined]>,
) {
  for (const [key, data] of previous) qc.setQueryData(key, data);
}

function invalidateReviewQueue(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ["ledger", "deposit-reviews"] });
  qc.invalidateQueries({ queryKey: ledgerKeyPrefix.bookings });
  qc.invalidateQueries({ queryKey: ledgerKeyPrefix.balances });
  qc.invalidateQueries({ queryKey: ledgerKeyPrefix.systemBalances });
}

export function useApproveDepositReview() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (uid: string) => client.approveDepositReview(uid),
    onMutate: async (uid) => {
      await qc.cancelQueries({ queryKey: ["ledger", "deposit-reviews"] });
      return { previous: optimisticallyRemoveFromQueue(qc, uid) };
    },
    onError: (_err, _uid, context) => {
      if (context) rollback(qc, context.previous);
    },
    onSettled: () => invalidateReviewQueue(qc),
  });
}

export function useRejectDepositReview() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ uid, reason }: { uid: string; reason: string }) =>
      client.rejectDepositReview(uid, reason),
    onMutate: async ({ uid }) => {
      await qc.cancelQueries({ queryKey: ["ledger", "deposit-reviews"] });
      return { previous: optimisticallyRemoveFromQueue(qc, uid) };
    },
    onError: (_err, _vars, context) => {
      if (context) rollback(qc, context.previous);
    },
    onSettled: () => invalidateReviewQueue(qc),
  });
}
