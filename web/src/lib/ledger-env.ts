// Single source of truth for the host's ledger backend config. The package
// never reads env — the app (composition root) resolves it here and hands it
// to LedgerProvider (client) / createServerLedgerClient (server).
//
// Security model: the ledger API key exists ONLY on the server. The browser
// talks to the same-origin BFF proxy (app/api/v1/[...path]) with no
// credentials beyond the httpOnly dashboard session cookie; the proxy
// attaches the server-held key. NEXT_PUBLIC_* must never carry the key —
// anything NEXT_PUBLIC is inlined into the public JS bundle.

import type { LedgerClientConfig } from "@azex/ledger-react";

const LOCAL_FALLBACK = "http://localhost:8080";

/**
 * Server-side config: used by RSC prefetch and by the BFF proxy when
 * forwarding browser calls. Requires LEDGER_API_URL_INTERNAL in production
 * (private/VPC URL of ledgerd); falls back to localhost in dev. Throws at
 * request time on force-dynamic pages, so misconfigured prod fails loudly
 * without breaking `next build`.
 */
export function serverLedgerConfig(): LedgerClientConfig {
  const internal = process.env.LEDGER_API_URL_INTERNAL;
  if (!internal && process.env.NODE_ENV === "production") {
    throw new Error("LEDGER_API_URL_INTERNAL must be set in production");
  }
  return {
    baseUrl: internal ?? LOCAL_FALLBACK,
    apiKey: process.env.LEDGER_API_KEY,
  };
}

/**
 * Client-side config for LedgerProvider: same-origin, no key. Every browser
 * request hits the BFF proxy at /api/v1/* on the dashboard's own origin,
 * which authenticates the session cookie and injects the server-held key.
 */
export function clientLedgerConfig(): LedgerClientConfig {
  return { baseUrl: "" };
}
