"use client";

import { useMemo, useState } from "react";
import { Button, Card, Input, Table, TextField } from "@heroui/react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip as RechartsTooltip,
  XAxis,
  YAxis,
} from "recharts";
import { useBalances } from "../../hooks/use-balances";
import { useSnapshots } from "../../hooks/use-system";
import { formatAmount } from "../../lib/utils";
import { EmptyState, ErrorState, PageHeader, TableSkeleton } from "../shared";

// Self-contained placeholder palette: the HeroUI skin ships no design tokens
// (heroui.css has "no preflight, no tokens, no HeroUI theme" — see its header
// comment), so this can't reference the shadcn skin's --chart-* vars. Swap
// for brand colors when this skin gets a real theme.
const CHART_COLORS = [
  "oklch(0.75 0.18 230)",
  "oklch(0.7 0.18 150)",
  "oklch(0.65 0.18 30)",
  "oklch(0.7 0.18 300)",
  "oklch(0.75 0.15 80)",
];

export function BalancesPage() {
  const [holderInput, setHolderInput] = useState("");
  const [holder, setHolder] = useState(0);
  const { data, isLoading, isError } = useBalances(holder);
  const balances = data ?? [];

  const submitHolder = () => setHolder(parseInt(holderInput, 10) || 0);

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
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const snapParams = useMemo(
    () => ({ holder: holder || undefined, start: thirtyDaysAgo, end: today }),
    [holder, thirtyDaysAgo, today],
  );
  const { data: snapData } = useSnapshots(snapParams);
  const snapshots = snapData ?? [];

  const chartData = snapshots.reduce<Record<string, Record<string, string | number>>>(
    (acc, s) => {
      if (!acc[s.snapshot_date]) acc[s.snapshot_date] = { date: s.snapshot_date };
      // chart display only — intentional lossy conversion
      acc[s.snapshot_date][`c${s.classification_uid}`] = parseFloat(s.balance);
      return acc;
    },
    {},
  );
  const chartArray = Object.values(chartData).sort((a, b) =>
    String(a.date).localeCompare(String(b.date)),
  );
  const classIds = [...new Set(snapshots.map((s) => s.classification_uid))];

  return (
    <div className="space-y-6">
      <PageHeader title="Balances" description="Search balances by account holder" />

      <div className="flex gap-2">
        <TextField
          aria-label="Account Holder ID"
          className="max-w-xs"
          value={holderInput}
          onChange={setHolderInput}
        >
          <Input
            placeholder="Account Holder ID"
            onKeyDown={(e) => {
              if (e.key === "Enter") submitHolder();
            }}
          />
        </TextField>
        <Button onPress={submitHolder}>Search</Button>
      </div>

      {holder !== 0 && (
        <>
          {isLoading ? (
            <TableSkeleton rows={4} />
          ) : isError ? (
            <ErrorState message="Failed to load balances" />
          ) : balances.length === 0 ? (
            <EmptyState
              title="No balances found"
              description={`No balances found for holder ${holder}`}
            />
          ) : (
            <Card>
              <Card.Header>
                <Card.Title>Balance Breakdown — Holder {holder}</Card.Title>
              </Card.Header>
              <Card.Content>
                <Table>
                  <Table.ScrollContainer>
                    <Table.Content
                      aria-label={`Balance breakdown for holder ${holder}`}
                      className="min-w-[480px]"
                    >
                      <Table.Header>
                        <Table.Column isRowHeader>Currency</Table.Column>
                        <Table.Column>Classification</Table.Column>
                        <Table.Column className="text-end">Balance</Table.Column>
                      </Table.Header>
                      <Table.Body items={balances}>
                        {(b) => (
                          <Table.Row id={`${b.currency_uid}-${b.classification_uid}`}>
                            <Table.Cell className="max-w-40"><span className="block truncate" title={b.currency_uid}>{b.currency_uid}</span></Table.Cell>
                            <Table.Cell className="max-w-40"><span className="block truncate" title={b.classification_uid}>{b.classification_uid}</span></Table.Cell>
                            <Table.Cell className="text-end font-mono tabular-nums">
                              {formatAmount(b.balance)}
                            </Table.Cell>
                          </Table.Row>
                        )}
                      </Table.Body>
                    </Table.Content>
                  </Table.ScrollContainer>
                </Table>
              </Card.Content>
            </Card>
          )}

          {chartArray.length > 0 && (
            <Card>
              <Card.Header>
                <Card.Title>Balance Trend (30 days)</Card.Title>
              </Card.Header>
              <Card.Content>
                <ResponsiveContainer width="100%" height={300}>
                  <LineChart data={chartArray}>
                    <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" />
                    <XAxis
                      dataKey="date"
                      tick={{ fontSize: 11, fill: "var(--muted)" }}
                    />
                    <YAxis tick={{ fontSize: 11, fill: "var(--muted)" }} />
                    <RechartsTooltip
                      contentStyle={{
                        backgroundColor: "var(--surface)",
                        border: "1px solid var(--border)",
                        borderRadius: "8px",
                        color: "var(--foreground)",
                      }}
                    />
                    {classIds.map((cid, i) => (
                      <Line
                        key={cid}
                        type="monotone"
                        dataKey={`c${cid}`}
                        stroke={CHART_COLORS[i % CHART_COLORS.length]}
                        dot={false}
                        name={`Classification ${cid}`}
                      />
                    ))}
                  </LineChart>
                </ResponsiveContainer>
              </Card.Content>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
