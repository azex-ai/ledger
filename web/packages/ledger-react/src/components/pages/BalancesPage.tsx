"use client";

import { useMemo, useState } from "react";
import { formatAmount } from "../../lib/utils";
import { useBalances } from "../../hooks/use-balances";
import { useSnapshots } from "../../hooks/use-system";
import { errorText } from "../../lib/error-message";
import { PageHeader } from "../page-header";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  ResponsiveContainer, LineChart, Line, XAxis, YAxis, Tooltip, CartesianGrid,
} from "recharts";
import { ErrorState } from "../error-state";

export function BalancesPage() {
  const [holderInput, setHolderInput] = useState("");
  const [holder, setHolder] = useState(0);
  const { data, isLoading, isError, refetch } = useBalances(holder);
  const balances = data ?? [];

  // Memo so the dates are stable across re-renders. Without this, useSnapshots
  // sees a new `start`/`end` every render and refetches forever.
  const { today, thirtyDaysAgo } = useMemo(() => {
    const now = new Date();
    return {
      today: now.toISOString().slice(0, 10),
      thirtyDaysAgo: new Date(now.getTime() - 30 * 86400000)
        .toISOString()
        .slice(0, 10),
    };
  }, []);
  // Snapshots require a single currency_uid (J-1, 2026-09-02 web audit): the
  // server hard-requires it and there's no per-currency aggregate endpoint,
  // so the trend charts the first currency the balance table returned. Until
  // balances load there is no currency to chart, so the query stays disabled
  // (useSnapshots' own isDisabled) rather than firing a guaranteed-400
  // request with currency_uid missing.
  const primaryCurrencyUid = balances[0]?.currency_uid;
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const snapParams = useMemo(
    () => ({
      holder: holder || undefined,
      currency_uid: primaryCurrencyUid,
      start: thirtyDaysAgo,
      end: today,
    }),
    [holder, primaryCurrencyUid, thirtyDaysAgo, today],
  );
  const {
    data: snapData,
    isLoading: snapLoading,
    isError: snapIsError,
    error: snapError,
    refetch: snapRefetch,
  } = useSnapshots(snapParams);
  const snapshots = snapData ?? [];

  // chartData keeps both the lossy Number (geometry only) AND the original
  // decimal string (raw*, for the tooltip/axis formatter — J-5) so no
  // display path reads the parseFloat value directly.
  const chartData = snapshots.reduce<Record<string, Record<string, string | number>>>((acc, s) => {
    if (!acc[s.snapshot_date]) acc[s.snapshot_date] = { date: s.snapshot_date };
    const key = `c${s.classification_uid}`;
    acc[s.snapshot_date][key] = parseFloat(s.balance); // chart geometry only — intentional lossy conversion
    acc[s.snapshot_date][`${key}Raw`] = s.balance;
    return acc;
  }, {});
  const chartArray = Object.values(chartData).sort((a, b) =>
    String(a.date).localeCompare(String(b.date)),
  );
  const classIds = [...new Set(snapshots.map((s) => s.classification_uid))];

  const COLORS = [
    "var(--chart-1)", "var(--chart-2)", "var(--chart-3)",
    "var(--chart-4)", "var(--chart-5)",
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Balances" description="Search balances by account holder" />

      <div className="flex gap-2">
        {/* J-12 (2026-09-02 web audit): placeholder text is not a substitute
            for an accessible name (it disappears once a value is entered and
            isn't reliably announced by every screen reader) — mirrors the
            HeroUI skin's `aria-label="Account Holder ID"` on the same field. */}
        <Input
          aria-label="Account Holder ID"
          placeholder="Account Holder ID"
          value={holderInput}
          onChange={(e) => setHolderInput(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && setHolder(parseInt(holderInput) || 0)}
          className="max-w-xs"
        />
        <Button onClick={() => setHolder(parseInt(holderInput) || 0)}>Search</Button>
      </div>

      {/* Negative holders are the system-side counterpart accounts — equally queryable. */}
      {holder !== 0 && (
        <>
          {isLoading ? (
            <div className="h-40 animate-shimmer rounded" />
          ) : isError ? (
            <ErrorState message="Failed to load balances" onRetry={refetch} />
          ) : balances.length === 0 ? (
            <p className="text-sm text-muted-foreground">No balances found for holder {holder}</p>
          ) : (
            <Card>
              <CardHeader>
                <CardTitle className="text-sm font-medium">
                  Balance Breakdown — Holder {holder}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <Table aria-label={`Balance breakdown for holder ${holder}`}>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Currency</TableHead>
                      <TableHead>Classification</TableHead>
                      <TableHead className="text-right">Balance</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {balances.map((b) => (
                      <TableRow key={`${b.currency_uid}-${b.classification_uid}`}>
                        <TableCell className="max-w-40">
                          <span className="block truncate font-mono text-xs" title={b.currency_uid}>{b.currency_uid}</span>
                        </TableCell>
                        <TableCell className="max-w-40">
                          <span className="block truncate font-mono text-xs" title={b.classification_uid}>{b.classification_uid}</span>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">{formatAmount(b.balance)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          )}

          {balances.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle className="text-sm font-medium">Balance Trend (30 days)</CardTitle>
              </CardHeader>
              <CardContent>
                {snapLoading ? (
                  <div className="h-[300px] animate-shimmer rounded" />
                ) : snapIsError ? (
                  <ErrorState message={errorText(snapError, "Failed to load balance trend")} onRetry={snapRefetch} />
                ) : chartArray.length === 0 ? (
                  <p className="text-sm text-muted-foreground">No historical snapshots in the last 30 days</p>
                ) : (
                  <ResponsiveContainer width="100%" height={300}>
                    <LineChart data={chartArray}>
                      <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" />
                      <XAxis dataKey="date" tick={{ fontSize: 11, fill: "var(--muted-foreground)" }} />
                      <YAxis
                        tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
                        tickFormatter={(v) => formatAmount(String(v))}
                      />
                      <Tooltip
                        contentStyle={{
                          backgroundColor: "var(--card)",
                          border: "1px solid var(--border)",
                          borderRadius: "6px",
                          color: "var(--card-foreground)",
                        }}
                        formatter={(value, name, entry) => {
                          const payload = entry?.payload as Record<string, string | number> | undefined;
                          const raw = payload?.[`${entry?.dataKey}Raw`];
                          return [formatAmount(typeof raw === "string" ? raw : String(value)), name];
                        }}
                      />
                      {classIds.map((cid, i) => (
                        <Line
                          key={cid}
                          type="monotone"
                          dataKey={`c${cid}`}
                          stroke={COLORS[i % COLORS.length]}
                          dot={false}
                          name={`Classification ${cid}`}
                        />
                      ))}
                    </LineChart>
                  </ResponsiveContainer>
                )}
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
