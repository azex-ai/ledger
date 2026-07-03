import { render } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactElement, ReactNode } from "react";
import { LedgerProvider } from "../../src/provider/provider";
import { server } from "../setup";

export const BASE = "http://ledger.test";

/** Standard success envelope used by the ledger API. */
export function ok<T>(data: T) {
  return HttpResponse.json({ code: 200, message: "ok", data });
}

/** A GET handler returning the success envelope for a fixed body. */
export function getOk<T>(path: string, data: T) {
  return http.get(`${BASE}${path}`, () => ok(data));
}

/** A POST handler returning the success envelope for a fixed body. */
export function postOk<T>(path: string, data: T) {
  return http.post(`${BASE}${path}`, () => ok(data));
}

/**
 * Render a page inside a fresh LedgerProvider with retries disabled (so error
 * states surface immediately). Tests register their own MSW handlers via the
 * shared `server` before calling this.
 */
export function renderPage(ui: ReactElement): ReturnType<typeof render> {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient }}>
      {children}
    </LedgerProvider>
  );
  return render(ui, { wrapper });
}

/** Re-export the MSW server for convenience in page tests. */
export { server };
