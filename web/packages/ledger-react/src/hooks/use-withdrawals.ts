import { useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { useClassificationIdByCode } from "./use-classification-id";
import { ledgerKeys } from "./keys";

const WITHDRAW_CODE = "withdraw";

export function useWithdrawClassificationId(): string {
  return useClassificationIdByCode(WITHDRAW_CODE);
}

/**
 * Cursor-paginated withdrawal bookings. Same paging contract as useJournals:
 * flatten `data?.pages.flatMap((p) => p.list)`, page via `fetchNextPage`.
 */
export function useWithdrawals(
  params: { holder?: number; status?: string },
  limit = 20,
) {
  const client = useLedgerClient();
  const classificationUid = useWithdrawClassificationId();
  return useInfiniteQuery({
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
