import { NextRequest, NextResponse } from "next/server";
import { ledgerBackend, ledgerCredential, sameOrigin, SESSION_COOKIE, verifySession } from "@/lib/auth";

export const dynamic = "force-dynamic";

async function proxy(request: NextRequest) {
  const session = verifySession(request.cookies.get(SESSION_COOKIE)?.value);
  if (!session) return NextResponse.json({ code: 10101, message: "Sign in to continue", data: null }, { status: 401 });
  const hasBody = request.method !== "GET" && request.method !== "HEAD";
  if (hasBody && !sameOrigin(request)) {
    return NextResponse.json({ code: 10150, message: "Request origin rejected", data: null }, { status: 403 });
  }
  // A fixed configured backend, never a browser-supplied URL or credential.
  const upstream = new URL(ledgerBackend());
  upstream.pathname = request.nextUrl.pathname;
  upstream.search = request.nextUrl.search;
  const headers = new Headers({ Authorization: `Bearer ${ledgerCredential(session)}` });
  // Narrow pass-through: a browser cannot spoof auth, cookies, proxy IPs or host.
  for (const name of ["content-type", "accept", "idempotency-key"]) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  try {
    const result = await fetch(upstream, { method: request.method, headers,
      body: hasBody ? await request.arrayBuffer() : undefined,
      cache: "no-store", redirect: "manual", signal: AbortSignal.timeout(15_000) });
    const responseHeaders = new Headers({ "Cache-Control": "no-store" });
    for (const name of ["content-type", "x-request-id", "retry-after"]) {
      const value = result.headers.get(name);
      if (value) responseHeaders.set(name, value);
    }
    return new NextResponse(result.body, { status: result.status, headers: responseHeaders });
  } catch {
    return NextResponse.json({ code: 15000, message: "Ledger service unavailable", data: null }, { status: 502 });
  }
}

export { proxy as GET, proxy as POST, proxy as PUT, proxy as PATCH, proxy as DELETE };
