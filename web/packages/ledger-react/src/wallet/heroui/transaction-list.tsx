"use client";

import { fiveSkeletonRows } from "../../lib/skeleton-slots";

import type { ReactNode } from "react";
import { Button, Card, Chip, Skeleton, cn } from "@heroui/react";
import { ReceiptText } from "lucide-react";
import { EmptyState, ErrorState } from "../../heroui/shared";
import { formatSignedAmount, formatUTC } from "../../lib/utils";
import type { WalletTransaction } from "../client";
import { useWalletTransactions } from "../hooks";
import { describeQuoteLine, type UnitLabels } from "../quote";

/*
 * Wallet transaction list (HeroUI skin). Page logic mirrors the shadcn skin
 * (src/wallet/components/transaction-list.tsx) — keep in sync. The list is a
 * card of rows (not a Table), so the load-more control is a plain centered
 * Button rather than the table-coupled LoadMoreBar. Converted rows carry the
 * same quote line, worded by the shared describeQuotes presenter.
 */

export interface TransactionListProps {
  /**
   * Label overrides keyed by `kind`, a small, deployment-stable vocabulary
   * (`"deposit" | "withdrawal" | "transfer" | "fee" | "adjustment" |
   * "other"` — see `core.HolderTxKind`, docs/INVARIANTS.md I-44) —
   * the product-side i18n/wording anchor (e.g. `{ deposit: "Top up" }`).
   * Falls back to the library's `kind_label`.
   *
   * `kind` was previously the ledger's internal journal-type UUID (a
   * different, opaque value per deployment) — a `kindLabels` map keyed by
   * that shape can never match anything under this version and must be
   * rewritten to the vocabulary above. See this package's CHANGELOG for the
   * migration note.
   */
  kindLabels?: Record<string, string>;
  /**
   * Label overrides keyed by unit code for the quote line under a converted
   * row (`{ INPUT_TOKEN: "input tokens" }`) — the wording anchor for
   * measured units, as `kindLabels` is for `kind`. Falls back to the code.
   */
  unitLabels?: UnitLabels;
  /**
   * Shown under a row whose quotes the server withheld because the journal
   * also carries other holders' entries (`quotes_omitted`). Defaults to
   * "Breakdown unavailable".
   */
  quotesOmittedLabel?: string;
  /** Full-row custom renderer (escape hatch); the default row otherwise. */
  renderItem?: (tx: WalletTransaction) => ReactNode;
  /** Page size for the cursor pagination. */
  limit?: number;
}

function ListSkeleton() {
  return (
    <Card>
      <Card.Content className="flex flex-col gap-3 py-4">
        {fiveSkeletonRows.map((slot) => (
          <div key={slot.id} className="flex items-center justify-between gap-4">
            <Skeleton className="h-4 w-40 rounded" />
            <Skeleton className="h-4 w-24 rounded" />
          </div>
        ))}
      </Card.Content>
    </Card>
  );
}

function DefaultRow({
  tx,
  label,
  unitLabels,
  quotesOmittedLabel,
}: {
  tx: WalletTransaction;
  label: string;
  unitLabels?: UnitLabels;
  quotesOmittedLabel?: string;
}) {
  const quoteLine = describeQuoteLine(tx, unitLabels, quotesOmittedLabel);
  // J-21 (2026-09-02 web audit): fold `direction` into a signed string and
  // let formatSignedAmount own the sign/color, instead of hardcoding "+"/"-"
  // at the call site — display.ts's contract is that callers never re-derive
  // or strip the sign themselves. Server contract: `amount` is an absolute
  // value (`postgres/holder_store.go`'s `net.Abs()`), so `direction ===
  // "out"` is the only place the minus sign can legitimately come from.
  const { text, isNegative } = formatSignedAmount(
    tx.direction === "out" ? `-${tx.amount}` : tx.amount,
  );
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
        {/* The quotes, "Breakdown unavailable" when the server withheld
            them (quotes_omitted), or nothing -- see describeQuoteLine. */}
        {quoteLine !== null && (
          <p
            className="text-muted whitespace-normal break-words text-xs tabular-nums"
            title={quoteLine}
          >
            {quoteLine}
          </p>
        )}
      </div>
      <p
        className={cn(
          "shrink-0 text-sm font-medium tabular-nums",
          isNegative ? "text-danger" : "text-success",
        )}
      >
        {isNegative ? "" : "+"}
        {text}{" "}
        <span className="text-muted font-normal">{tx.currency_code}</span>
      </p>
    </li>
  );
}

/** The holder's transaction history, newest first, with Load More paging. */
export function TransactionList({
  kindLabels,
  unitLabels,
  quotesOmittedLabel,
  renderItem,
  limit = 20,
}: TransactionListProps = {}) {
  const { data, isLoading, isError, refetch, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useWalletTransactions(limit);
  const transactions = data?.pages.flatMap((p) => p.list) ?? [];

  if (isLoading) return <ListSkeleton />;
  if (isError) {
    return <ErrorState message="Couldn't load your transactions. Please try again." onRetry={refetch} />;
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
                  unitLabels={unitLabels}
                  quotesOmittedLabel={quotesOmittedLabel}
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
