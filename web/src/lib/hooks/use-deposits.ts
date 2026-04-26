import { useQuery } from "@tanstack/react-query";
import { useLedgerMutation } from "./use-ledger-mutation";
import * as api from "@/lib/api";

export function useDeposits(params: { holder?: number; status?: string }) {
  return useQuery({
    queryKey: ["deposits", params],
    queryFn: () => api.listDeposits(params),
  });
}

export function useConfirmingDeposit() {
  return useLedgerMutation(
    ({ id, channelRef }: { id: number; channelRef: string }) =>
      api.confirmingDeposit(id, channelRef),
    ["deposits"],
  );
}

export function useConfirmDeposit() {
  return useLedgerMutation(
    ({ id, ...body }: { id: number; actual_amount: string; channel_ref: string }) =>
      api.confirmDeposit(id, body),
    ["deposits"],
  );
}

export function useFailDeposit() {
  return useLedgerMutation(
    ({ id, reason }: { id: number; reason: string }) =>
      api.failDeposit(id, reason),
    ["deposits"],
  );
}
