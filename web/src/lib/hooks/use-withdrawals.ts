import { useQuery } from "@tanstack/react-query";
import { useLedgerMutation } from "./use-ledger-mutation";
import * as api from "@/lib/api";
import type { Booking } from "@/lib/api";

const WITHDRAW_CODE = "withdraw";

function useClassificationIdByCode(code: string): number {
  const { data } = useQuery({
    queryKey: ["classifications", true],
    queryFn: () => api.listClassifications(true),
    staleTime: 5 * 60_000,
  });
  return data?.find((c) => c.code === code)?.id ?? 0;
}

export function useWithdrawClassificationId(): number {
  return useClassificationIdByCode(WITHDRAW_CODE);
}

export function useWithdrawals(params: { holder?: number; status?: string }) {
  const classificationId = useWithdrawClassificationId();
  return useQuery<Booking[]>({
    queryKey: ["bookings", "withdraw", { ...params, classificationId }],
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

export function useReserveWithdraw() {
  return useLedgerMutation(
    (id: number) =>
      api.transitionBooking(id, { to_status: "reserved" }),
    ["bookings"],
  );
}

/**
 * Approve / reject a withdrawal under review. Approved -> `processing`,
 * rejected -> `failed`.
 */
export function useReviewWithdraw() {
  return useLedgerMutation(
    ({ id, approved }: { id: number; approved: boolean }) =>
      api.transitionBooking(id, {
        to_status: approved ? "processing" : "failed",
      }),
    ["bookings"],
  );
}

export function useProcessWithdraw() {
  return useLedgerMutation(
    ({ id, channelRef }: { id: number; channelRef: string }) =>
      api.transitionBooking(id, {
        to_status: "processing",
        channel_ref: channelRef,
      }),
    ["bookings"],
  );
}

export function useConfirmWithdraw() {
  return useLedgerMutation(
    (id: number) =>
      api.transitionBooking(id, { to_status: "confirmed" }),
    ["bookings"],
  );
}

export function useFailWithdraw() {
  return useLedgerMutation(
    ({ id, reason }: { id: number; reason: string }) =>
      api.transitionBooking(id, {
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
  return useLedgerMutation(
    (id: number) =>
      api.transitionBooking(id, { to_status: "reserved" }),
    ["bookings"],
  );
}
