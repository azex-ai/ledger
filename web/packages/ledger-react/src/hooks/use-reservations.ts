import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";

export function useReservations(params: { holder?: number; status?: string }) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "reservations", params],
    queryFn: () => client.listReservations(params),
  });
}

export function useSettleReservation() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, actualAmount }: { id: number; actualAmount: string }) =>
      client.settleReservation(id, actualAmount),
    ["reservations"],
  );
}

export function useReleaseReservation() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: number) => client.releaseReservation(id),
    ["reservations"],
  );
}
