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
import { AlertCircle, TrendingUp } from "lucide-react";

export function BalanceTrend() {
  const { data, isLoading, isError } = useSystemBalances();

  const chartData = (data ?? []).map((b) => ({
    label: `C${b.classification_id} / Cur${b.currency_id}`,
    balance: parseFloat(b.total_balance), // chart display only — intentional lossy conversion
  }));

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle className="text-sm font-medium">System Balances</CardTitle>
        <TrendingUp className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="h-[300px] animate-shimmer rounded" />
        ) : isError ? (
          <div className="flex h-[300px] flex-col items-center justify-center gap-2 text-sm text-destructive">
            <AlertCircle className="h-5 w-5" />
            <span>Failed to load balances</span>
          </div>
        ) : chartData.length === 0 ? (
          <div className="flex h-[300px] flex-col items-center justify-center gap-2">
            <TrendingUp className="h-8 w-8 text-muted-foreground/50" />
            <span className="text-sm text-muted-foreground">No balance data yet</span>
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={300}>
            <BarChart data={chartData} barCategoryGap="20%">
              <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
              <XAxis
                dataKey="label"
                tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
                axisLine={false}
                tickLine={false}
              />
              <YAxis
                tick={{ fontSize: 11, fill: "var(--muted-foreground)" }}
                axisLine={false}
                tickLine={false}
                width={60}
              />
              <Tooltip
                cursor={{ fill: "color-mix(in oklch, var(--muted) 50%, transparent)" }}
                contentStyle={{
                  backgroundColor: "var(--popover)",
                  border: "1px solid var(--border)",
                  borderRadius: "8px",
                  color: "var(--popover-foreground)",
                  fontSize: "12px",
                  boxShadow: "0 4px 12px rgba(0,0,0,0.3)",
                }}
              />
              <Bar dataKey="balance" fill="var(--chart-1)" radius={[6, 6, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  );
}
