"use client";

import {
  MutationCache,
  QueryCache,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { useState, type CSSProperties, type ReactNode } from "react";
import {
  LedgerAppearanceContext,
  LedgerPortalContainerContext,
} from "./context";

/*
 * LedgerShell — the provider chrome shared by every surface's provider
 * (admin <LedgerProvider>, wallet <WalletProvider>): QueryClient ownership +
 * onError wiring, the appearance class, theme overrides, and the
 * `.ledger-root` div floating layers portal into. Client contexts stay in
 * each surface's own provider — the trust domains differ, the chrome doesn't.
 */

export interface LedgerShellConfig {
  /** Reuse the host app's QueryClient. Omitted → the shell creates its own. */
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

export function LedgerShell({
  config,
  children,
}: {
  config: LedgerShellConfig;
  children: ReactNode;
}): React.JSX.Element {
  const { queryClient, onError, theme, appearance = "system" } = config;

  // Root element state (not a ref): floating layers portal into it, so its
  // availability must trigger a re-render once the div mounts.
  const [rootEl, setRootEl] = useState<HTMLDivElement | null>(null);

  // Own QueryClient: stable across renders via lazy useState initializer.
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
    </QueryClientProvider>
  );
}
