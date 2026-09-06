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

import { DepositAddressCard, WalletPanel, WalletProvider } from "@azex/ledger-react/wallet";
import { Toaster } from "@azex/ledger-react";

const baseUrl =
  process.env.NEXT_PUBLIC_LEDGER_API_URL ?? "http://localhost:8090";

// Set only after wiring the backend's real DepositAddressProvider and USDC
// allowlist. Never guess a network or send funds to a fixture address.
const depositNetwork = process.env.NEXT_PUBLIC_LEDGER_DEPOSIT_NETWORK;

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
      <main className="example-wallet">
        <h1>My wallet</h1>
        <WalletPanel
          kindLabels={{ deposit: "Top up" }}
          actions={(balance) => depositNetwork && (!balance || balance.currency_code === "USDC") ? (
            <a href="#deposit" className="example-deposit-link">Add USDC</a>
          ) : null}
        />
        <section id="deposit" aria-label="Add funds">
          {depositNetwork ? (
            <DepositAddressCard network={depositNetwork} assets={["USDC"]} />
          ) : (
            <p className="example-wallet-note">Deposits are currently unavailable.</p>
          )}
        </section>
      </main>
      <Toaster theme="system" position="bottom-right" />
    </WalletProvider>
  );
}
