import { createLedgerClient } from "../src/client/client";

// Separate providers in an individual test opt into the same known identity.
export const testCacheScope = { backend: "test-ledger", identity: "test-admin" };
export const testQueryScope = createLedgerClient({
  baseUrl: "http://ledger.test",
  cacheScope: testCacheScope,
}).cacheScope;
