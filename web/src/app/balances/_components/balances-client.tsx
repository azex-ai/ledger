"use client";

import { useState } from "react";
import { formatAmount } from "@/lib/utils";
import { useBalances } from "@/lib/hooks/use-balances";
import { useSnapshots } from "@/lib/hooks/use-system";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  ResponsiveContainer, LineChart, Line, XAxis, YAxis, Tooltip, CartesianGrid,
} from "recharts";
import { ErrorState } from "@/components/error-state";

export function BalancesClient() {
  const [holderInput, setHolderInput] = useState("");
  const [holder, setHolder] = useState(0);
  const { data, isLoading, isError } = useBalances(holder);
  const balances = data ?? [];

  const today = new Date().toISOString().slice(0, 10);
  const thirtyDaysAgo = new Date(Date.now() - 30 * 86400000).toISOString().slice(0, 10);
  const { data: snapData } = useSnapshots({
    holder: holder || undefined,
    start: thirtyDaysAgo,
    end: today,
  });
  const snapshots = snapData ?? [];

  const chartData = snapshots.reduce<Record<string, Record<string, string | number>>>((acc, s) => {
    if (!acc[s.snapshot_date]) acc[s.snapshot_date] = { date: s.snapshot_date };
    acc[s.snapshot_date][`c${s.classification_id}`] = parseFloat(s.balance); // chart display only — intentional lossy conversion
    return acc;
  }, {});
  const chartArray = Object.values(chartData).sort((a, b) =>
    String(a.date).localeCompare(String(b.date)),
  );
  const classIds = [...new Set(snapshots.map((s) => s.classification_id))];

  const COLORS = [
    "hsl(var(--chart-1))", "hsl(var(--chart-2))", "hsl(var(--chart-3))",
    "hsl(var(--chart-4))", "hsl(var(--chart-5))",
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Balances" description="Search balances by account holder" />

      <div className="flex gap-2">
        <Input
          placeholder="Account Holder ID"
          value={holderInput}
          onChange={(e) => setHolderInput(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && setHolder(parseInt(holderInput) || 0)}
          className="max-w-xs"
        />
        <Button onClick={() => setHolder(parseInt(holderInput) || 0)}>Search</Button>
      </div>

      {holder > 0 && (
        <>
          {isLoading ? (
            <div className="h-40 animate-shimmer rounded" />
          ) : isError ? (
            <ErrorState message="Failed to load balances" />
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
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Currency</TableHead>
                      <TableHead>Classification</TableHead>
                      <TableHead className="text-right">Balance</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {balances.map((b, i) => (
                      <TableRow key={`${b.currency_id}-${b.classification_id}`}>
                        <TableCell>{b.currency_id}</TableCell>
                        <TableCell>{b.classification_id}</TableCell>
                        <TableCell className="text-right font-mono">{formatAmount(b.balance)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          )}

          {chartArray.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle className="text-sm font-medium">Balance Trend (30 days)</CardTitle>
              </CardHeader>
              <CardContent>
                <ResponsiveContainer width="100%" height={300}>
                  <LineChart data={chartArray}>
                    <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                    <XAxis dataKey="date" tick={{ fontSize: 11, fill: "hsl(var(--muted-foreground))" }} />
                    <YAxis tick={{ fontSize: 11, fill: "hsl(var(--muted-foreground))" }} />
                    <Tooltip
                      contentStyle={{
                        backgroundColor: "hsl(var(--card))",
                        border: "1px solid hsl(var(--border))",
                        borderRadius: "6px",
                        color: "hsl(var(--card-foreground))",
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
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
