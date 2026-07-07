"use client";

/*
 * End-user wallet demo (topology A: token handoff).
 *
 * The host backend authenticates its own session, maps user -> holder, and
 * mints a short-lived holder token (/api/session/wallet-token, in-process
 * MintHolderToken). The wallet client calls getToken lazily and re-calls it
 * once whenever a request comes back 401 (token expired) — no refresh logic
 * needed here. The ledger API key never reaches the browser.
 */

import { WalletPanel, WalletProvider } from "@azex/ledger-react/wallet";

const baseUrl =
  process.env.NEXT_PUBLIC_LEDGER_API_URL ?? "http://localhost:8090";

async function fetchWalletToken(): Promise<string> {
  const res = await fetch(`${baseUrl}/api/session/wallet-token`, {
    method: "POST",
  });
  if (!res.ok) throw new Error("wallet session unavailable");
  const body: { token: string } = await res.json();
  return body.token;
}

export default function WalletPage() {
  return (
    <WalletProvider
      config={{ baseUrl: `${baseUrl}/api/v1`, getToken: fetchWalletToken }}
    >
      <main className="mx-auto max-w-3xl p-6">
        <h1 className="mb-6 text-xl font-semibold">My wallet</h1>
        <WalletPanel
          kindLabels={{ deposit_confirm: "Top up" }}
          actions={
            <button
              type="button"
              className="rounded-md border border-border px-3 py-1 text-xs"
              onClick={() => alert("Top-up flow lives in the host product")}
            >
              Top up
            </button>
          }
        />
      </main>
    </WalletProvider>
  );
}
