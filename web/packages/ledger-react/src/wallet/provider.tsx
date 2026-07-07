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
  const { baseUrl, getToken, fetch } = config;

  // Stable inputs → stable client identity; `getToken`/`fetch` must be
  // stable references (see WalletClientConfig).
  const client = useMemo(
    () => createWalletClient({ baseUrl, getToken, fetch }),
    [baseUrl, getToken, fetch],
  );

  return (
    <WalletClientContext.Provider value={client}>
      <LedgerShell config={config}>{children}</LedgerShell>
    </WalletClientContext.Provider>
  );
}
