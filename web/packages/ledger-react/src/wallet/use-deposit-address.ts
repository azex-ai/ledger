import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useWalletClient } from "./context";
import { walletKeys } from "./keys";

/**
 * The token-bound holder's already-registered CREATE2 deposit address. 404s
 * (via `ApiRequestError`) if none exists yet -- callers show a "Generate
 * address" CTA on error and drive `useEnsureWalletDepositAddress` from it.
 * `retry: false` because a 404 here is an expected terminal state, not a
 * transient failure worth retrying.
 */
export function useWalletDepositAddress() {
  const client = useWalletClient();
  return useQuery({
    queryKey: walletKeys.depositAddress(client.scope),
    queryFn: () => client.getDepositAddress(),
    retry: false,
  });
}

/**
 * Idempotently issue the token-bound holder's deposit address. Safe to call
 * even if one already exists -- the server always returns the same address.
 */
export function useEnsureWalletDepositAddress() {
  const client = useWalletClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => client.ensureDepositAddress(),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: walletKeys.depositAddress(client.scope) });
    },
  });
}
