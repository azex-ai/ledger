import { useMutation, useQueryClient, type MutationFunction } from "@tanstack/react-query";

/**
 * Wrapper around useMutation that automatically invalidates balance-related
 * queries on success. Pass additional module-specific keys to invalidate.
 *
 *   const mutation = useLedgerMutation(api.confirmDeposit, ["deposits"]);
 */
export function useLedgerMutation<TData, TVariables>(
  mutationFn: MutationFunction<TData, TVariables>,
  invalidateKeys: string[],
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn,
    onSuccess: () => {
      for (const key of invalidateKeys) {
        qc.invalidateQueries({ queryKey: [key] });
      }
      qc.invalidateQueries({ queryKey: ["balances"] });
      qc.invalidateQueries({ queryKey: ["system-balances"] });
    },
  });
}
