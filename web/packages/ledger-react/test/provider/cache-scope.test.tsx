import { render, renderHook, screen, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { describe, expect, test, vi } from "vitest";
import { createLedgerClient, type LedgerClientConfig } from "../../src/client/client";
import { LedgerProvider } from "../../src/provider/provider";
import { useLedgerClient } from "../../src/provider/context";
import { useHealth } from "../../src/hooks/use-system";

const BASE = "http://ledger.test";
const shared = { backend: "tenant-east", identity: "session-a" };

function freshClient() {
  return new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
}

function response(status: string) {
  return Response.json({ code: 200, message: null, data: { status } });
}

function Health() {
  const result = useHealth();
  return <div data-testid="health">{result.data?.status ?? "loading"}</div>;
}

describe("admin cache identity", () => {
  test.each(["backend", "apiKey", "fetch"] as const)(
    "default scope changes with observable %s configuration",
    async (field) => {
      const qc = freshClient();
      const fetchA = vi.fn<typeof fetch>(async (url, init) => {
        const changed = String(url).startsWith("http://other.test") ||
          new Headers(init?.headers).get("authorization") === "Bearer secret-b";
        return response(changed ? "B" : "A");
      });
      const configA: LedgerClientConfig = { baseUrl: BASE, apiKey: "secret-a", fetch: fetchA };
      const configB: LedgerClientConfig = {
        ...configA,
        ...(field === "backend" ? { baseUrl: "http://other.test" } :
          field === "apiKey" ? { apiKey: "secret-b" } :
            { fetch: vi.fn<typeof fetch>(async () => response("B")) }),
      };
      const { rerender } = render(
        <LedgerProvider config={{ ...configA, queryClient: qc }}><Health /></LedgerProvider>,
      );
      await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("A"));
      rerender(<LedgerProvider config={{ ...configB, queryClient: qc }}><Health /></LedgerProvider>);
      expect(screen.getByTestId("health")).toHaveTextContent("loading");
      await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("B"));
      expect(qc.getQueryCache().getAll()).toHaveLength(2);
      const serializedKeys = JSON.stringify(qc.getQueryCache().getAll().map((q) => q.queryKey));
      for (const secret of ["secret-a", "secret-b", BASE, "http://other.test"]) {
        expect(serializedKeys).not.toContain(secret);
      }
    },
  );

  test("same-config provider instances remain isolated in a shared QueryClient", async () => {
    const qc = freshClient();
    const fetcher = vi.fn<typeof fetch>(async () => response("ok"));
    const config = { baseUrl: BASE, fetch: fetcher, queryClient: qc };
    render(<><LedgerProvider config={config}><Health /></LedgerProvider>
      <LedgerProvider config={config}><Health /></LedgerProvider></>);
    await waitFor(() => expect(screen.getAllByTestId("health").every((el) => el.textContent === "ok")).toBe(true));
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(qc.getQueryCache().getAll()).toHaveLength(2);
  });

  test("explicit scopes distinguish logical backend even for the same BFF URL", async () => {
    const qc = freshClient();
    let backend = "east";
    const fetcher = vi.fn<typeof fetch>(async () => response(backend));
    const config = { baseUrl: BASE, fetch: fetcher, queryClient: qc };
    const { rerender } = render(<LedgerProvider config={{ ...config, cacheScope: shared }}><Health /></LedgerProvider>);
    await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("east"));
    backend = "west";
    rerender(<LedgerProvider config={{ ...config, cacheScope: { ...shared, backend: "tenant-west" } }}><Health /></LedgerProvider>);
    expect(screen.getByTestId("health")).toHaveTextContent("loading");
    await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("west"));
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  test.each(["scope", "queryClient"] as const)(
    "hidden BFF cookie changes require the host to replace %s",
    async (boundary) => {
      const qc = freshClient();
      let cookieIdentity = "A";
      const fetcher = vi.fn<typeof fetch>(async () => response(cookieIdentity));
      const config = { baseUrl: BASE, fetch: fetcher, queryClient: qc };
      const { rerender } = render(<LedgerProvider config={config}><Health /></LedgerProvider>);
      await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("A"));
      cookieIdentity = "B";
      // Same observable config: SDK cannot detect a cookie change behind BFF.
      rerender(<LedgerProvider config={config}><Health /></LedgerProvider>);
      expect(screen.getByTestId("health")).toHaveTextContent("A");
      expect(fetcher).toHaveBeenCalledTimes(1);
      const nextConfig = boundary === "scope"
        ? { ...config, cacheScope: { ...shared, identity: "session-b" } }
        : { ...config, queryClient: freshClient() };
      rerender(<LedgerProvider key={boundary === "queryClient" ? "new-session" : undefined} config={nextConfig}><Health /></LedgerProvider>);
      expect(screen.getByTestId("health")).toHaveTextContent("loading");
      await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("B"));
      expect(fetcher).toHaveBeenCalledTimes(2);
    },
  );

  test("explicit identity is a host assertion, including when the API key changes", async () => {
    const qc = freshClient();
    const fetcher = vi.fn<typeof fetch>(async (_url, init) =>
      response(new Headers(init?.headers).get("authorization") === "Bearer secret-a" ? "A" : "B"));
    const config = { baseUrl: BASE, fetch: fetcher, queryClient: qc, cacheScope: shared };
    const { rerender } = render(<LedgerProvider config={{ ...config, apiKey: "secret-a" }}><Health /></LedgerProvider>);
    await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("A"));
    rerender(<LedgerProvider config={{ ...config, apiKey: "secret-b" }}><Health /></LedgerProvider>);
    expect(screen.getByTestId("health")).toHaveTextContent("A");
    expect(fetcher).toHaveBeenCalledTimes(1);
    rerender(<LedgerProvider config={{ ...config, apiKey: "secret-b", cacheScope: { ...shared, identity: "session-b" } }}><Health /></LedgerProvider>);
    await waitFor(() => expect(screen.getByTestId("health")).toHaveTextContent("B"));
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  test("equivalent scope objects preserve the client across rerenders", () => {
    let version = 0;
    const { result, rerender } = renderHook(() => useLedgerClient(), {
      wrapper: ({ children }) => <LedgerProvider config={{ baseUrl: BASE, cacheScope: { ...shared }, appearance: version ? "dark" : "light" }}>{children}</LedgerProvider>,
    });
    const first = result.current;
    version += 1;
    rerender();
    expect(result.current).toBe(first);
    expect(result.current.cacheScope).toBe(first.cacheScope);
  });

  test("request config and scope are snapshots, and known API secrets are rejected", async () => {
    const fetcher = vi.fn<typeof fetch>(async () => response("ok"));
    const config = { baseUrl: BASE, apiKey: "server-secret", fetch: fetcher, cacheScope: { ...shared } };
    const client = createLedgerClient(config);
    config.baseUrl = "http://other.test";
    config.apiKey = "other-secret";
    config.cacheScope.identity = "other-user";
    await client.getHealth();
    expect(fetcher.mock.calls[0][0]).toBe(`${BASE}/api/v1/system/health`);
    expect(new Headers(fetcher.mock.calls[0][1]?.headers).get("authorization")).toBe("Bearer server-secret");
    expect(client.cacheScope).toEqual(["shared", shared.backend, shared.identity]);
    expect(Object.isFrozen(client.cacheScope)).toBe(true);
    for (const field of ["backend", "identity"] as const) {
      expect(() => createLedgerClient({ baseUrl: BASE, apiKey: "server-secret", cacheScope: { ...shared, [field]: "server-secret" } }))
        .toThrow("cacheScope must not contain the API key");
      expect(() => createLedgerClient({ baseUrl: BASE, cacheScope: { ...shared, [field]: " " } }))
        .toThrow("cacheScope requires non-empty backend and identity IDs");
    }
  });
});
