// @vitest-environment node
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { NextRequest } from "next/server";
import { POST as login, DELETE as logout } from "../src/app/api/session/route";
import { GET, POST, PUT, PATCH, DELETE } from "../src/app/api/v1/[...path]/route";
import { dashboardRequestOriginAllowed, mintSession, SESSION_COOKIE, verifyPassword, verifySession } from "../src/lib/dashboard-auth";

const origin = "http://localhost:3100";
const upstreamFetch = vi.fn();
beforeEach(() => {
  vi.stubEnv("NODE_ENV", "production");
  vi.stubEnv("DASHBOARD_PASSWORD", "operator-password");
  vi.stubEnv("DASHBOARD_SESSION_SECRET", "test-only-session-secret");
  vi.stubEnv("DASHBOARD_PUBLIC_ORIGIN", "");
  // Empty config is absent, matching an optional deployment variable.
  delete process.env.DASHBOARD_PUBLIC_ORIGIN;
  vi.stubEnv("LEDGER_API_URL_INTERNAL", "http://private-ledger.test");
  vi.stubEnv("LEDGER_API_KEY", "server-secret");
  upstreamFetch.mockReset().mockImplementation(async () => Response.json({ code: 200, data: null }));
  vi.stubGlobal("fetch", upstreamFetch);
});
afterEach(() => { vi.unstubAllEnvs(); vi.unstubAllGlobals(); });

