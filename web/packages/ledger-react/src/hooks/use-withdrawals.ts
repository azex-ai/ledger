import { useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { useClassificationIdByCode } from "./use-classification-id";
import { ledgerKeys } from "./keys";

const WITHDRAW_CODE = "withdraw";

export function useWithdrawClassificationId(): string {
  return useClassificationIdByCode(WITHDRAW_CODE).uid;
}

/**
 * Cursor-paginated withdrawal bookings. Same paging contract as useJournals:
 * flatten `data?.pages.flatMap((p) => p.list)`, page via `fetchNextPage`.
 *
 * Gated on the "withdraw" classification lookup — its `isLoading`/`isError`
 * are folded into the returned `isLoading`/`isError` so a failed lookup
 * surfaces as an error state instead of a false "no withdrawals" empty state
 * (M2, 2026-08-26 web audit).
 */
export function useWithdrawals(
  params: { holder?: number; status?: string },
  limit = 20,
) {
  const client = useLedgerClient();
  const classification = useClassificationIdByCode(WITHDRAW_CODE);
  const classificationUid = classification.uid;
  const query = useInfiniteQuery({
    queryKey: ledgerKeys.bookings(WITHDRAW_CODE, { ...params, classificationUid, limit }),
    queryFn: ({ pageParam }: { pageParam: string | undefined }) =>
      client.listBookings({
        holder: params.holder,
        status: params.status,
        classification_uid: classificationUid,
        cursor: pageParam,
        limit,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: classificationUid !== "",
  });

  return {
    ...query,
    isLoading: classification.isLoading || query.isLoading,
    isError: classification.isError || query.isError,
    refetch: classification.isError ? classification.refetch : query.refetch,
  };
}

export function useReserveWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: string) => client.transitionBooking(id, { to_status: "reserved" }),
    ["bookings"],
  );
}

/**
 * Approve / reject a withdrawal under review. Approved -> `processing`,
 * rejected -> `failed`.
 */
export function useReviewWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, approved }: { id: string; approved: boolean }) =>
      client.transitionBooking(id, {
        to_status: approved ? "processing" : "failed",
      }),
    ["bookings"],
  );
}

export function useProcessWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, channelRef }: { id: string; channelRef: string }) =>
      client.transitionBooking(id, {
        to_status: "processing",
        channel_ref: channelRef,
      }),
    ["bookings"],
  );
}

export function useConfirmWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: string) => client.transitionBooking(id, { to_status: "confirmed" }),
    ["bookings"],
  );
}

export function useFailWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, reason }: { id: string; reason: string }) =>
      client.transitionBooking(id, {
        to_status: "failed",
        metadata: { reason },
      }),
    ["bookings"],
  );
}

/**
 * Retry a `failed` withdrawal by re-entering the `reserved` state. The
 * classification's lifecycle has an explicit failed -> reserved edge.
 */
export function useRetryWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: string) => client.transitionBooking(id, { to_status: "reserved" }),
    ["bookings"],
  );
}
