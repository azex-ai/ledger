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
  const { baseUrl, apiKey, fetch, cacheScope } = config;
  const sharedScope = cacheScope !== undefined;
  const backend = cacheScope?.backend;
  const identity = cacheScope?.identity;

  // Build the client once per distinct config; memo keyed on the fields that
  // actually shape requests. Stable inputs → stable client identity (Phase 3
  // hooks depend on this). Caveat: `fetch` must be a stable reference — an
  // inline arrow changes identity every render and rebuilds the client. See
  // LedgerClientConfig.fetch.
  const client = useMemo(
    () => createLedgerClient({
      baseUrl, apiKey, fetch,
      cacheScope: sharedScope ? { backend: backend ?? "", identity: identity ?? "" } : undefined,
    }),
    [baseUrl, apiKey, fetch, sharedScope, backend, identity],
  );

  return (
    // Reset local mutation/preview state at an identity boundary as well as
    // changing query keys. An injected QueryClient can safely retain both scopes.
    <LedgerClientContext.Provider key={JSON.stringify(client.cacheScope)} value={client}>
      <LedgerShell config={config}>{children}</LedgerShell>
    </LedgerClientContext.Provider>
  );
}
