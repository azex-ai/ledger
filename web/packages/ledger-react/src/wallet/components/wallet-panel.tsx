"use client";

import type { ReactNode } from "react";
import { WalletBalances, type WalletBalancesProps } from "./balance-card";
import { TransactionList, type TransactionListProps } from "./transaction-list";

export interface WalletPanelProps extends WalletBalancesProps, TransactionListProps {
  /** Replace a complete region. Omit for the default; null hides the region. */
  slots?: {
    balances?: ReactNode;
    transactions?: ReactNode;
  };
}

/**
 * Zero-assembly wallet: balances on top, transaction history below — the
 * wallet-surface counterpart of the admin's <LedgerAdmin/>. Compose the
 * pieces yourself for a different layout, or replace regions through slots.
 */
export function WalletPanel({
  actions,
  kindLabels,
  renderItem,
  limit,
  slots,
}: WalletPanelProps = {}) {
  return (
    <div className="space-y-6">
      {slots?.balances !== undefined ? slots.balances : <WalletBalances actions={actions} />}
      {slots?.transactions !== undefined ? slots.transactions : (
        <section aria-label="Transaction history" className="space-y-3">
          <h2 className="text-sm font-medium text-muted-foreground">Activity</h2>
          <TransactionList kindLabels={kindLabels} renderItem={renderItem} limit={limit} />
        </section>
      )}
    </div>
  );
}
