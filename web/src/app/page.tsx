import { QueryClient, HydrationBoundary, dehydrate } from "@tanstack/react-query";
import { DashboardPage } from "@azex/ledger-react/charts";
import {
  createServerLedgerClient,
  prefetchSystemHealth,
  prefetchSystemBalances,
  prefetchJournals,
} from "@azex/ledger-react/server";
import { NextLink } from "@/components/next-link";

// Admin dashboard is per-request data — never statically prerendered. This also
// keeps `next build` from trying to reach the backend at build time.
export const dynamic = "force-dynamic";

export default async function Page() {
  const queryClient = new QueryClient();
  const client = createServerLedgerClient({
    baseUrl:
      process.env.LEDGER_API_URL_INTERNAL ??
      process.env.NEXT_PUBLIC_API_URL ??
      "http://localhost:8080",
    apiKey: process.env.LEDGER_API_KEY,
  });

  // Best-effort server prefetch. If the backend is unreachable, fall through
  // to client-side fetching rather than failing the render.
  try {
    await Promise.all([
      prefetchSystemHealth(queryClient, client),
      prefetchSystemBalances(queryClient, client),
      prefetchJournals(queryClient, client, 10),
    ]);
  } catch {
    // Client hooks refetch on mount.
  }

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <DashboardPage linkComponent={NextLink} />
    </HydrationBoundary>
  );
}
