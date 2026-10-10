// Compile-time public cache contract (included in typecheck, not a runtime test).
import { createLedgerClient } from "../../src/client/client";
import type { LedgerCacheScope } from "../../src/client/types";
import { ledgerKeys } from "../../src/hooks/keys";

const scope: LedgerCacheScope = { backend: "tenant-east", identity: "admin-session" };
const client = createLedgerClient({ baseUrl: "", cacheScope: scope });
ledgerKeys.balances(client.cacheScope, 42);
ledgerKeys.all(client.cacheScope);
// @ts-expect-error Unscoped manual cache keys must be migrated.
ledgerKeys.balances(42);
// @ts-expect-error Both backend and identity are required for explicit sharing.
createLedgerClient({ baseUrl: "", cacheScope: { identity: "admin-session" } });
// @ts-expect-error A client cannot be relabeled to another scope.
client.cacheScope = ["instance", "other"];
// @ts-expect-error The resolved scope tuple is immutable.
client.cacheScope[1] = "other";
