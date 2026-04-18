import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as api from "@/lib/api";

export function useDeposits(params: { holder?: number; status?: string }) {
  return useQuery({
    queryKey: ["deposits", params],
    queryFn: () => api.listDeposits(params),
  });
}

export function useConfirmingDeposit() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, channelRef }: { id: number; channelRef: string }) =>
      api.confirmingDeposit(id, channelRef),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["deposits"] }),
  });
}

export function useConfirmDeposit() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: number; actual_amount: string; channel_ref: string }) =>
      api.confirmDeposit(id, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["deposits"] }),
  });
}

export function useFailDeposit() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, reason }: { id: number; reason: string }) =>
      api.failDeposit(id, reason),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["deposits"] }),
  });
}
