import "server-only";

import { cookies } from "next/headers";
import type { LedgerCacheScope } from "@azex/ledger-react";
import {
  dashboardAuthEnabled,
  SESSION_COOKIE,
  verifySession,
} from "./dashboard-auth";

/** Resolve at request time, after entering Next's dynamic cookie boundary. */
export async function getDashboardCacheScope(): Promise<LedgerCacheScope> {
  const cookieStore = await cookies();
  const configuredBackend = process.env.LEDGER_CACHE_BACKEND_ID?.trim();
  if (!configuredBackend && process.env.NODE_ENV === "production") {
    throw new Error("LEDGER_CACHE_BACKEND_ID must be set in production");
  }
  const backend = configuredBackend || "local-ledger";

  if (!dashboardAuthEnabled()) {
    return {
      backend,
      identity: process.env.NODE_ENV === "production" ? "anonymous" : "development-open",
    };
  }

  const token = cookieStore.get(SESSION_COOKIE)?.value;
  if (!token || !verifySession(token)) return { backend, identity: "anonymous" };

  // Only the verified public expiry is serialized. Never expose the cookie,
  // signature, signing key, API key, or private backend URL as a cache key.
  // The existing token format has no session nonce: identical minted tokens
  // (including logins in the same millisecond) represent the same session.
  const expiresAtMs = Number(token.slice(0, token.lastIndexOf(".")));
  return { backend, identity: `operator-session:${expiresAtMs}` };
}
