"use client";

import { LEDGER_NAV_ITEMS, LedgerAdmin, LedgerProvider } from "@azex/ledger-react";
import { useState } from "react";
import type { PublicSession } from "@/lib/auth";
import { Button } from "@/components/ui/button";

// Select the capabilities wired by this host. This is navigation configuration;
// the Go API independently authorizes every request using the session's key.
const unconfiguredSections = new Set(["/deposit-reviews", "/withdrawals", "/sweeps"]);
const navItems = LEDGER_NAV_ITEMS.filter((item) => item.type === "separator" || !unconfiguredSections.has(item.href));

export function Admin({ session, backend }: { session: PublicSession; backend: string }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  async function signOut() {
    setPending(true);
    setError("");
    try {
      const response = await fetch("/api/session", { method: "DELETE" });
      if (!response.ok) throw new Error("Sign out failed. Try again.");
      // Full navigation clears local page/mutation state and the QueryClient.
      window.location.replace("/login");
    } catch { setError("Sign out failed. Try again."); setPending(false); }
  }
  return <>
    <header className="host-session-bar">
      <span>Signed in as <strong>{session.account}</strong></span>
      <Button variant="outline" onClick={signOut} disabled={pending}>{pending ? "Signing out…" : "Sign out"}</Button>
      {error && <p role="alert">{error}</p>}
    </header>
    <LedgerProvider config={{ baseUrl: "", cacheScope: { backend, identity: `${session.account}:${session.nonce}` } }}>
      <LedgerAdmin navItems={navItems} />
    </LedgerProvider>
  </>;
}
