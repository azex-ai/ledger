import { createContext, useContext } from "react";
import type { LedgerClient } from "../client/client";

// Context holding the configured LedgerClient. Null when no provider is
// mounted — useLedgerClient() turns that into a loud error rather than
// silently handing back null.
export const LedgerClientContext = createContext<LedgerClient | null>(null);

export function useLedgerClient(): LedgerClient {
  const client = useContext(LedgerClientContext);
  if (client === null) {
    throw new Error("useLedgerClient must be used within <LedgerProvider>");
  }
  return client;
}

// Resolved appearance from the provider config. Defaults to "system"
// (matching the provider default) so components (e.g. LedgerAdmin's Toaster,
// which passes this straight to sonner's `theme`) behave sensibly even
// outside a provider in tests.
export const LedgerAppearanceContext = createContext<
  "light" | "dark" | "system"
>("system");

export function useLedgerAppearance(): "light" | "dark" | "system" {
  return useContext(LedgerAppearanceContext);
}

// The `.ledger-root` element, used as the portal container for floating
// layers (dialogs, selects, tooltips, sheets). Portalling INTO the root —
// instead of Base UI's default document.body — keeps portalled content
// inside the scope where the package's tokens and preflight apply, so
// overlays render correctly in hosts without any global Tailwind setup.
// Null before the root mounts (or outside a provider): consumers fall back
// to the Base UI default.
export const LedgerPortalContainerContext = createContext<HTMLElement | null>(
  null,
);

export function useLedgerPortalContainer(): HTMLElement | null {
  return useContext(LedgerPortalContainerContext);
}
