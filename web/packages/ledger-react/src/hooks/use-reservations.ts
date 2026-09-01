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
  // Payload-keyed idempotency (caller supplies the key via variables, same as
  // useSettlePartialReservation). settle's server-side receipt matches on the
  // amount (postgres/reserver_store.go:457,361), so the auto-minted key held
  // across a retry after a client timeout would ErrConflict the moment the
  // operator corrects the amount — a dead end. keyed on the amount, a corrected
  // amount mints a fresh key (M2, web audit; see use-idempotency-key.ts).
  return useLedgerMutation(
    ({
      id,
      actualAmount,
      idempotencyKey,
    }: {
      id: string;
      actualAmount: string;
      idempotencyKey: string;
    }) => client.settleReservation(id, actualAmount, idempotencyKey),
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
    (id: string, idempotencyKey) => client.finalizeReservationSettlement(id, idempotencyKey),
    ["reservations"],
  );
}

export function useReleaseReservation() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (id: string, idempotencyKey) => client.releaseReservation(id, idempotencyKey),
    ["reservations"],
  );
}
