"use client";

import type { ReactNode } from "react";
import { ReceiptText } from "lucide-react";
import { Card, CardContent } from "../../components/ui/card";
import { EmptyState } from "../../components/empty-state";
import { ErrorState } from "../../components/error-state";
import { LoadMoreBar } from "../../components/pagination-bar";
import { cn, formatAmount, formatUTC } from "../../lib/utils";
import type { WalletTransaction } from "../client";
import { useWalletTransactions } from "../hooks";

/*
 * Wallet transaction list (shadcn skin). Rows speak user language: a label,
 * a signed colored amount, a time, an optional memo, a refund marker.
 * Mirrored in the HeroUI skin — keep page logic in sync.
 */

export interface TransactionListProps {
  /**
   * Label overrides keyed by the stable `kind` code — the product-side
   * i18n/wording anchor (e.g. `{ deposit_confirm: "Top up" }`). Falls back
   * to the library's `kind_label`.
   */
  kindLabels?: Record<string, string>;
  /** Full-row custom renderer (escape hatch); the default row otherwise. */
  renderItem?: (tx: WalletTransaction) => ReactNode;
  /** Page size for the cursor pagination. */
  limit?: number;
}

function ListSkeleton() {
  return (
    <Card>
      <CardContent className="space-y-3 py-4">
        {Array.from({ length: 5 }, (_, i) => (
          <div key={i} className="flex items-center justify-between gap-4">
            <div className="h-4 w-40 animate-shimmer rounded" />
            <div className="h-4 w-24 animate-shimmer rounded" />
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

function DefaultRow({
  tx,
  label,
}: {
  tx: WalletTransaction;
  label: string;
}) {
  const isIn = tx.direction === "in";
  return (
    <li className="flex items-center justify-between gap-4 py-3">
      <div className="min-w-0">
        <p className="flex items-center gap-2 text-sm font-medium">
          <span className="truncate">{label}</span>
          {tx.reversal_of_uid !== "" && (
            <span className="shrink-0 rounded-full border border-border px-2 py-0.5 text-[10px] uppercase tracking-wide text-muted-foreground">
              Refund
            </span>
          )}
        </p>
        <p className="truncate text-xs text-muted-foreground">
          <time dateTime={tx.occurred_at}>{formatUTC(tx.occurred_at)}</time>
          {tx.memo !== "" && <> · {tx.memo}</>}
        </p>
      </div>
      <p
        className={cn(
          "shrink-0 text-sm font-medium tabular-nums",
          isIn
            ? "text-emerald-600 dark:text-emerald-400"
            : "text-red-600 dark:text-red-400",
        )}
      >
        {isIn ? "+" : "-"}
        {formatAmount(tx.amount)}{" "}
        <span className="text-muted-foreground font-normal">
          {tx.currency_code}
        </span>
      </p>
    </li>
  );
}

/** The holder's transaction history, newest first, with Load More paging. */
export function TransactionList({
  kindLabels,
  renderItem,
  limit = 20,
}: TransactionListProps = {}) {
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useWalletTransactions(limit);
  const transactions = data?.pages.flatMap((p) => p.list) ?? [];

  if (isLoading) return <ListSkeleton />;
  if (isError) {
    return <ErrorState message="Couldn't load your transactions. Please try again." />;
  }
  if (transactions.length === 0) {
    return (
      <EmptyState
        icon={ReceiptText}
        title="No transactions yet"
        description="Your activity will show up here."
      />
    );
  }

  return (
    <div className="space-y-4">
      <Card>
        <CardContent className="py-1">
          <ul className="divide-y divide-border">
            {transactions.map((tx) =>
              renderItem ? (
                <li key={`${tx.uid}-${tx.currency_uid}`}>{renderItem(tx)}</li>
              ) : (
                <DefaultRow
                  key={`${tx.uid}-${tx.currency_uid}`}
                  tx={tx}
                  label={kindLabels?.[tx.kind] ?? tx.kind_label}
                />
              ),
            )}
          </ul>
        </CardContent>
      </Card>
      <LoadMoreBar
        hasNextPage={hasNextPage}
        fetchNextPage={fetchNextPage}
        isFetchingNextPage={isFetchingNextPage}
      />
    </div>
  );
}
