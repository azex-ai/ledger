// Host-owned authentication, intentionally independent from ledger API scopes.
// No credential or signing material is exported to the client.
import "server-only";
import { createHash, createHmac, randomUUID, timingSafeEqual } from "node:crypto";

export const SESSION_COOKIE = "ledger_acceptance_session";
export const SESSION_SECONDS = 60 * 60;
const accounts = ["reader", "writer", "admin", "reviewer"] as const;
type Account = (typeof accounts)[number];
export type PublicSession = { account: Account; nonce: string; expiresAt: number };

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Missing host configuration: ${name}`);
  return value;
}

export function hostOrigin(): string {
  const url = new URL(required("ADMIN_ORIGIN"));
  if (url.protocol !== "https:" && !(url.protocol === "http:" && url.hostname === "127.0.0.1")) {
    throw new Error("ADMIN_ORIGIN requires HTTPS outside the loopback acceptance host");
  }
  return url.origin;
}

export function sameOrigin(request: Request): boolean {
  return request.headers.get("origin") === hostOrigin();
}

function isAccount(value: unknown): value is Account {
  return typeof value === "string" && accounts.some((account) => account === value);
}

function sign(payload: string): string {
  const secret = required("ADMIN_SESSION_SECRET");
  if (secret.length < 32) throw new Error("ADMIN_SESSION_SECRET must be at least 32 characters");
  return createHmac("sha256", secret).update(payload).digest("hex");
}

export function authenticate(account: unknown, password: unknown): Account | null {
  if (!isAccount(account) || typeof password !== "string") return null;
  const expected = required(`ADMIN_PASSWORD_${account.toUpperCase()}`);
  const hash = (text: string) => createHash("sha256").update(text).digest();
  return timingSafeEqual(hash(password), hash(expected)) ? account : null;
}

export function mintSession(account: Account, now = Date.now()): string {
  const session: PublicSession = { account, nonce: randomUUID(), expiresAt: now + SESSION_SECONDS * 1000 };
  const payload = Buffer.from(JSON.stringify(session)).toString("base64url");
  return `${payload}.${sign(payload)}`;
}

export function verifySession(token: string | undefined, now = Date.now()): PublicSession | null {
  if (!token || token.length > 1024) return null;
  const parts = token.split(".");
  if (parts.length !== 2) return null;
  const [payload, signature] = parts;
  const expected = sign(payload);
  if (!/^[0-9a-f]{64}$/.test(signature) || !timingSafeEqual(Buffer.from(signature, "hex"), Buffer.from(expected, "hex"))) return null;
  try {
    const value: unknown = JSON.parse(Buffer.from(payload, "base64url").toString("utf8"));
    if (typeof value !== "object" || value === null) return null;
    const session = value as Record<string, unknown>;
    if (!isAccount(session.account) || typeof session.nonce !== "string" ||
      !/^[0-9a-f-]{36}$/.test(session.nonce) || typeof session.expiresAt !== "number" ||
      !Number.isSafeInteger(session.expiresAt) || session.expiresAt <= now) return null;
    return { account: session.account, nonce: session.nonce, expiresAt: session.expiresAt };
  } catch { return null; }
}

export function ledgerCredential(session: PublicSession): string {
  return required(`LEDGER_KEY_${session.account.toUpperCase()}`);
}

export function ledgerBackend(): string { return required("LEDGER_API_URL_INTERNAL"); }
export function cacheBackend(): string { return required("LEDGER_CACHE_BACKEND_ID"); }

export function cookieOptions() {
  return { httpOnly: true, sameSite: "strict" as const, path: "/", secure: hostOrigin().startsWith("https:") };
}
