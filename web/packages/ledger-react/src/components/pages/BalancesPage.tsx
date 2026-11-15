"use client";

import { parseHolderInput } from "../../lib/parse-holder-input";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { formatAmount } from "../../lib/utils";
import { ExactAmount } from "../exact-amount";
import { useUidCodeLookups, useCurrencies } from "../../hooks/use-metadata";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Label } from "../ui/label";
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
  const { data: currencies, isLoading: currenciesLoading, isError: currenciesError, refetch: refetchCurrencies } = useCurrencies();
  const [selectedCurrency, setSelectedCurrency] = useState("");
  const trendCurrency = currencies?.some((currency) => currency.uid === selectedCurrency) ? selectedCurrency : currencies?.[0]?.uid ?? "";
  const { data, isLoading, isError, refetch } = useBalances(holder, trendCurrency);
  const balances = data ?? [];
  const { currencyCode, classCode } = useUidCodeLookups();

  function submitHolder() {
    const parsed = parseHolderInput(holderInput);
    if (typeof parsed === "string") {
      toast.error(parsed);
      return;
    }
    setHolder(parsed ?? 0);
  }

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
  // Only the selected currency is charted; quantities in different units are never combined.
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const snapParams = useMemo(
    () => ({
      holder: holder || undefined,
      currency_uid: trendCurrency,
      start: thirtyDaysAgo,
      end: today,
    }),
    [holder, trendCurrency, thirtyDaysAgo, today],
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
      <PageHeader title="Balances" description="Booked balances by account holder; reservation holds are not deducted" />

      <div className="flex flex-wrap items-end gap-2">
        {/* J-12 (2026-09-02 web audit): placeholder text is not a substitute
            for an accessible name (it disappears once a value is entered and
            isn't reliably announced by every screen reader) — mirrors the
            HeroUI skin's `aria-label="Account Holder ID"` on the same field. */}
        <Input
          aria-label="Account Holder ID"
          placeholder="Account Holder ID"
          value={holderInput}
          onChange={(e) => setHolderInput(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && submitHolder()}
          className="max-w-xs"
        />
        <div className="grid gap-2">
          <Label htmlFor="balance-currency">Currency</Label>
          <Select value={trendCurrency} onValueChange={(value) => { if (value) setSelectedCurrency(value); }} disabled={currenciesLoading || currenciesError || !currencies?.length}>
            <SelectTrigger id="balance-currency"><SelectValue placeholder={currenciesLoading ? "Loading currencies..." : "Select currency"}>{trendCurrency ? currencyCode(trendCurrency) : undefined}</SelectValue></SelectTrigger>
            <SelectContent>{currencies?.map((currency) => <SelectItem key={currency.uid} value={currency.uid}>{currency.code}</SelectItem>)}</SelectContent>
          </Select>
        </div>
        <Button onClick={submitHolder}>Search</Button>
      </div>

      {currenciesError && <ErrorState message="Failed to load currencies" onRetry={refetchCurrencies} />}
      {!currenciesLoading && !currenciesError && !currencies?.length && <p className="text-sm text-muted-foreground">No currencies configured</p>}
      {/* Negative holders are the system-side counterpart accounts — equally queryable. */}
      {holder !== 0 && trendCurrency !== "" && (
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
                  Booked balances — Holder {holder}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <Table aria-label={`Booked balances for holder ${holder}`}>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Currency</TableHead>
                      <TableHead>Classification</TableHead>
                      <TableHead className="text-right">Booked balance</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {balances.map((b) => (
                      <TableRow key={`${b.currency_uid}-${b.classification_uid}`}>
                        <TableCell className="max-w-40">
                          <span className="block truncate font-mono text-xs" title={b.currency_uid}>{currencyCode(b.currency_uid)}</span>
                        </TableCell>
                        <TableCell className="max-w-40">
                          <span className="block truncate font-mono text-xs" title={b.classification_uid}>{classCode(b.classification_uid)}</span>
                        </TableCell>
                        <TableCell className="text-right tabular-nums"><ExactAmount value={b.balance} currencyCode={currencyCode(b.currency_uid)} /></TableCell>
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
                <CardTitle className="text-sm font-medium">Booked balance trend (30 days) · {trendCurrency ? currencyCode(trendCurrency) : ""}</CardTitle>
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
                          return [`${typeof raw === "string" ? raw : String(value)} ${trendCurrency ? currencyCode(trendCurrency) : ""}`, name];
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
