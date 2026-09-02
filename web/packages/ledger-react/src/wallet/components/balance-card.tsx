"use client";

import { useState, type ReactNode } from "react";
import { ChevronDown, ChevronUp, Wallet } from "lucide-react";
import { Button } from "../../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../../components/ui/card";
import { EmptyState } from "../../components/empty-state";
import { ErrorState } from "../../components/error-state";
import { formatAmount, formatUTC } from "../../lib/utils";
import type { WalletBalance, WalletHold } from "../client";
import { useWalletBalance, useWalletHolds } from "../hooks";

type HoldsQuery = ReturnType<typeof useWalletHolds>;

/*
 * Wallet balance surfaces (shadcn skin). User language only: balance,
 * available, pending, locked — never ledger internals. Mirrored in the
 * HeroUI skin (src/wallet/heroui) — keep page logic in sync.
 */

function BalanceCardSkeleton() {
  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="h-4 w-24 animate-shimmer rounded" />
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="h-8 w-40 animate-shimmer rounded" />
        <div className="h-4 w-full animate-shimmer rounded" />
        <div className="h-4 w-full animate-shimmer rounded" />
      </CardContent>
    </Card>
  );
}

// J-3 (2026-09-02 web audit): the holds query's own isLoading/isError were
// never read here, so a failed /holder/holds request rendered the SAME
// "Nothing is on hold right now." as a genuinely empty result — directly
// contradicting a non-zero `locked` figure shown one row up on the same
// card. Four states now: loading → error (with Retry) → empty → data.
function HoldsDetail({
  list,
  isLoading,
  isError,
  onRetry,
}: {
  list: WalletHold[];
  isLoading: boolean;
  isError: boolean;
  onRetry: () => void;
}) {
  if (isLoading) {
    return <div className="h-4 w-full animate-shimmer rounded my-1" />;
  }
  if (isError) {
    return (
      <div className="flex items-center justify-between gap-2 py-1 text-xs text-destructive">
        <span>Couldn&apos;t load hold details.</span>
        <Button variant="ghost" size="sm" className="h-6 px-2" onClick={onRetry}>
          Retry
        </Button>
      </div>
    );
  }
  if (list.length === 0) {
    return (
      <p className="text-xs text-muted-foreground py-1">
        Nothing is on hold right now.
      </p>
    );
  }
  return (
    <ul className="space-y-1 py-1">
      {list.map((h) => (
        <li
          key={h.uid}
          className="flex items-center justify-between text-xs text-muted-foreground"
        >
          <span>
            On hold until{" "}
            <time dateTime={h.expires_at}>{formatUTC(h.expires_at)}</time>
          </span>
          <span className="tabular-nums">
            {formatAmount(h.amount)} {h.currency_code}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** Presentational card for one currency's balance breakdown. */
function BalanceCardView({
  balance,
  holds,
  actions,
}: {
  balance: WalletBalance;
  holds: HoldsQuery;
  actions?: ReactNode;
}) {
  const [showHolds, setShowHolds] = useState(false);
  const currencyHolds = (holds.data ?? []).filter(
    (h) => h.currency_uid === balance.currency_uid,
  );
  const rows = [
    { label: "Available", value: balance.available },
    { label: "Pending", value: balance.pending },
  ];

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">
          {balance.currency_code} balance
        </CardTitle>
        {actions}
      </CardHeader>
      <CardContent>
        <p className="text-3xl font-bold tabular-nums">
          {formatAmount(balance.total)}
          <span className="ml-2 text-base font-normal text-muted-foreground">
            {balance.currency_code}
          </span>
        </p>
        <dl className="mt-4 space-y-2 text-sm">
          {rows.map((r) => (
            <div key={r.label} className="flex items-center justify-between">
              <dt className="text-muted-foreground">{r.label}</dt>
              <dd className="tabular-nums">{formatAmount(r.value)}</dd>
            </div>
          ))}
          <div className="flex items-center justify-between">
            <dt className="text-muted-foreground">
              <Button
                variant="ghost"
                size="sm"
                className="-ml-2 h-6 gap-1 px-2 text-muted-foreground"
                onClick={() => setShowHolds((v) => !v)}
                aria-expanded={showHolds}
              >
                On hold
                {showHolds ? (
                  <ChevronUp className="h-3 w-3" aria-hidden />
                ) : (
                  <ChevronDown className="h-3 w-3" aria-hidden />
                )}
              </Button>
            </dt>
            <dd className="tabular-nums">{formatAmount(balance.locked)}</dd>
          </div>
          {showHolds && (
            <HoldsDetail
              list={currencyHolds}
              isLoading={holds.isLoading}
              isError={holds.isError}
              onRetry={() => holds.refetch()}
            />
          )}
        </dl>
      </CardContent>
    </Card>
  );
}

export interface WalletBalanceCardProps {
  /** Currency to show. Omit only when the holder has exactly one currency. */
  currencyUid?: string;
  /** Host-provided actions (top-up / cash-out buttons — writes are the product's flow). */
  actions?: ReactNode;
}

/** One currency's balance card: total + available / pending / on-hold rows. */
export function WalletBalanceCard({ currencyUid, actions }: WalletBalanceCardProps) {
  const { data, isLoading, isError, refetch } = useWalletBalance(currencyUid);
  const holds = useWalletHolds();

  if (isLoading) return <BalanceCardSkeleton />;
  if (isError) return <ErrorState message="Couldn't load your balance. Please try again." onRetry={refetch} />;
  const balance = data?.[0];
  if (!balance) {
    return (
      <EmptyState
        icon={Wallet}
        title="No balance yet"
        description="Your balance will appear here after your first transaction."
      />
    );
  }
  return (
    <BalanceCardView balance={balance} holds={holds} actions={actions} />
  );
}

export interface WalletBalancesProps {
  /** Host-provided actions, rendered on every card. */
  actions?: ReactNode;
}

/** All of the holder's currencies, one balance card each. */
export function WalletBalances({ actions }: WalletBalancesProps) {
  const { data, isLoading, isError, refetch } = useWalletBalance();
  const holds = useWalletHolds();

  if (isLoading) {
    return (
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <BalanceCardSkeleton />
        <BalanceCardSkeleton />
      </div>
    );
  }
  if (isError) return <ErrorState message="Couldn't load your balances. Please try again." onRetry={refetch} />;
  if (!data || data.length === 0) {
    return (
      <EmptyState
        icon={Wallet}
        title="No balance yet"
        description="Your balance will appear here after your first transaction."
      />
    );
  }
  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
      {data.map((b) => (
        <BalanceCardView
          key={b.currency_uid}
          balance={b}
          holds={holds}
          actions={actions}
        />
      ))}
    </div>
  );
}
