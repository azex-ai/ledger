"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export default function Login() {
  const [account, setAccount] = useState("");
  const [password, setPassword] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  async function signIn(event: FormEvent) {
    event.preventDefault();
    if (pending) return;
    setPending(true); setError("");
    try {
      const response = await fetch("/api/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ account, password }) });
      if (!response.ok) { setError(response.status === 401 ? "Incorrect account or password." : "Sign in is unavailable. Try again."); return; }
      window.location.replace("/");
    } catch { setError("Could not reach the server. Try again."); }
    finally { setPending(false); }
  }
  return <main className="host-login">
    <form onSubmit={signIn} className="host-login-form">
      <h1>Ledger administration</h1>
      <div><Label htmlFor="account">Account</Label><Input id="account" autoComplete="username" value={account} onChange={(event) => setAccount(event.target.value)} required /></div>
      <div><Label htmlFor="password">Password</Label><Input id="password" type="password" autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} required /></div>
      {error && <p role="alert">{error}</p>}
      <Button type="submit" disabled={pending}>{pending ? "Signing in…" : "Sign in"}</Button>
    </form>
  </main>;
}
