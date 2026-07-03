import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { useClassificationIdByCode } from "./use-classification-id";
import { ledgerKeys } from "./keys";
import type { Booking } from "../client/types";

const WITHDRAW_CODE = "withdraw";

export function useWithdrawClassificationId(): number {
  return useClassificationIdByCode(WITHDRAW_CODE);
}

export function useWithdrawals(params: { holder?: number; status?: string }) {
  const client = useLedgerClient();
  const classificationId = useWithdrawClassificationId();
  return useQuery<Booking[]>({
    queryKey: ledgerKeys.bookings(WITHDRAW_CODE, { ...params, classificationId }),
    queryFn: async () => {
      const page = await client.listBookings({
        holder: params.holder,
        status: params.status,
        classification_id: classificationId,
      });
      return page.list;
    },
    enabled: classificationId > 0,
  });
}

export function useReserveWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: number) => client.transitionBooking(id, { to_status: "reserved" }),
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
    ({ id, approved }: { id: number; approved: boolean }) =>
      client.transitionBooking(id, {
        to_status: approved ? "processing" : "failed",
      }),
    ["bookings"],
  );
}

export function useProcessWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, channelRef }: { id: number; channelRef: string }) =>
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
    (id: number) => client.transitionBooking(id, { to_status: "confirmed" }),
    ["bookings"],
  );
}

export function useFailWithdraw() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, reason }: { id: number; reason: string }) =>
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
    (id: number) => client.transitionBooking(id, { to_status: "reserved" }),
    ["bookings"],
  );
}