function request(method: string, source: string | null, body?: unknown, path = "/api/v1/journals") {
  const headers = new Headers({ host: new URL(origin).host, cookie: `${SESSION_COOKIE}=${mintSession().token}` });
  if (source !== null) headers.set("origin", source);
  if (body !== undefined) headers.set("content-type", "application/json");
  return new NextRequest(origin + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
}

describe("dashboard browser mutation boundary", () => {
  for (const [method, handler] of [["POST", POST], ["PUT", PUT], ["PATCH", PATCH], ["DELETE", DELETE]] as const) {
    test.each([null, "null", "http://localhost:3101", "https://localhost:3100", "https://attacker.test", "not-an-origin"])(`${method} refuses Origin %s despite a valid cookie`, async (source) => {
      const res = await handler(request(method, source, { amount: "1" }));
      expect(res.status).toBe(403);
      expect(upstreamFetch).not.toHaveBeenCalled();
    });
    test(`${method} forwards same-origin with server-only authorization`, async () => {
      const req = request(method, origin, { amount: "1" });
      const res = await handler(req);
      expect(res.status).toBe(200);
      expect(upstreamFetch).toHaveBeenCalledTimes(1);
      const [url, init] = upstreamFetch.mock.calls[0];
      expect(String(url)).toBe("http://private-ledger.test/api/v1/journals");
      expect(init.headers.get("authorization")).toBe("Bearer server-secret");
      expect(init.headers.has("cookie")).toBe(false);
      expect(await res.text()).not.toContain("server-secret");
    });
  }
  test.each(["127.0.0.1:43111", "[::1]:43111"])("uses real Host %s despite NextRequest loopback URL normalization", async (host) => {
    const publicOrigin = `http://${host}`;
    function aliasRequest(source: string, path = "/api/v1/journals") {
      return new NextRequest(publicOrigin + path, { method: "POST", headers: {
        host, origin: source, cookie: `${SESSION_COOKIE}=${mintSession().token}`,
        "content-type": "application/json",
      }, body: JSON.stringify({ password: "operator-password" }) });
    }
    const req = aliasRequest(publicOrigin);
    expect(new URL(req.url).hostname).toBe("localhost");
    expect((await POST(req)).status).toBe(200);
    expect((await login(aliasRequest(publicOrigin, "/api/session"))).status).toBe(200);
    upstreamFetch.mockClear();
    for (const foreign of ["http://localhost:43111", publicOrigin.replace("43111", "43112"), publicOrigin.replace("http:", "https:")]) {
      expect((await POST(aliasRequest(foreign))).status).toBe(403);
      expect((await login(aliasRequest(foreign, "/api/session"))).status).toBe(403);
    }
    expect(upstreamFetch).not.toHaveBeenCalled();
  });
  test.each([null, "", "user@localhost:3100", "localhost:3100/path", "localhost:3100?x=1", "localhost:3100#x", "localhost:3100,attacker.test", "localhost:bad", "local host:3100", "%6cocalhost:3100", "localhost\\attacker.test"])("missing/malformed Host %s fails closed", async (host) => {
    const req = request("POST", origin, {});
    if (host === null) req.headers.delete("host");
    else req.headers.set("host", host);
    expect((await POST(req)).status).toBe(403);
    expect(upstreamFetch).not.toHaveBeenCalled();
  });
  test("forged forwarded headers cannot approve a cross-port origin", async () => {
    const req = request("POST", "http://localhost:3101", {});
    req.headers.set("x-forwarded-host", "localhost:3101");
    req.headers.set("x-forwarded-proto", "http");
    req.headers.set("forwarded", "host=localhost:3101;proto=http");
    expect((await POST(req)).status).toBe(403);
    expect(upstreamFetch).not.toHaveBeenCalled();
  });
  test("configured public origin supports a reverse proxy without trusting request forwarding headers", async () => {
    vi.stubEnv("DASHBOARD_PUBLIC_ORIGIN", "https://dashboard.example");
    expect((await POST(request("POST", "https://dashboard.example", {}))).status).toBe(200);
    upstreamFetch.mockClear();
    expect((await POST(request("POST", origin, {}))).status).toBe(403);
    expect(upstreamFetch).not.toHaveBeenCalled();
  });
  test.each(["garbage", "null", "https://dashboard.example/path", "https://user:pass@dashboard.example", "https://dashboard.example?x=1"])("invalid public origin %s fails closed", async (configured) => {
    vi.stubEnv("DASHBOARD_PUBLIC_ORIGIN", configured);
    expect((await POST(request("POST", origin, {}))).status).toBe(403);
    expect(upstreamFetch).not.toHaveBeenCalled();
  });
  test("a 64-character non-ASCII signature rejects without throwing or forwarding", async () => {
    const expiresAt = mintSession().token.split(".")[0];
    const token = `${expiresAt}.${"é".repeat(64)}`;
    expect(() => verifySession(token)).not.toThrow();
    expect(verifySession(token)).toBe(false);
    const req = request("GET", null);
    req.cookies.set(SESSION_COOKIE, token);
    expect((await GET(req)).status).toBe(401);
    expect(upstreamFetch).not.toHaveBeenCalled();
  });
  test("GET needs no Origin but still requires a valid session; HEAD is origin-safe", async () => {
    expect((await GET(request("GET", null))).status).toBe(200);
    expect(dashboardRequestOriginAllowed(request("HEAD", null))).toBe(true);
    for (const token of ["", "forged", mintSession(Date.now() - 13 * 3600_000).token]) {
      const req = request("GET", null);
      req.cookies.set(SESSION_COOKIE, token);
      expect((await GET(req)).status).toBe(401);
    }
  });
});

describe("dashboard session", () => {
  test.each([null, "null", "http://localhost:3101"])("login/logout reject Origin %s without setting cookies", async (source) => {
    for (const res of [await login(request("POST", source, { password: "operator-password" }, "/api/session")), await logout(request("DELETE", source, undefined, "/api/session"))]) {
      expect(res.status).toBe(403);
      expect(res.headers.has("set-cookie")).toBe(false);
    }
  });
  test.each([42, null, true, [], {}, { password: 42 }, { password: [] }, { password: {} }, { password: "wrong" }])("invalid password payload %j is rejected rather than throwing", async (body) => {
    const res = await login(request("POST", origin, body, "/api/session"));
    expect(res.status).toBe(401);
    expect(res.headers.has("set-cookie")).toBe(false);
  });
  test("password check rejects non-string runtime values", () => {
    for (const input of [42, null, undefined, {}, []]) expect(verifyPassword(input)).toBe(false);
  });
  test("same-origin login/logout retain secure session cookie behavior", async () => {
    const res = await login(request("POST", origin, { password: "operator-password" }, "/api/session"));
    expect(res.status).toBe(200);
    expect(res.headers.get("set-cookie")).toMatch(/HttpOnly/);
    expect(res.headers.get("set-cookie")).toMatch(/Secure/);
    expect((await logout(request("DELETE", origin, undefined, "/api/session"))).headers.get("set-cookie")).toMatch(/Max-Age=0/);
  });
});
