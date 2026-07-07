"use client";

import { useMemo, type ReactNode } from "react";
import { createLedgerClient, type LedgerClientConfig } from "../client/client";
import { LedgerClientContext } from "./context";
import { LedgerShell, type LedgerShellConfig } from "./shell";

export interface LedgerProviderConfig
  extends LedgerClientConfig,
    LedgerShellConfig {}

export function LedgerProvider({
  config,
  children,
}: {
  config: LedgerProviderConfig;
  children: ReactNode;
}): React.JSX.Element {
  const { baseUrl, apiKey, fetch } = config;

  // Build the client once per distinct config; memo keyed on the fields that
  // actually shape requests. Stable inputs → stable client identity (Phase 3
  // hooks depend on this). Caveat: `fetch` must be a stable reference — an
  // inline arrow changes identity every render and rebuilds the client. See
  // LedgerClientConfig.fetch.
  const client = useMemo(
    () => createLedgerClient({ baseUrl, apiKey, fetch }),
    [baseUrl, apiKey, fetch],
  );

  return (
    <LedgerClientContext.Provider value={client}>
      <LedgerShell config={config}>{children}</LedgerShell>
    </LedgerClientContext.Provider>
  );
}
