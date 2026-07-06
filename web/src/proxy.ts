// Session gate for every dashboard page. Unauthenticated visitors are
// redirected to /login. API surfaces are excluded: the BFF proxy
// (/api/v1/*) and the session endpoints return JSON 401s themselves —
// redirecting an XHR to an HTML login page would corrupt client error
// handling.
import { NextResponse, type NextRequest } from "next/server";
import {
  SESSION_COOKIE,
  dashboardAuthEnabled,
  dashboardAuthMisconfigured,
  verifySession,
} from "@/lib/dashboard-auth";

export function proxy(request: NextRequest) {
  if (dashboardAuthMisconfigured()) {
    // Production without DASHBOARD_PASSWORD: refuse to serve rather than
    // silently running an open dashboard.
    return new NextResponse(
      "dashboard auth is not configured (set DASHBOARD_PASSWORD)",
      { status: 503 },
    );
  }
  if (!dashboardAuthEnabled()) {
    return NextResponse.next(); // dev without a password — open
  }
  if (verifySession(request.cookies.get(SESSION_COOKIE)?.value)) {
    return NextResponse.next();
  }
  const loginUrl = new URL("/login", request.url);
  loginUrl.searchParams.set("from", request.nextUrl.pathname);
  return NextResponse.redirect(loginUrl);
}

export const config = {
  // Everything except: the login page, session + BFF API routes (they emit
  // JSON 401s), Next internals, and static files (anything with an extension).
  matcher: ["/((?!login|api/session|api/v1|_next/static|_next/image|.*\\..*).*)"],
};
