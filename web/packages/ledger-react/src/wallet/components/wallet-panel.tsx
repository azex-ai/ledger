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
 * Zero-assembly wallet: balances on top, transaction history below — the
 * wallet-surface counterpart of the admin's <LedgerAdmin/>. Compose the
 * pieces yourself when you need a different layout.
 */
export function WalletPanel({ actions, kindLabels }: WalletPanelProps = {}) {
  return (
    <div className="space-y-6">
      <WalletBalances actions={actions} />
      <section aria-label="Transaction history" className="space-y-3">
        <h2 className="text-sm font-medium text-muted-foreground">Activity</h2>
        <TransactionList kindLabels={kindLabels} />
      </section>
    </div>
  );
}
