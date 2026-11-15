"use client";

import type { ReactNode } from "react";
import { LedgerProvider, Toaster, type LedgerCacheScope } from "@azex/ledger-react";
import { clientLedgerConfig } from "@/lib/ledger-env";

/**
 * The server supplies the same public scope used by RSC prefetch. A session
 * change delivered by router.refresh updates this prop without requiring the
 * root layout to remount; LedgerProvider resets its scoped state and cache.
 */
export function LedgerProviders({ children, cacheScope }: {
  children: ReactNode;
  cacheScope: LedgerCacheScope;
}) {
  return (
    <LedgerProvider config={{ ...clientLedgerConfig(), cacheScope, appearance: "dark" }}>
      {children}
      <Toaster
        theme="dark"
        position="bottom-right"
        toastOptions={{
          style: {
            background: "var(--card)",
            border: "1px solid var(--border)",
            color: "var(--card-foreground)",
          },
        }}
      />
    </LedgerProvider>
  );
}
