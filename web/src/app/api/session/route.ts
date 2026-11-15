// Dashboard session endpoints: POST = login, DELETE = logout.
// Cookie is httpOnly + sameSite=lax + secure (in production), so the
// session token is never readable from page JavaScript.
import { NextResponse, type NextRequest } from "next/server";
import {
  SESSION_COOKIE,
  dashboardRequestOriginAllowed,
  dashboardAuthEnabled,
  dashboardAuthMisconfigured,
  mintSession,
  verifyPassword,
} from "@/lib/dashboard-auth";

export async function POST(req: NextRequest) {
  if (!dashboardRequestOriginAllowed(req)) {
    return NextResponse.json(
      { code: 10103, message: "same-origin request required", data: null },
      { status: 403 },
    );
  }

  if (dashboardAuthMisconfigured()) {
    return NextResponse.json(
      { code: 10500, message: "dashboard auth is not configured", data: null },
      { status: 503 },
    );
  }
  if (!dashboardAuthEnabled()) {
    // Dev without a password — nothing to log into.
    return NextResponse.json({ code: 200, message: "ok", data: null });
  }

  let password = "";
  try {
    const body: unknown = await req.json();
    if (body && typeof body === "object" && "password" in body && typeof body.password === "string") {
      password = body.password;
    }
  } catch {
    // fall through with empty password → rejected below
  }

  if (!verifyPassword(password)) {
    return NextResponse.json(
      { code: 10101, message: "invalid password", data: null },
      { status: 401 },
    );
  }

  const { token, maxAgeSeconds } = mintSession();
  const res = NextResponse.json({ code: 200, message: "ok", data: null });
  res.cookies.set(SESSION_COOKIE, token, {
    httpOnly: true,
    sameSite: "lax",
    secure: process.env.NODE_ENV === "production",
    path: "/",
    maxAge: maxAgeSeconds,
  });
  return res;
}

export async function DELETE(req: NextRequest) {
  if (!dashboardRequestOriginAllowed(req)) {
    return NextResponse.json(
      { code: 10103, message: "same-origin request required", data: null },
      { status: 403 },
    );
  }

  const res = NextResponse.json({ code: 200, message: "ok", data: null });
  res.cookies.set(SESSION_COOKIE, "", {
    httpOnly: true,
    sameSite: "lax",
    secure: process.env.NODE_ENV === "production",
    path: "/",
    maxAge: 0,
  });
  return res;
}
