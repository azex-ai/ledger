"use client";

import { parseHolderInput } from "../../lib/parse-holder-input";
import { useMemo, useState } from "react";
import { Button, Card, Input, Table, TextField, Select, ListBox, Label, toast } from "@heroui/react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip as RechartsTooltip,
  XAxis,
  YAxis,
} from "recharts";
import { useCurrencies, useUidCodeLookups } from "../../hooks/use-metadata";
import { useBalances } from "../../hooks/use-balances";
import { useSnapshots } from "../../hooks/use-system";
import { formatAmount } from "../../lib/utils";
import { errorText } from "../../lib/error-message";
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
  const { data: currencies, isLoading: currenciesLoading, isError: currenciesError, refetch: refetchCurrencies } = useCurrencies();
  const [selectedCurrency, setSelectedCurrency] = useState("");
  const currencyUid = currencies?.some((currency) => currency.uid === selectedCurrency) ? selectedCurrency : currencies?.[0]?.uid ?? "";
  const { data, isLoading, isError, refetch } = useBalances(holder, currencyUid);
  const balances = data ?? [];
  const { currencyCode, classCode } = useUidCodeLookups();

  function submitHolder() {
    const parsed = parseHolderInput(holderInput);
    if (typeof parsed === "string") {
      toast.danger(parsed);
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
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const snapParams = useMemo(
    () => ({
      holder: holder || undefined,
      currency_uid: currencyUid,
      start: thirtyDaysAgo,
      end: today,
    }),
    [holder, currencyUid, thirtyDaysAgo, today],
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
  const chartData = snapshots.reduce<Record<string, Record<string, string | number>>>(
    (acc, s) => {
      if (!acc[s.snapshot_date]) acc[s.snapshot_date] = { date: s.snapshot_date };
      const key = `c${s.classification_uid}`;
      acc[s.snapshot_date][key] = parseFloat(s.balance); // chart geometry only — intentional lossy conversion
      acc[s.snapshot_date][`${key}Raw`] = s.balance;
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
      <PageHeader title="Balances" description="Booked balances by account holder; reservation holds are not deducted" />

      <div className="flex flex-wrap items-end gap-2">
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
        <Select aria-label="Currency" value={currencyUid} onChange={(key) => setSelectedCurrency(String(key ?? ""))} isDisabled={currenciesLoading || currenciesError || !currencies?.length}>
          <Label>Currency</Label>
          <Select.Trigger><Select.Value /><Select.Indicator /></Select.Trigger>
          <Select.Popover><ListBox>{currencies?.map((currency) => <ListBox.Item key={currency.uid} id={currency.uid} textValue={currency.code}>{currency.code}<ListBox.ItemIndicator /></ListBox.Item>)}</ListBox></Select.Popover>
        </Select>
        <Button onPress={submitHolder}>Search</Button>
      </div>

      {currenciesError && <ErrorState message="Failed to load currencies" onRetry={refetchCurrencies} />}
      {!currenciesLoading && !currenciesError && !currencies?.length && <p className="text-sm text-muted">No currencies configured</p>}
      {holder !== 0 && currencyUid !== "" && (
        <>
          {isLoading ? (
            <TableSkeleton rows={4} />
          ) : isError ? (
            <ErrorState message="Failed to load balances" onRetry={refetch} />
          ) : balances.length === 0 ? (
            <EmptyState
              title="No balances found"
              description={`No balances found for holder ${holder}`}
            />
          ) : (
            <Card>
              <Card.Header>
                <Card.Title>Booked balances — Holder {holder}</Card.Title>
              </Card.Header>
              <Card.Content>
                <Table>
                  <Table.ScrollContainer>
                    <Table.Content
                      aria-label={`Booked balances for holder ${holder}`}
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
                            <Table.Cell className="max-w-40"><span className="block truncate" title={b.currency_uid}>{currencyCode(b.currency_uid)}</span></Table.Cell>
                            <Table.Cell className="max-w-40"><span className="block truncate" title={b.classification_uid}>{classCode(b.classification_uid)}</span></Table.Cell>
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

          {balances.length > 0 && (
            <Card>
              <Card.Header>
                <Card.Title>Booked balance trend (30 days) · {currencies?.find((currency) => currency.uid === currencyUid)?.code ?? currencyUid}</Card.Title>
              </Card.Header>
              <Card.Content>
                {snapLoading ? (
                  <TableSkeleton rows={4} />
                ) : snapIsError ? (
                  <ErrorState message={errorText(snapError, "Failed to load balance trend")} onRetry={snapRefetch} />
                ) : chartArray.length === 0 ? (
                  <EmptyState title="No historical snapshots in the last 30 days" />
                ) : (
                  <ResponsiveContainer width="100%" height={300}>
                    <LineChart data={chartArray}>
                      <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" />
                      <XAxis
                        dataKey="date"
                        tick={{ fontSize: 11, fill: "var(--muted)" }}
                      />
                      <YAxis
                        tick={{ fontSize: 11, fill: "var(--muted)" }}
                        tickFormatter={(v) => formatAmount(String(v))}
                      />
                      <RechartsTooltip
                        contentStyle={{
                          backgroundColor: "var(--surface)",
                          border: "1px solid var(--border)",
                          borderRadius: "8px",
                          color: "var(--foreground)",
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
                          stroke={CHART_COLORS[i % CHART_COLORS.length]}
                          dot={false}
                          name={`Classification ${cid}`}
                        />
                      ))}
                    </LineChart>
                  </ResponsiveContainer>
                )}
              </Card.Content>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
