import { useState } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { dehydrate, HydrationBoundary, QueryClient } from "@tanstack/react-query";
import { useHealth, useLedgerClient } from "@azex/ledger-react";
import { createServerLedgerClient, prefetchSystemHealth } from "@azex/ledger-react/server";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LedgerProviders } from "../src/components/ledger-providers";
import { getDashboardCacheScope } from "../src/lib/ledger-cache-scope";
import { mintSession, SESSION_COOKIE } from "../src/lib/dashboard-auth";
import { serverLedgerConfig } from "../src/lib/ledger-env";

const session = vi.hoisted(() => ({ token: undefined as string | undefined }));
vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => name === "ledger_dash_session" && session.token
      ? { value: session.token } : undefined,
  }),
}));

beforeEach(() => {
  vi.stubEnv("NODE_ENV", "production");
  vi.stubEnv("DASHBOARD_PASSWORD", "test-operator-password");
  vi.stubEnv("DASHBOARD_SESSION_SECRET", "test-session-signing-secret");
  vi.stubEnv("LEDGER_CACHE_BACKEND_ID", "ledger-production-east");
  vi.stubEnv("LEDGER_API_URL_INTERNAL", "http://private-ledger.test");
  vi.stubEnv("LEDGER_API_KEY", "test-server-api-secret");
  session.token = undefined;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});

function Probe() {
  const health = useHealth();
  const client = useLedgerClient();
  const [edits, setEdits] = useState(0);
  return <>
    <output data-testid="health">{health.data?.status ?? "pending"}</output>
    <output data-testid="scope">{JSON.stringify(client.cacheScope)}</output>
    <button onClick={() => setEdits(edits + 1)}>edits: {edits}</button>
  </>;
}

describe("dashboard request scope", () => {
  test("only a verified public expiry reaches serialized scope", async () => {
    const minted = mintSession();
    session.token = minted.token;
    const expected = {
      backend: "ledger-production-east",
      identity: `operator-session:${minted.token.split(".")[0]}`,
    };
    expect(await getDashboardCacheScope()).toEqual(expected);
    // Layout and each prefetch page resolve the same request without a global cache.
    expect(await getDashboardCacheScope()).toEqual(expected);
    const serialized = JSON.stringify(expected);
    for (const secret of [minted.token, minted.token.split(".")[1],
      "test-server-api-secret", "test-session-signing-secret", "http://private-ledger.test"]) {
      expect(serialized).not.toContain(secret);
    }
    expect(SESSION_COOKIE).toBe("ledger_dash_session");
  });

  test.each(["missing", "forged", "expired"])("%s session is anonymous", async (kind) => {
    const minted = mintSession(kind === "expired" ? Date.now() - 13 * 60 * 60 * 1000 : Date.now());
    session.token = kind === "missing" ? undefined : kind === "forged"
      ? `${minted.token.split(".")[0]}.${"0".repeat(64)}` : minted.token;
    expect(await getDashboardCacheScope()).toEqual({ backend: "ledger-production-east", identity: "anonymous" });
  });

  test("auth-off development is separate from anonymous and defaults only in development", async () => {
    vi.stubEnv("NODE_ENV", "development");
    vi.stubEnv("DASHBOARD_PASSWORD", "");
    vi.stubEnv("LEDGER_CACHE_BACKEND_ID", "");
    expect(await getDashboardCacheScope()).toEqual({ backend: "local-ledger", identity: "development-open" });
    vi.stubEnv("DASHBOARD_PASSWORD", "test-operator-password");
    expect((await getDashboardCacheScope()).identity).toBe("anonymous");
    vi.stubEnv("NODE_ENV", "production");
    await expect(getDashboardCacheScope()).rejects.toThrow("LEDGER_CACHE_BACKEND_ID must be set in production");
    vi.stubEnv("LEDGER_CACHE_BACKEND_ID", "production");
    vi.stubEnv("DASHBOARD_PASSWORD", "");
    expect((await getDashboardCacheScope()).identity).toBe("anonymous");
  });

  test("the existing deterministic token format defines the same session at the same mint time", async () => {
    const now = Date.now();
    const first = mintSession(now);
    const second = mintSession(now);
    expect(second.token).toBe(first.token);
    session.token = first.token;
    const scope = await getDashboardCacheScope();
    session.token = second.token;
    expect(await getDashboardCacheScope()).toEqual(scope);
  });
});

