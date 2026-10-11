import assert from "node:assert/strict";
import test from "node:test";
import { authenticate, cookieOptions, ledgerCredential, mintSession, sameOrigin, verifySession } from "../lib/auth.ts";

process.env.ADMIN_SESSION_SECRET = "test-session-secret-with-at-least-32-characters";
process.env.ADMIN_ORIGIN = "http://127.0.0.1:43110";
for (const account of ["reader", "writer", "admin", "reviewer"]) {
  process.env[`ADMIN_PASSWORD_${account.toUpperCase()}`] = `password-${account}`;
  process.env[`LEDGER_KEY_${account.toUpperCase()}`] = `server-key-${account}`;
}

test("only authenticated accounts choose a server-held credential", () => {
  for (const account of ["reader", "writer", "admin", "reviewer"]) {
    assert.equal(authenticate(account, `password-${account}`), account);
    const session = verifySession(mintSession(account));
    assert.equal(session.account, account);
    assert.equal(ledgerCredential(session), `server-key-${account}`);
    assert.equal(JSON.stringify(session).includes("server-key"), false);
  }
  assert.equal(authenticate("admin", "password-reader"), null);
  assert.equal(authenticate("admin", { value: "password-admin" }), null);
  assert.equal(authenticate("admin:admin", "password-admin"), null);
});

test("session signature, expiry and login nonce define a real session boundary", () => {
  const now = 1_800_000_000_000;
  const first = mintSession("reader", now);
  const second = mintSession("reader", now);
  assert.notEqual(verifySession(first, now).nonce, verifySession(second, now).nonce);
  assert.equal(verifySession(first, now + 3_600_000), null);
  const [payload, signature] = first.split(".");
  assert.equal(verifySession(`${payload}.${"é".repeat(64)}`, now), null);
  assert.equal(verifySession(`${payload}.${"z".repeat(64)}`, now), null);
  const forged = JSON.parse(Buffer.from(payload, "base64url").toString());
  forged.account = "admin";
  assert.equal(verifySession(`${Buffer.from(JSON.stringify(forged)).toString("base64url")}.${signature}`, now), null);
  assert.equal(verifySession(first + ".extra", now), null);
  assert.equal(verifySession(undefined, now), null);
});

test("mutation Origin is exact, required and independent from spoofable Host", () => {
  const request = (origin) => new Request("http://127.0.0.1:43110/api/session", { method: "POST", headers: origin ? { origin } : {} });
  assert.equal(sameOrigin(request("http://127.0.0.1:43110")), true);
  assert.equal(sameOrigin(request("http://localhost:43110")), false);
  assert.equal(sameOrigin(request("http://127.0.0.1:43111")), false);
  assert.equal(sameOrigin(request("https://attacker.example")), false);
  assert.equal(sameOrigin(request(null)), false);
  assert.equal(cookieOptions().httpOnly, true);
  assert.equal(cookieOptions().sameSite, "strict");
  assert.equal(cookieOptions().secure, false); // explicit HTTP loopback harness
  process.env.ADMIN_ORIGIN = "https://ledger.example";
  assert.equal(cookieOptions().secure, true);
  process.env.ADMIN_ORIGIN = "http://ledger.example";
  assert.throws(() => cookieOptions());
  process.env.ADMIN_ORIGIN = "http://127.0.0.1:43110";
});
