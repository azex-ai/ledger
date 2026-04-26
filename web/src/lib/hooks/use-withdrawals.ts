import { useQuery } from "@tanstack/react-query";
import { useLedgerMutation } from "./use-ledger-mutation";
import * as api from "@/lib/api";

export function useWithdrawals(params: { holder?: number; status?: string }) {
  return useQuery({
    queryKey: ["withdrawals", params],
    queryFn: () => api.listWithdrawals(params),
  });
}

export function useReserveWithdraw() {
  return useLedgerMutation(
    (id: number) => api.reserveWithdraw(id),
    ["withdrawals"],
  );
}

export function useReviewWithdraw() {
  return useLedgerMutation(
    ({ id, approved }: { id: number; approved: boolean }) =>
      api.reviewWithdraw(id, approved),
    ["withdrawals"],
  );
}

export function useProcessWithdraw() {
  return useLedgerMutation(
    ({ id, channelRef }: { id: number; channelRef: string }) =>
      api.processWithdraw(id, channelRef),
    ["withdrawals"],
  );
}

export function useConfirmWithdraw() {
  return useLedgerMutation(
    (id: number) => api.confirmWithdraw(id),
    ["withdrawals"],
  );
}

export function useFailWithdraw() {
  return useLedgerMutation(
    ({ id, reason }: { id: number; reason: string }) =>
      api.failWithdraw(id, reason),
    ["withdrawals"],
  );
}

export function useRetryWithdraw() {
  return useLedgerMutation(
    (id: number) => api.retryWithdraw(id),
    ["withdrawals"],
  );
}
