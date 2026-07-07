import { useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { useClassificationIdByCode } from "./use-classification-id";
import { ledgerKeys } from "./keys";

const DEPOSIT_CODE = "deposit";

export function useDepositClassificationId(): string {
  return useClassificationIdByCode(DEPOSIT_CODE);
}

/**
 * Cursor-paginated deposit bookings. Pages carry `{list, next_cursor}` —
 * flatten with `data?.pages.flatMap((p) => p.list)` and drive "Load more"
 * from `hasNextPage`/`fetchNextPage` (same contract as useJournals).
 */
export function useDeposits(
  params: { holder?: number; status?: string },
  limit = 20,
) {
  const client = useLedgerClient();
  const classificationUid = useDepositClassificationId();
  return useInfiniteQuery({
    queryKey: ledgerKeys.bookings(DEPOSIT_CODE, { ...params, classificationUid, limit }),
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

/**
 * Move a deposit from `pending` -> `confirming`. The channel ref is the
 * external transaction reference (tx hash, etc).
 */
export function useConfirmingDeposit() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, channelRef }: { id: string; channelRef: string }) =>
      client.transitionBooking(id, {
        to_status: "confirming",
        channel_ref: channelRef,
      }),
    ["bookings"],
  );
}

/**
 * Move a deposit from `confirming` -> `confirmed` with the actual settled
 * amount (which may differ from the expected amount, within tolerance).
 */
export function useConfirmDeposit() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({
      id,
      actual_amount,
      channel_ref,
    }: {
      id: string;
      actual_amount: string;
      channel_ref: string;
    }) =>
      client.transitionBooking(id, {
        to_status: "confirmed",
        amount: actual_amount,
        channel_ref,
      }),
    ["bookings"],
  );
}

export function useFailDeposit() {
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
