"use client";

import type { ReactNode } from "react";
import { ReceiptText } from "lucide-react";
import { Card, CardContent } from "../../components/ui/card";
import { EmptyState } from "../../components/empty-state";
import { ErrorState } from "../../components/error-state";
import { LoadMoreBar } from "../../components/pagination-bar";
import { cn, formatSignedAmount, formatUTC } from "../../lib/utils";
import type { WalletTransaction } from "../client";
import { useWalletTransactions } from "../hooks";
import { describeQuoteLine, type UnitLabels } from "../quote";
import { ExactAmount } from "./exact-amount";

/*
 * Wallet transaction list (shadcn skin). Rows speak user language: a label,
 * a signed colored amount, a time, an optional memo, a refund marker, and —
 * when the amount came from a conversion — the priced lines behind it.
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
  // or strip the sign themselves (this is exactly the M1 bug class, just
  // with `direction` standing in for the ledger's own debit/credit side).
  // Server contract: `amount` is an absolute value (`postgres/holder_store.go`'s
  // `net.Abs()`), so `direction === "out"` is the only place the minus sign
  // can legitimately come from.
  const { text, isNegative } = formatSignedAmount(
    tx.direction === "out" ? `-${tx.amount}` : tx.amount,
  );
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
        {/* The quotes, "Breakdown unavailable" when the server withheld
            them (quotes_omitted), or nothing -- see describeQuoteLine. */}
        {quoteLine !== null && (
          <p
            className="whitespace-normal break-words text-xs text-muted-foreground tabular-nums"
            title={quoteLine}
          >
            {quoteLine}
          </p>
        )}
      </div>
      <p
        className={cn(
          "shrink-0 text-sm font-medium tabular-nums",
          isNegative
            ? "text-red-600 dark:text-red-400"
            : "text-emerald-600 dark:text-emerald-400",
        )}
      >
        <ExactAmount
          value={isNegative ? `-${tx.amount}` : tx.amount}
          currencyCode={tx.currency_code}
        >
          {isNegative ? "" : "+"}
          {text}{" "}
          <span className="ml-1 text-muted-foreground font-normal">
            {tx.currency_code}
          </span>
        </ExactAmount>
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
                  unitLabels={unitLabels}
                  quotesOmittedLabel={quotesOmittedLabel}
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
