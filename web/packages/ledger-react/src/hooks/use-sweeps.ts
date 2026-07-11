import { useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useClassificationIdByCode } from "./use-classification-id";
import { ledgerKeys } from "./keys";

const SWEEP_CODE = "sweep";

export function useSweepClassificationId(): string {
  return useClassificationIdByCode(SWEEP_CODE);
}

/**
 * Cursor-paginated sweep bookings -- the on-chain custody-collection audit
 * trail (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §4). Every
 * sweep booking is keyed against a per-chain sentinel system holder, not a
 * real user, so there is no per-holder filter here (unlike useDeposits /
 * useWithdrawals). Same paging contract: flatten
 * `data?.pages.flatMap((p) => p.list)`, page via `fetchNextPage`.
 */
export function useSweeps(params: { status?: string } = {}, limit = 20) {
  const client = useLedgerClient();
  const classificationUid = useSweepClassificationId();
  return useInfiniteQuery({
    queryKey: ledgerKeys.bookings(SWEEP_CODE, { ...params, classificationUid, limit }),
    queryFn: ({ pageParam }: { pageParam: string | undefined }) =>
      client.listBookings({
        status: params.status,
        classification_uid: classificationUid,
        cursor: pageParam,
        limit,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: classificationUid !== "",
  });
}
