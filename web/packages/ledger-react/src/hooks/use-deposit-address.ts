import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { useLedgerMutation } from "./use-ledger-mutation";
import { ledgerKeys } from "./keys";

/**
 * Look up a holder's already-registered CREATE2 deposit address. 404s (via
 * `ApiRequestError`) if the holder has none yet -- callers show a "Generate
 * address" CTA on error and drive `useEnsureDepositAddress` from it. `retry:
 * false` because a 404 here is an expected terminal state, not a transient
 * failure worth retrying.
 */
export function useDepositAddress(holder: number) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.depositAddress(holder),
    queryFn: () => client.getDepositAddress(holder),
    enabled: holder !== 0,
    retry: false,
  });
}

/**
 * Idempotently issue a holder's deposit address. Safe to call even if one
 * already exists -- the server always returns the same address for the same
 * holder.
 */
export function useEnsureDepositAddress() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (holder: number) => client.ensureDepositAddress(holder),
    ["deposit-address"],
  );
}
