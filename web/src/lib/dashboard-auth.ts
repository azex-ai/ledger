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
export function verifyPassword(candidate: unknown): boolean {
  const expected = process.env.DASHBOARD_PASSWORD;
  if (!expected || typeof candidate !== "string") return false;
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
  // Restrict to the ASCII hex encoding we mint before comparing UTF-8 bytes.
  // Equal JS string lengths alone do not guarantee equal Buffer lengths.
  if (sig.length !== expected.length || !/^[0-9a-f]{64}$/.test(sig)) return false;
  return timingSafeEqual(Buffer.from(sig, "utf8"), Buffer.from(expected, "utf8"));
}

/** Browser mutations must come from this exact origin (including port).
 * Reverse proxies may set DASHBOARD_PUBLIC_ORIGIN to the externally visible
 * origin. Never infer trust from client-supplied Forwarded / X-Forwarded-*.
 * NextRequest normalizes loopback URL hostnames to localhost. Use the browser
 * request Host authority with the framework URL scheme when no public origin
 * is configured; a proxy that rewrites either must configure the public origin.
 * Missing/malformed Host or Origin fail closed, including login/logout.
 */
export function dashboardRequestOriginAllowed(req: Pick<Request, "method" | "url" | "headers">): boolean {
  if (req.method === "GET" || req.method === "HEAD") return true;
  const origin = req.headers.get("origin");
  if (!origin || origin === "null") return false;
  try {
    const source = new URL(origin);
    const configured = process.env.DASHBOARD_PUBLIC_ORIGIN;
    let target: URL;
    if (configured !== undefined) {
      target = new URL(configured);
    } else {
      const host = req.headers.get("host");
      // Host is an authority, never a URL, credentials or a forwarded list.
      if (!host || /[\s/\\@?#%,]/.test(host)) return false;
      target = new URL(`${new URL(req.url).protocol}//${host}`);
    }
    if (!["http:", "https:"].includes(target.protocol)) return false;
    if (target.username || target.password || target.pathname !== "/" || target.search || target.hash) return false;
    return source.origin === origin && source.origin === target.origin;
  } catch {
    return false;
  }
}
