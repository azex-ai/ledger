"use client";

import { useMemo, type ReactNode } from "react";
import { LedgerShell, type LedgerShellConfig } from "../provider/shell";
import { createWalletClient, type WalletClientConfig } from "./client";
import { WalletClientContext } from "./context";

export interface WalletProviderConfig
  extends WalletClientConfig,
    LedgerShellConfig {}

/**
 * Provider for the end-user wallet surface. Same chrome as the admin
 * LedgerProvider (QueryClient wiring, appearance, `.ledger-root` scope) but
 * a different client behind a different context: wallet components can never
 * accidentally reach the admin API and vice versa.
 */
export function WalletProvider({
  config,
  children,
}: {
  config: WalletProviderConfig;
  children: ReactNode;
}): React.JSX.Element {
  const { baseUrl, getToken, scope, fetch } = config;

  // Stable inputs → stable client identity; `getToken`/`fetch` must be
  // stable references (see WalletClientConfig). A scope change (account
  // switch) rebuilds the client, which re-keys every wallet query.
  const client = useMemo(
    () => createWalletClient({ baseUrl, getToken, scope, fetch }),
    [baseUrl, getToken, scope, fetch],
  );

  return (
    <WalletClientContext.Provider value={client}>
      <LedgerShell config={config}>{children}</LedgerShell>
    </WalletClientContext.Provider>
  );
}
