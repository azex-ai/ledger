"use client";

import { useSystemBalances } from "@/lib/hooks/use-system";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  ResponsiveContainer,
  BarChart,
  Bar,
  XAxis,
  YAxis,
  Tooltip,
  CartesianGrid,
} from "recharts";
import { AlertCircle } from "lucide-react";

export function BalanceTrend() {
  const { data, isLoading, isError } = useSystemBalances();

  const chartData = (data ?? []).map((b) => ({
    label: `C${b.classification_id} / Cur${b.currency_id}`,
    balance: parseFloat(b.total_balance),
  }));

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-medium">System Balances</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="h-[300px] animate-pulse rounded bg-muted" />
        ) : isError ? (
          <div className="flex h-[300px] items-center justify-center gap-2 text-sm text-destructive">
            <AlertCircle className="h-4 w-4" />
            Failed to load balances
          </div>
        ) : chartData.length === 0 ? (
          <div className="flex h-[300px] items-center justify-center text-sm text-muted-foreground">
            No balance data yet
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={300}>
            <BarChart data={chartData}>
              <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
              <XAxis
                dataKey="label"
                tick={{ fontSize: 12, fill: "hsl(var(--muted-foreground))" }}
              />
              <YAxis tick={{ fontSize: 12, fill: "hsl(var(--muted-foreground))" }} />
              <Tooltip
                contentStyle={{
                  backgroundColor: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "6px",
                  color: "hsl(var(--card-foreground))",
                }}
              />
              <Bar dataKey="balance" fill="hsl(var(--chart-1))" radius={[4, 4, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  );
}
