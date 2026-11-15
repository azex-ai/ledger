import { QueryClient, HydrationBoundary, dehydrate } from "@tanstack/react-query";
import { JournalsPage } from "@azex/ledger-react";
import {
  createServerLedgerClient,
  prefetchJournals,
} from "@azex/ledger-react/server";
import { NextLink } from "@/components/next-link";
import { serverLedgerConfig } from "@/lib/ledger-env";
import { getDashboardCacheScope } from "@/lib/ledger-cache-scope";

export const dynamic = "force-dynamic";

export default async function Page() {
  const queryClient = new QueryClient();
  // Resolve config outside the try/catch — a misconfig must fail loudly, not be
  // swallowed as a "best-effort prefetch" failure.
  const cacheScope = await getDashboardCacheScope();
  const client = createServerLedgerClient({ ...serverLedgerConfig(), cacheScope });

  try {
    await prefetchJournals(queryClient, client, 20);
  } catch (err) {
    console.warn(
      "[ledger] server prefetch failed, falling back to client fetch:",
      err,
    );
  }

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <JournalsPage linkComponent={NextLink} />
    </HydrationBoundary>
  );
}
