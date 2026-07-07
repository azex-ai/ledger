"use client";

import { useState, type ReactNode } from "react";
import { Button, Card, Skeleton, cn } from "@heroui/react";
import { ChevronDown, ChevronUp, Wallet } from "lucide-react";
import { EmptyState, ErrorState } from "../../heroui/shared";
import { formatAmount, formatUTC } from "../../lib/utils";
import type { WalletBalance, WalletHold } from "../client";
import { useWalletBalance, useWalletHolds } from "../hooks";

/*
 * Wallet balance surfaces (HeroUI skin). Page logic mirrors the shadcn skin
 * (src/wallet/components/balance-card.tsx) — keep in sync.
 */

function BalanceCardSkeleton() {
  return (
    <Card>
      <Card.Header>
        <Skeleton className="h-4 w-24 rounded" />
      </Card.Header>
      <Card.Content className="flex flex-col gap-3">
        <Skeleton className="h-8 w-40 rounded" />
        <Skeleton className="h-4 w-full rounded" />
        <Skeleton className="h-4 w-full rounded" />
      </Card.Content>
    </Card>
  );
}

function HoldsDetail({ holds }: { holds: WalletHold[] }) {
  if (holds.length === 0) {
    return <p className="text-muted py-1 text-xs">Nothing is on hold right now.</p>;
  }
  return (
    <ul className="flex flex-col gap-1 py-1">
      {holds.map((h) => (
        <li key={h.uid} className="text-muted flex items-center justify-between text-xs">
          <span>
            On hold until <time dateTime={h.expires_at}>{formatUTC(h.expires_at)}</time>
          </span>
          <span className="tabular-nums">
            {formatAmount(h.amount)} {h.currency_code}
          </span>
        </li>
      ))}
    </ul>
  );
}

function BalanceCardView({
  balance,
  holds,
  actions,
}: {
  balance: WalletBalance;
  holds: WalletHold[];
  actions?: ReactNode;
}) {
  const [showHolds, setShowHolds] = useState(false);
  const currencyHolds = holds.filter(
    (h) => h.currency_uid === balance.currency_uid,
  );
  const rows = [
    { label: "Available", value: balance.available },
    { label: "Pending", value: balance.pending },
  ];

  return (
    <Card>
      <Card.Header className="flex-row items-center justify-between">
        <Card.Title className="text-muted text-sm font-medium">
          {balance.currency_code} balance
        </Card.Title>
        {actions}
      </Card.Header>
      <Card.Content>
        <p className="text-3xl font-bold tabular-nums">
          {formatAmount(balance.total)}
          <span className="text-muted ml-2 text-base font-normal">
            {balance.currency_code}
          </span>
        </p>
        <dl className="mt-4 flex flex-col gap-2 text-sm">
          {rows.map((r) => (
            <div key={r.label} className="flex items-center justify-between">
              <dt className="text-muted">{r.label}</dt>
              <dd className="tabular-nums">{formatAmount(r.value)}</dd>
            </div>
          ))}
          <div className="flex items-center justify-between">
            <dt>
              <Button
                variant="ghost"
                size="sm"
                className="text-muted -ml-2 h-6 gap-1 px-2"
                onPress={() => setShowHolds((v) => !v)}
                aria-expanded={showHolds}
              >
                On hold
                {showHolds ? (
                  <ChevronUp className="size-3" aria-hidden />
                ) : (
                  <ChevronDown className="size-3" aria-hidden />
                )}
              </Button>
            </dt>
            <dd className="tabular-nums">{formatAmount(balance.locked)}</dd>
          </div>
          {showHolds && <HoldsDetail holds={currencyHolds} />}
        </dl>
      </Card.Content>
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
  const { data, isLoading, isError } = useWalletBalance(currencyUid);
  const holds = useWalletHolds();

  if (isLoading) return <BalanceCardSkeleton />;
  if (isError) return <ErrorState message="Couldn't load your balance. Please try again." />;
  const balance = data?.[0];
  if (!balance) {
    return (
      <EmptyState
        icon={<Wallet className="text-muted size-8" aria-hidden />}
        title="No balance yet"
        description="Your balance will appear here after your first transaction."
      />
    );
  }
  return (
    <BalanceCardView balance={balance} holds={holds.data ?? []} actions={actions} />
  );
}

export interface WalletBalancesProps {
  /** Host-provided actions, rendered on every card. */
  actions?: ReactNode;
}

/** All of the holder's currencies, one balance card each. */
export function WalletBalances({ actions }: WalletBalancesProps) {
  const { data, isLoading, isError } = useWalletBalance();
  const holds = useWalletHolds();

  if (isLoading) {
    return (
      <div className={cn("grid grid-cols-1 gap-4 sm:grid-cols-2")}>
        <BalanceCardSkeleton />
        <BalanceCardSkeleton />
      </div>
    );
  }
  if (isError) return <ErrorState message="Couldn't load your balances. Please try again." />;
  if (!data || data.length === 0) {
    return (
      <EmptyState
        icon={<Wallet className="text-muted size-8" aria-hidden />}
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
          holds={holds.data ?? []}
          actions={actions}
        />
      ))}
    </div>
  );
}
