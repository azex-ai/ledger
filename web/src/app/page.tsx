import { QueryClient, HydrationBoundary, dehydrate } from "@tanstack/react-query";
import { DashboardPage } from "@azex/ledger-react/charts";
import {
  createServerLedgerClient,
  prefetchSystemHealth,
  prefetchSystemBalances,
  prefetchJournals,
} from "@azex/ledger-react/server";
import { NextLink } from "@/components/next-link";
import { serverLedgerConfig } from "@/lib/ledger-env";

// Admin dashboard is per-request data — never statically prerendered. This also
// keeps `next build` from trying to reach the backend at build time.
export const dynamic = "force-dynamic";

export default async function Page() {
  const queryClient = new QueryClient();
  // Resolve config outside the try/catch — a misconfig must fail loudly, not be
  // swallowed as a "best-effort prefetch" failure.
  const client = createServerLedgerClient(serverLedgerConfig());

  // Best-effort server prefetch. If the backend is unreachable, fall through
  // to client-side fetching rather than failing the render.
  try {
    await Promise.all([
      prefetchSystemHealth(queryClient, client),
      prefetchSystemBalances(queryClient, client),
      prefetchJournals(queryClient, client, 10),
    ]);
  } catch (err) {
    console.warn(
      "[ledger] server prefetch failed, falling back to client fetch:",
      err,
    );
  }

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <DashboardPage linkComponent={NextLink} />
    </HydrationBoundary>
  );
}
