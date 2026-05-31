// Single source of truth for the host's ledger backend config. The package
// never reads env — the app (composition root) resolves it here and hands it
// to LedgerProvider (client) / createServerLedgerClient (server).
//
// Two distinct configs because the server and the browser have different
// fail-loud rules:
//   - server: an internal/private URL is allowed; missing config must fail at
//     request time on the force-dynamic pages (not at `next build`).
//   - client: NEXT_PUBLIC_* are inlined into the browser bundle; missing config
//     must fail loudly in a real prod browser session, but must NOT break the
//     build/prerender (which has no `window`).

import type { LedgerClientConfig } from "@azex/ledger-react";

const LOCAL_FALLBACK = "http://localhost:8080";

/**
 * Server-side config for RSC prefetch. Prefers the internal/private URL, falls
 * back to the public one, then localhost in dev. Throws in production when
 * neither URL is set so a misconfigured prod request fails loudly rather than
 * silently hitting localhost. Runs at request time (force-dynamic pages), so it
 * does not break `next build`.
 */
export function serverLedgerConfig(): LedgerClientConfig {
  const internal = process.env.LEDGER_API_URL_INTERNAL;
  const pub = process.env.NEXT_PUBLIC_API_URL;
  if (!internal && !pub && process.env.NODE_ENV === "production") {
    throw new Error(
      "LEDGER_API_URL_INTERNAL or NEXT_PUBLIC_API_URL must be set in production",
    );
  }
  return {
    baseUrl: internal ?? pub ?? LOCAL_FALLBACK,
    apiKey: process.env.LEDGER_API_KEY,
  };
}

/**
 * Client-side config for LedgerProvider. Throws only on the client in
 * production when NEXT_PUBLIC_API_URL is unset — preserving build/prerender
 * (no `window` on the server) while failing loudly in a real prod browser
 * session, never silently calling localhost.
 */
export function clientLedgerConfig(): LedgerClientConfig {
  if (
    !process.env.NEXT_PUBLIC_API_URL &&
    process.env.NODE_ENV === "production" &&
    typeof window !== "undefined"
  ) {
    throw new Error(
      "NEXT_PUBLIC_API_URL must be set in production builds (no localhost fallback)",
    );
  }
  return {
    baseUrl: process.env.NEXT_PUBLIC_API_URL ?? LOCAL_FALLBACK,
    apiKey: process.env.NEXT_PUBLIC_API_KEY,
  };
}
