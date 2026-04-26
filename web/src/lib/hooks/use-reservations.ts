import { useQuery } from "@tanstack/react-query";
import { useLedgerMutation } from "./use-ledger-mutation";
import * as api from "@/lib/api";

export function useReservations(params: { holder?: number; status?: string }) {
  return useQuery({
    queryKey: ["reservations", params],
    queryFn: () => api.listReservations(params),
  });
}

export function useSettleReservation() {
  return useLedgerMutation(
    ({ id, actualAmount }: { id: number; actualAmount: string }) =>
      api.settleReservation(id, actualAmount),
    ["reservations"],
  );
}

export function useReleaseReservation() {
  return useLedgerMutation(
    (id: number) => api.releaseReservation(id),
    ["reservations"],
  );
}
