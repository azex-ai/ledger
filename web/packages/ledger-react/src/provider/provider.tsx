"use client";

import {
  QueryCache,
  QueryClient,
  QueryClientProvider,
  MutationCache,
} from "@tanstack/react-query";
import { useMemo, useState, type CSSProperties, type ReactNode } from "react";
import { createLedgerClient, type LedgerClientConfig } from "../client/client";
import { LedgerClientContext } from "./context";

export interface LedgerProviderConfig extends LedgerClientConfig {
  /** Reuse the host app's QueryClient. Omitted → the provider creates its own. */
  queryClient?: QueryClient;
  /**
   * Global error sink. Wired into the package-created QueryClient's query +
   * mutation caches. NOTE: when `queryClient` is injected, the host owns its
   * caches and `onError` is NOT applied — the host wires its own handling.
   */
  onError?: (err: unknown) => void;
  /** CSS custom-property overrides applied inline to the `.ledger-root` div. */
  theme?: Record<string, string>;
}

export function LedgerProvider({
  config,
  children,
}: {
  config: LedgerProviderConfig;
  children: ReactNode;
}): React.JSX.Element {
  const { baseUrl, apiKey, fetch, queryClient, onError, theme } = config;

  // Build the client once per distinct config; memo keyed on the fields that
  // actually shape requests so it is not rebuilt on every render.
  const client = useMemo(
    () => createLedgerClient({ baseUrl, apiKey, fetch }),
    [baseUrl, apiKey, fetch],
  );

  // Own QueryClient: stable across renders via lazy useState initializer.
  // Wire onError into both caches so query + mutation failures surface to it.
  const [ownClient] = useState(
    () =>
      new QueryClient({
        queryCache: new QueryCache({ onError }),
        mutationCache: new MutationCache({ onError }),
      }),
  );
  const activeQueryClient = queryClient ?? ownClient;

  const style = theme ? (theme as CSSProperties) : undefined;

  return (
    <QueryClientProvider client={activeQueryClient}>
      <LedgerClientContext.Provider value={client}>
        <div className="ledger-root" style={style}>
          {children}
        </div>
      </LedgerClientContext.Provider>
    </QueryClientProvider>
  );
}
