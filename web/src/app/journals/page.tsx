import { QueryClient, HydrationBoundary, dehydrate } from "@tanstack/react-query";
import { JournalsPage } from "@azex/ledger-react";
import {
  createServerLedgerClient,
  prefetchJournals,
} from "@azex/ledger-react/server";
import { NextLink } from "@/components/next-link";

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

  try {
    await prefetchJournals(queryClient, client, 20);
  } catch {
    // Client hook refetches on mount.
  }

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <JournalsPage linkComponent={NextLink} />
    </HydrationBoundary>
  );
}
