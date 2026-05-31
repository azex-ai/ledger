"use client";

import type { ReactNode } from "react";
import { LedgerProvider, Toaster } from "@azex/ledger-react";
import { clientLedgerConfig } from "@/lib/ledger-env";

/**
 * Client provider boundary. Resolves the client-side ledger config (which fails
 * loudly in a real prod browser session if NEXT_PUBLIC_API_URL is unset),
 * mounts the LedgerProvider context once, and renders the toast surface.
 */
export function LedgerProviders({ children }: { children: ReactNode }) {
  return (
    <LedgerProvider config={clientLedgerConfig()}>
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
