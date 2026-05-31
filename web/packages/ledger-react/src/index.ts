// _smoke-client.tsx is kept as the "use client" directive anchor until Phase 2
// adds the real Provider. The client + types below are framework-agnostic.
export { SMOKE_CLIENT } from "./_smoke-client";

export const VERSION = "0.0.0";

export { ApiRequestError, createLedgerClient } from "./client/client";
export type { LedgerClient, LedgerClientConfig } from "./client/client";
export type * from "./client/types";
