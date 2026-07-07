"use client";

import type { ReactNode } from "react";
import { Button, Card, Chip, Skeleton, cn } from "@heroui/react";
import { ReceiptText } from "lucide-react";
import { EmptyState, ErrorState } from "../../heroui/shared";
import { formatAmount, formatUTC } from "../../lib/utils";
import type { WalletTransaction } from "../client";
import { useWalletTransactions } from "../hooks";

/*
 * Wallet transaction list (HeroUI skin). Page logic mirrors the shadcn skin
 * (src/wallet/components/transaction-list.tsx) — keep in sync. The list is a
 * card of rows (not a Table), so the load-more control is a plain centered
 * Button rather than the table-coupled LoadMoreBar.
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
      <Card.Content className="flex flex-col gap-3 py-4">
        {Array.from({ length: 5 }, (_, i) => (
          <div key={i} className="flex items-center justify-between gap-4">
            <Skeleton className="h-4 w-40 rounded" />
            <Skeleton className="h-4 w-24 rounded" />
          </div>
        ))}
      </Card.Content>
    </Card>
  );
}

function DefaultRow({ tx, label }: { tx: WalletTransaction; label: string }) {
  const isIn = tx.direction === "in";
  return (
    <li className="flex items-center justify-between gap-4 py-3">
      <div className="min-w-0">
        <p className="flex items-center gap-2 text-sm font-medium">
          <span className="truncate">{label}</span>
          {tx.reversal_of_uid !== "" && (
            <Chip size="sm" variant="soft">
              Refund
            </Chip>
          )}
        </p>
        <p className="text-muted truncate text-xs">
          <time dateTime={tx.occurred_at}>{formatUTC(tx.occurred_at)}</time>
          {tx.memo !== "" && <> · {tx.memo}</>}
        </p>
      </div>
      <p
        className={cn(
          "shrink-0 text-sm font-medium tabular-nums",
          isIn ? "text-success" : "text-danger",
        )}
      >
        {isIn ? "+" : "-"}
        {formatAmount(tx.amount)}{" "}
        <span className="text-muted font-normal">{tx.currency_code}</span>
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
        icon={<ReceiptText className="text-muted size-8" aria-hidden />}
        title="No transactions yet"
        description="Your activity will show up here."
      />
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <Card.Content className="py-1">
          <ul className="divide-border divide-y">
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
        </Card.Content>
      </Card>
      {hasNextPage && (
        <div className="flex justify-center">
          <Button
            variant="secondary"
            size="sm"
            isPending={isFetchingNextPage}
            onPress={() => fetchNextPage()}
          >
            {isFetchingNextPage ? "Loading..." : "Load More"}
          </Button>
        </div>
      )}
    </div>
  );
}
