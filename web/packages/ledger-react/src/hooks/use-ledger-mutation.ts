import {
  useMutation,
  useQueryClient,
  type MutationFunction,
} from "@tanstack/react-query";

/**
 * Wrapper around useMutation that automatically invalidates balance-related
 * queries on success. Callers pass bare key segments (e.g. ["journals"]); the
 * wrapper namespaces each under the package's "ledger" prefix to match the
 * query keys used by the hooks in this package.
 *
 *   const mutation = useLedgerMutation((body) => client.postJournal(body), ["journals"]);
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
        qc.invalidateQueries({ queryKey: ["ledger", key] });
      }
      qc.invalidateQueries({ queryKey: ["ledger", "balances"] });
      qc.invalidateQueries({ queryKey: ["ledger", "system-balances"] });
    },
  });
}
