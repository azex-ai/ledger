// Dashboard session auth — server-only. The dashboard is gated by a single
// operator password (DASHBOARD_PASSWORD); a successful login mints an
// HMAC-signed, expiring session token stored in an httpOnly cookie. The
// ledger API key never reaches the browser: browser calls go through the
// same-origin BFF proxy (app/api/v1/[...path]) which checks this session and
// attaches the server-held key.
//
// Env:
//   DASHBOARD_PASSWORD        — required in production; unset in dev = auth off
//   DASHBOARD_SESSION_SECRET  — optional HMAC key; derived from the password
//                               when unset (fine for a single-instance dashboard)
import { createHash, createHmac, timingSafeEqual } from "node:crypto";

export const SESSION_COOKIE = "ledger_dash_session";
const SESSION_TTL_MS = 12 * 60 * 60 * 1000; // 12h

export function dashboardAuthEnabled(): boolean {
  return Boolean(process.env.DASHBOARD_PASSWORD);
}

/** True when running a production build without a dashboard password —
 * a misconfiguration we refuse to serve behind (fail loud, not open). */
export function dashboardAuthMisconfigured(): boolean {
  return process.env.NODE_ENV === "production" && !dashboardAuthEnabled();
}

function sessionSecret(): Buffer {
  const explicit = process.env.DASHBOARD_SESSION_SECRET;
  if (explicit) return Buffer.from(explicit, "utf8");
  // Derive from the password so a bare DASHBOARD_PASSWORD deployment works.
  return createHash("sha256")
    .update(`ledger-dash-session:${process.env.DASHBOARD_PASSWORD ?? ""}`)
    .digest();
}

function hmac(payload: string): string {
  return createHmac("sha256", sessionSecret()).update(payload).digest("hex");
}

/** Constant-time password check. */
export function verifyPassword(candidate: string): boolean {
  const expected = process.env.DASHBOARD_PASSWORD;
  if (!expected) return false;
  const a = createHash("sha256").update(candidate).digest();
  const b = createHash("sha256").update(expected).digest();
  return timingSafeEqual(a, b);
}

/** Mint a session token: `<expiresAtMs>.<hmac>`. */
export function mintSession(now = Date.now()): {
  token: string;
  maxAgeSeconds: number;
} {
  const exp = now + SESSION_TTL_MS;
  const payload = String(exp);
  return {
    token: `${payload}.${hmac(payload)}`,
    maxAgeSeconds: Math.floor(SESSION_TTL_MS / 1000),
  };
}

/** Verify a session token's signature and expiry. */
export function verifySession(
  token: string | undefined,
  now = Date.now(),
): boolean {
  if (!token) return false;
  const dot = token.lastIndexOf(".");
  if (dot <= 0) return false;
  const payload = token.slice(0, dot);
  const sig = token.slice(dot + 1);
  const exp = Number(payload);
  if (!Number.isFinite(exp) || exp <= now) return false;
  const expected = hmac(payload);
  if (sig.length !== expected.length) return false;
  return timingSafeEqual(Buffer.from(sig, "utf8"), Buffer.from(expected, "utf8"));
}
