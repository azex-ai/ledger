"use client";

import { LedgerAdmin, LedgerProvider } from "@azex/ledger-react/heroui";

const baseUrl =
  process.env.NEXT_PUBLIC_LEDGER_API_URL ?? "http://localhost:8090";

export default function Home() {
  return (
    <LedgerProvider config={{ baseUrl }}>
      <LedgerAdmin />
    </LedgerProvider>
  );
}