describe("real dashboard provider consumption", () => {
  test("refreshed server props isolate a new login and logout without remounting the host provider", async () => {
    let responseStatus = "operator-A";
    const browserFetch = vi.fn<typeof fetch>(async () => Response.json({
      code: 200, message: null, data: { status: responseStatus },
    }));
    vi.stubGlobal("fetch", browserFetch);
    const now = Date.now();
    session.token = mintSession(now).token;
    const scopeA = await getDashboardCacheScope();
    const { rerender } = render(<LedgerProviders cacheScope={scopeA}><Probe /></LedgerProviders>);
    await waitFor(() => expect(screen.getByTestId("health").textContent).toBe("operator-A"));
    fireEvent.click(screen.getByRole("button", { name: "edits: 0" }));

    // router.refresh supplies new RSC props; the host root/provider identity is
    // retained. Only LedgerProvider's scoped subtree owns the session reset.
    session.token = mintSession(now + 1).token;
    responseStatus = "operator-B";
    const scopeB = await getDashboardCacheScope();
    rerender(<LedgerProviders cacheScope={scopeB}><Probe /></LedgerProviders>);
    expect(screen.getByTestId("health").textContent).toBe("pending");
    expect(screen.getByRole("button", { name: "edits: 0" })).toBeDefined();
    await waitFor(() => expect(screen.getByTestId("health").textContent).toBe("operator-B"));
    expect(scopeB.identity).not.toBe(scopeA.identity);

    session.token = undefined;
    responseStatus = "anonymous-response";
    rerender(<LedgerProviders cacheScope={await getDashboardCacheScope()}><Probe /></LedgerProviders>);
    expect(screen.getByTestId("health").textContent).toBe("pending");
    await waitFor(() => expect(screen.getByTestId("health").textContent).toBe("anonymous-response"));
    expect(screen.getByTestId("scope").textContent).toContain("anonymous");
    expect(browserFetch).toHaveBeenCalledTimes(3);
    expect(browserFetch.mock.calls.every(([url]) => String(url) === "/api/v1/system/health")).toBe(true);
  });

  test("server prefetch survives JSON transport into the real same-origin provider", async () => {
    session.token = mintSession().token;
    const serverScope = await getDashboardCacheScope();
    const serverFetch = vi.fn<typeof fetch>(async () => Response.json({
      code: 200, message: null, data: { status: "SSR-healthy" },
    }));
    const client = createServerLedgerClient({ ...serverLedgerConfig(), cacheScope: serverScope, fetch: serverFetch });
    const serverQC = new QueryClient();
    await prefetchSystemHealth(serverQC, client);
    const serialized = JSON.stringify(dehydrate(serverQC));
    for (const secret of [session.token!, "test-server-api-secret", "http://private-ledger.test"]) {
      expect(serialized).not.toContain(secret);
    }
    // Stale data may refetch normally. Keep it pending to prove the first
    // browser render consumes the transported data, not the fetch response.
    const browserFetch = vi.fn<typeof fetch>(() => new Promise(() => {}));
    vi.stubGlobal("fetch", browserFetch);
    render(<LedgerProviders cacheScope={await getDashboardCacheScope()}>
      <HydrationBoundary state={JSON.parse(serialized)}><Probe /></HydrationBoundary>
    </LedgerProviders>);
    expect(screen.getByTestId("health").textContent).toBe("SSR-healthy");
    expect(screen.getByTestId("scope").textContent).toContain(serverScope.identity);
    expect(serverFetch.mock.calls[0][0]).toBe("http://private-ledger.test/api/v1/system/health");
    await waitFor(() => expect(browserFetch).toHaveBeenCalledTimes(1));
    expect(browserFetch.mock.calls[0][0]).toBe("/api/v1/system/health");
  });
});
