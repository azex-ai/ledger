"use client";

import {
  QueryCache,
  QueryClient,
  QueryClientProvider,
  MutationCache,
} from "@tanstack/react-query";
import { useMemo, useState, type CSSProperties, type ReactNode } from "react";
import { createLedgerClient, type LedgerClientConfig } from "../client/client";
import {
  LedgerAppearanceContext,
  LedgerClientContext,
  LedgerPortalContainerContext,
} from "./context";

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
  /**
   * Color scheme for everything under the provider. "system" (default)
   * follows the OS via prefers-color-scheme; "dark"/"light" force a variant
   * by class on the `.ledger-root` wrapper. Fine-grained re-theming goes
   * through `theme` overrides.
   */
  appearance?: "light" | "dark" | "system";
}

export function LedgerProvider({
  config,
  children,
}: {
  config: LedgerProviderConfig;
  children: ReactNode;
}): React.JSX.Element {
  const {
    baseUrl,
    apiKey,
    fetch,
    queryClient,
    onError,
    theme,
    appearance = "system",
  } = config;

  // Build the client once per distinct config; memo keyed on the fields that
  // actually shape requests. Stable inputs → stable client identity (Phase 3
  // hooks depend on this). Caveat: `fetch` must be a stable reference — an
  // inline arrow changes identity every render and rebuilds the client. See
  // LedgerClientConfig.fetch.
  const client = useMemo(
    () => createLedgerClient({ baseUrl, apiKey, fetch }),
    [baseUrl, apiKey, fetch],
  );

  // Root element state (not a ref): floating layers portal into it, so its
  // availability must trigger a re-render once the div mounts.
  const [rootEl, setRootEl] = useState<HTMLDivElement | null>(null);

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
        <LedgerAppearanceContext.Provider value={appearance}>
          <LedgerPortalContainerContext.Provider value={rootEl}>
            <div
              ref={setRootEl}
              className={
                appearance === "dark"
                  ? "ledger-root dark"
                  : appearance === "light"
                    ? "ledger-root"
                    : "ledger-root system"
              }
              style={style}
            >
              {children}
            </div>
          </LedgerPortalContainerContext.Provider>
        </LedgerAppearanceContext.Provider>
      </LedgerClientContext.Provider>
    </QueryClientProvider>
  );
}
