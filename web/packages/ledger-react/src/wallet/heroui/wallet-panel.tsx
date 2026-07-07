"use client";

import type { ReactNode } from "react";
import { WalletBalances } from "./balance-card";
import { TransactionList } from "./transaction-list";

export interface WalletPanelProps {
  /** Host-provided actions (top-up / cash-out), rendered on each balance card. */
  actions?: ReactNode;
  /** Label overrides by stable `kind` code, forwarded to the transaction list. */
  kindLabels?: Record<string, string>;
}

/**
 * Zero-assembly wallet: balances on top, transaction history below. Page
 * logic mirrors the shadcn skin (src/wallet/components/wallet-panel.tsx).
 */
export function WalletPanel({ actions, kindLabels }: WalletPanelProps = {}) {
  return (
    <div className="flex flex-col gap-6">
      <WalletBalances actions={actions} />
      <section aria-label="Transaction history" className="flex flex-col gap-3">
        <h2 className="text-muted text-sm font-medium">Activity</h2>
        <TransactionList kindLabels={kindLabels} />
      </section>
    </div>
  );
}
