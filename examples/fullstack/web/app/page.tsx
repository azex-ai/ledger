"use client";

import { LEDGER_NAV_ITEMS, LedgerAdmin, LedgerProvider } from "@azex/ledger-react";

const navItems = LEDGER_NAV_ITEMS.filter(
  (item) => item.type === "separator" || item.href !== "/withdrawals",
);

const baseUrl =
  process.env.NEXT_PUBLIC_LEDGER_API_URL ?? "http://localhost:8090";

export default function Home() {
  return (
    <LedgerProvider config={{ baseUrl }}>
      <LedgerAdmin navItems={navItems} />
    </LedgerProvider>
  );
}
