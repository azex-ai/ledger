import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { ledgerKeys } from "./keys";

export function useReservations(params: {
  holder?: number;
  status?: string;
  cursor?: string;
  limit?: number;
}) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.reservations(params),
    queryFn: () => client.listReservations(params),
  });
}

export function useSettleReservation() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, actualAmount }: { id: string; actualAmount: string }) =>
      client.settleReservation(id, actualAmount),
    ["reservations"],
  );
}

export function useSettlePartialReservation() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({
      id,
      amount,
      idempotencyKey,
    }: {
      id: string;
      amount: string;
      idempotencyKey: string;
    }) => client.settlePartialReservation(id, amount, idempotencyKey),
    ["reservations"],
  );
}

export function useFinalizeReservationSettlement() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: string) => client.finalizeReservationSettlement(id),
    ["reservations"],
  );
}

export function useReleaseReservation() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: string) => client.releaseReservation(id),
    ["reservations"],
  );
}
