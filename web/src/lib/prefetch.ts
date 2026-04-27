import type { QueryClient } from "@tanstack/react-query";
import * as api from "./api";

const ROUTE_PREFETCHERS: Record<string, (qc: QueryClient) => void> = {
  "/": (qc) => {
    qc.prefetchQuery({ queryKey: ["health"], queryFn: api.getHealth });
    qc.prefetchQuery({ queryKey: ["system-balances"], queryFn: api.getSystemBalances });
    qc.prefetchInfiniteQuery({
      queryKey: ["journals", 10],
      queryFn: ({ pageParam }) =>
        api.listJournals({ cursor: pageParam as string, limit: 10 }),
      initialPageParam: "",
    });
  },
  "/journals": (qc) => {
    qc.prefetchInfiniteQuery({
      queryKey: ["journals", 20],
      queryFn: ({ pageParam }) =>
        api.listJournals({ cursor: pageParam as string, limit: 20 }),
      initialPageParam: "",
    });
  },
  "/currencies": (qc) => {
    qc.prefetchQuery({ queryKey: ["currencies"], queryFn: api.listCurrencies });
  },
  "/journal-types": (qc) => {
    qc.prefetchQuery({
      queryKey: ["journal-types", undefined],
      queryFn: () => api.listJournalTypes(),
    });
  },
  "/classifications": (qc) => {
    qc.prefetchQuery({
      queryKey: ["classifications", undefined],
      queryFn: () => api.listClassifications(),
    });
  },
  "/templates": (qc) => {
    qc.prefetchQuery({
      queryKey: ["templates", undefined],
      queryFn: () => api.listTemplates(),
    });
  },
  "/deposits": (qc) => {
    // The deposits screen filters bookings by classification_id at runtime,
    // but the classification list is itself the gate. Warm both so the
    // first paint has data.
    qc.prefetchQuery({
      queryKey: ["classifications", true],
      queryFn: () => api.listClassifications(true),
    });
  },
  "/withdrawals": (qc) => {
    qc.prefetchQuery({
      queryKey: ["classifications", true],
      queryFn: () => api.listClassifications(true),
    });
  },
  "/reservations": (qc) => {
    qc.prefetchQuery({
      queryKey: ["reservations", { status: undefined }],
      queryFn: () => api.listReservations({}),
    });
  },
};

export function prefetchRoute(qc: QueryClient, href: string) {
  ROUTE_PREFETCHERS[href]?.(qc);
}
