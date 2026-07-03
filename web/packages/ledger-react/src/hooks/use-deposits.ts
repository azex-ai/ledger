import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { useClassificationIdByCode } from "./use-classification-id";
import { ledgerKeys } from "./keys";
import type { Booking } from "../client/types";

const DEPOSIT_CODE = "deposit";

export function useDepositClassificationId(): number {
  return useClassificationIdByCode(DEPOSIT_CODE);
}

export function useDeposits(params: { holder?: number; status?: string }) {
  const client = useLedgerClient();
  const classificationId = useDepositClassificationId();
  return useQuery<Booking[]>({
    queryKey: ledgerKeys.bookings(DEPOSIT_CODE, { ...params, classificationId }),
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

/**
 * Move a deposit from `pending` -> `confirming`. The channel ref is the
 * external transaction reference (tx hash, etc).
 */
export function useConfirmingDeposit() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, channelRef }: { id: number; channelRef: string }) =>
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
      id: number;
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
    ({ id, reason }: { id: number; reason: string }) =>
      client.transitionBooking(id, {
        to_status: "failed",
        metadata: { reason },
      }),
    ["bookings"],
  );
}
