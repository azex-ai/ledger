import { useQuery } from "@tanstack/react-query";
import { useLedgerMutation } from "./use-ledger-mutation";
import * as api from "@/lib/api";
import type { Booking } from "@/lib/api";

const DEPOSIT_CODE = "deposit";

/**
 * Resolve the classification ID for a given code (e.g. "deposit", "withdraw").
 *
 * The classification list is small and stable, so it's cached for a long time.
 * Returns 0 (falsy) until classifications have loaded.
 */
function useClassificationIdByCode(code: string): number {
  const { data } = useQuery({
    queryKey: ["classifications", true],
    queryFn: () => api.listClassifications(true),
    staleTime: 5 * 60_000,
  });
  return data?.find((c) => c.code === code)?.id ?? 0;
}

export function useDepositClassificationId(): number {
  return useClassificationIdByCode(DEPOSIT_CODE);
}

export function useDeposits(params: { holder?: number; status?: string }) {
  const classificationId = useDepositClassificationId();
  return useQuery<Booking[]>({
    queryKey: ["bookings", "deposit", { ...params, classificationId }],
    queryFn: async () => {
      const page = await api.listBookings({
        holder: params.holder,
        status: params.status,
        classification_id: classificationId,
      });
      return page.data;
    },
    enabled: classificationId > 0,
  });
}

/**
 * Move a deposit from `pending` -> `confirming`. The channel ref is the
 * external transaction reference (tx hash, etc).
 */
export function useConfirmingDeposit() {
  return useLedgerMutation(
    ({ id, channelRef }: { id: number; channelRef: string }) =>
      api.transitionBooking(id, {
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
      api.transitionBooking(id, {
        to_status: "confirmed",
        amount: actual_amount,
        channel_ref,
      }),
    ["bookings"],
  );
}

export function useFailDeposit() {
  return useLedgerMutation(
    ({ id, reason }: { id: number; reason: string }) =>
      api.transitionBooking(id, {
        to_status: "failed",
        metadata: { reason },
      }),
    ["bookings"],
  );
}
