// BFF proxy: the browser talks to this same-origin surface; the ledger API
// key lives only here (server), never in the browser bundle. Requests are
// gated on the dashboard session cookie, then forwarded verbatim to ledgerd
// at LEDGER_API_URL_INTERNAL with the server-held bearer key attached.
//
// Wire paths are unchanged: /api/v1/journals here forwards to
// {internal}/api/v1/journals, so @azex/ledger-react runs with baseUrl: "".
import { NextResponse, type NextRequest } from "next/server";
import {
  SESSION_COOKIE,
  dashboardAuthEnabled,
  dashboardAuthMisconfigured,
  verifySession,
} from "@/lib/dashboard-auth";
import { serverLedgerConfig } from "@/lib/ledger-env";

export const dynamic = "force-dynamic";

// Headers that must not be forwarded in either direction (hop-by-hop, or
// values the proxy itself owns).
const SKIP_REQUEST_HEADERS = new Set([
  "host",
  "connection",
  "authorization",
  "cookie",
  "content-length",
  "accept-encoding",
]);
const SKIP_RESPONSE_HEADERS = new Set([
  "connection",
  "transfer-encoding",
  "content-encoding",
  "content-length",
  "set-cookie",
]);

async function proxy(req: NextRequest) {
  if (dashboardAuthMisconfigured()) {
    return NextResponse.json(
      { code: 10500, message: "dashboard auth is not configured", data: null },
      { status: 503 },
    );
  }
  if (
    dashboardAuthEnabled() &&
    !verifySession(req.cookies.get(SESSION_COOKIE)?.value)
  ) {
    return NextResponse.json(
      { code: 10101, message: "unauthenticated", data: null },
      { status: 401 },
    );
  }

  const { baseUrl, apiKey } = serverLedgerConfig();
  const upstream = new URL(req.nextUrl.pathname + req.nextUrl.search, baseUrl);

  const headers = new Headers();
  req.headers.forEach((value, key) => {
    if (!SKIP_REQUEST_HEADERS.has(key.toLowerCase())) headers.set(key, value);
  });
  if (apiKey) headers.set("Authorization", `Bearer ${apiKey}`);

  const hasBody = req.method !== "GET" && req.method !== "HEAD";
  let res: Response;
  try {
    res = await fetch(upstream, {
      method: req.method,
      headers,
      body: hasBody ? await req.arrayBuffer() : undefined,
      cache: "no-store",
      redirect: "manual",
    });
  } catch {
    // Upstream unreachable — surface as a gateway error in the standard
    // envelope; details stay in server logs, not the browser.
    return NextResponse.json(
      { code: 15000, message: "ledger service unavailable", data: null },
      { status: 502 },
    );
  }

  const responseHeaders = new Headers();
  res.headers.forEach((value, key) => {
    if (!SKIP_RESPONSE_HEADERS.has(key.toLowerCase()))
      responseHeaders.set(key, value);
  });

  return new NextResponse(res.body, {
    status: res.status,
    headers: responseHeaders,
  });
}

export {
  proxy as GET,
  proxy as POST,
  proxy as PUT,
  proxy as PATCH,
  proxy as DELETE,
};
