import { useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { ledgerKeys } from "./keys";

/**
 * Cursor-paginated reservations. Same paging contract as useJournals:
 * flatten `data?.pages.flatMap((p) => p.list)`, page via `fetchNextPage`.
 */
export function useReservations(params: {
  holder?: number;
  status?: string;
  limit?: number;
}) {
  const client = useLedgerClient();
  return useInfiniteQuery({
    queryKey: ledgerKeys.reservations(params),
    queryFn: ({ pageParam }: { pageParam: string | undefined }) =>
      client.listReservations({ ...params, cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
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
