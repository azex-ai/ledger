import { render } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactElement, ReactNode } from "react";
import { LedgerProvider } from "../../src/provider/provider";
import { server } from "../setup";

export const BASE = "http://ledger.test";

/** Standard success envelope used by the ledger API. */
export function ok<T>(data: T) {
  return HttpResponse.json({ code: 200, message: null, data });
}

/** The business envelope for a 400 "missing required query param" error. */
function missingParamError(param: string) {
  return HttpResponse.json(
    { code: 12003, message: { text: `${param} is required` }, data: null },
    { status: 400 },
  );
}

/**
 * A GET handler returning the success envelope for a fixed body.
 *
 * `requiredQuery` mirrors real handlers that hard-require query params (e.g.
 * `server/handler_system.go`'s `/snapshots` — holder/currency_uid/start/end)
 * (J-17, 2026-09-02 web audit): without this, a page requesting a URL that
 * omits a required param got 200 in tests but 400 for real, so a broken
 * request looked passing. Missing params return the same 400 envelope shape
 * a real handler would.
 */
export function getOk<T>(
  path: string,
  data: T,
  opts?: { requiredQuery?: string[] },
) {
  // List endpoints wrap their payload as {list} (api-contract §6); object
  // payloads pass through unchanged.
  const payload = Array.isArray(data) ? { list: data } : data;
  return http.get(`${BASE}${path}`, ({ request }) => {
    const url = new URL(request.url);
    for (const param of opts?.requiredQuery ?? []) {
      const value = url.searchParams.get(param);
      if (value === null || value === "") return missingParamError(param);
    }
    return ok(payload);
  });
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
