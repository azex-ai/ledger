"use client";

import type { ReactNode } from "react";
import type { UnitLabels } from "../quote";
import { WalletBalances } from "./balance-card";
import { TransactionList } from "./transaction-list";

export interface WalletPanelProps {
  /** Host-provided actions (top-up / cash-out), rendered on each balance card. */
  actions?: ReactNode;
  /** Label overrides by stable `kind` code, forwarded to the transaction list. */
  kindLabels?: Record<string, string>;
  /** Label overrides by unit code for converted rows' quote line, forwarded to the transaction list. */
  unitLabels?: UnitLabels;
  /** Wording for rows whose quotes the server withheld (`quotes_omitted`), forwarded to the transaction list. */
  quotesOmittedLabel?: string;
}

/**
 * Zero-assembly wallet: balances on top, transaction history below. Page
 * logic mirrors the shadcn skin (src/wallet/components/wallet-panel.tsx).
 */
export function WalletPanel({ actions, kindLabels, unitLabels, quotesOmittedLabel }: WalletPanelProps = {}) {
  return (
    <div className="flex flex-col gap-6">
      <WalletBalances actions={actions} />
      <section aria-label="Transaction history" className="flex flex-col gap-3">
        <h2 className="text-muted text-sm font-medium">Activity</h2>
        <TransactionList
          kindLabels={kindLabels}
          unitLabels={unitLabels}
          quotesOmittedLabel={quotesOmittedLabel}
        />
      </section>
    </div>
  );
}
