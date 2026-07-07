"use client";

import { Activity, Clock, Lock, Scale, TrendingUp, BookOpen, type LucideIcon } from "lucide-react";
import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { Card, Chip, Skeleton, Table } from "@heroui/react";
import { useHealth, useSystemBalances } from "../../hooks/use-system";
import { useClassifications, useCurrencies } from "../../hooks/use-metadata";
import { useJournals } from "../../hooks/use-journals";
import { formatAmount, formatUTC } from "../../lib/utils";
import { DefaultLink, type LinkComponent } from "../../components/nav";
import { EmptyState, ErrorState, PageHeader, TableSkeleton } from "../shared";

export interface DashboardPageProps {
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so the
   * page works without a host router. Forwarded to the Recent Journals widget
   * for its row links and "View all" link.
   */
  linkComponent?: LinkComponent;
}

export function DashboardPage({ linkComponent = DefaultLink }: DashboardPageProps = {}) {
  return (
    <div className="space-y-6">
      <PageHeader title="Dashboard" description="System overview and recent activity" />
      <HealthCards />
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <SystemBalancesCard />
        <RecentJournalsCard linkComponent={linkComponent} />
      </div>
    </div>
  );
}

// ─── Health cards ───────────────────────────────────────────────────

type ChipColor = "success" | "warning" | "danger" | "accent";

interface HealthCard {
  title: string;
  value: string;
  icon: LucideIcon;
  desc: string;
  chip?: { color: ChipColor; label: string };
}

function HealthCards() {
  const { data, isLoading, isError } = useHealth();

  if (isError) {
    return (
      <ErrorState message="Unable to reach the API. Health check failed — is the backend running?" />
    );
  }

  const isHealthy = data?.status === "ok";
  const isDegraded = data?.status === "degraded";

  const cards: HealthCard[] = [
    {
      title: "Rollup Queue",
      value: data ? String(data.rollup_queue_depth) : "-",
      icon: Activity,
      desc: "Pending rollups",
      chip: data
        ? Number(data.rollup_queue_depth) > 100
          ? { color: "warning", label: "Backlogged" }
          : { color: "success", label: "Normal" }
        : undefined,
    },
    {
      title: "Checkpoint Age",
      value: data ? `${data.checkpoint_max_age_seconds}s` : "-",
      icon: Clock,
      desc: "Max age (seconds)",
      chip: data
        ? Number(data.checkpoint_max_age_seconds) > 300
          ? { color: "warning", label: "Stale" }
          : { color: "success", label: "Fresh" }
        : undefined,
    },
    {
      title: "Active Reservations",
      value: data ? String(data.active_reservations) : "-",
      icon: Lock,
      desc: "Currently locked",
    },
    {
      title: "Status",
      value: data
        ? isHealthy
          ? "Healthy"
          : isDegraded
            ? "Degraded"
            : data.status
        : "-",
      icon: Scale,
      desc: "System health",

    },
  ];

  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {cards.map((c) => {
        const Icon = c.icon;
        return (
          <Card key={c.title}>
            <Card.Header className="flex-row items-center justify-between gap-2">
              <Card.Title className="text-sm font-medium text-muted">{c.title}</Card.Title>
              <Icon aria-hidden className="size-4 text-muted" />
            </Card.Header>
            <Card.Content className="gap-1">
              {isLoading ? (
                <Skeleton className="h-7 w-16 rounded-lg" />
              ) : (
                <div className="flex items-center gap-2">
                  <span className="text-2xl font-semibold">{c.value}</span>
                  {c.chip ? (
                    <Chip color={c.chip.color} size="sm" variant="soft">
                      {c.chip.label}
                    </Chip>
                  ) : null}
                </div>
              )}
              <p className="text-xs text-muted">{c.desc}</p>
            </Card.Content>
          </Card>
        );
      })}
    </div>
  );
}

// ─── System balances chart ──────────────────────────────────────────

function SystemBalancesCard() {
  const { data, isLoading, isError } = useSystemBalances();
  // uid → human code lookups for axis labels. Metadata lists are small and
  // cached; while they load, fall back to a shortened uid.
  const { data: classifications } = useClassifications();
  const { data: currencies } = useCurrencies();

  const classCode = (uid: string) =>
    classifications?.find((c) => c.uid === uid)?.code ?? uid.slice(0, 8);
  const currencyCode = (uid: string) =>
    currencies?.find((c) => c.uid === uid)?.code ?? uid.slice(0, 8);

  const chartData = (data ?? []).map((b) => ({
    label: `${classCode(b.classification_uid)} · ${currencyCode(b.currency_uid)}`,
    balance: parseFloat(b.total_balance), // chart display only — intentional lossy conversion
  }));

  return (
    <Card>
      <Card.Header className="flex-row items-center justify-between">
        <Card.Title className="text-sm font-medium">System Balances</Card.Title>
        <TrendingUp aria-hidden className="size-4 text-muted" />
      </Card.Header>
      <Card.Content>
        {isLoading ? (
          <Skeleton className="h-[300px] w-full rounded-lg" />
        ) : isError ? (
          <ErrorState message="Failed to load balances" />
        ) : chartData.length === 0 ? (
          <EmptyState
            icon={<TrendingUp aria-hidden className="size-8 text-muted" />}
            title="No balance data yet"
          />
        ) : (
          <ResponsiveContainer width="100%" height={300}>
            <BarChart data={chartData} barCategoryGap="20%">
              <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
              <XAxis
                dataKey="label"
                tick={{ fontSize: 11, fill: "var(--muted)" }}
                axisLine={false}
                tickLine={false}
              />
              <YAxis
                tick={{ fontSize: 11, fill: "var(--muted)" }}
                axisLine={false}
                tickLine={false}
                width={60}
              />
              <Tooltip
                cursor={{ fill: "color-mix(in oklch, var(--muted) 20%, transparent)" }}
                contentStyle={{
                  backgroundColor: "var(--surface-secondary)",
                  border: "1px solid var(--border)",
                  borderRadius: "8px",
                  color: "var(--foreground)",
                  fontSize: "12px",
                  boxShadow: "0 4px 12px oklch(0 0 0 / 0.15)",
                }}
              />
              <Bar dataKey="balance" fill="var(--accent)" radius={[6, 6, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        )}
      </Card.Content>
    </Card>
  );
}

// ─── Recent journals ─────────────────────────────────────────────────

function RecentJournalsCard({ linkComponent: Link = DefaultLink }: { linkComponent?: LinkComponent }) {
  const { data, isLoading, isError } = useJournals(10);
  const journals = data?.pages[0]?.list ?? [];

  return (
    <Card>
      <Card.Header className="flex-row items-center justify-between">
        <Card.Title className="text-sm font-medium">Recent Journals</Card.Title>
        <Link
          href="/journals"
          className="text-xs text-muted transition-colors hover:text-foreground"
        >
          View all &rarr;
        </Link>
      </Card.Header>
      <Card.Content>
        {isLoading ? (
          <TableSkeleton rows={3} />
        ) : isError ? (
          <ErrorState message="Failed to load journals" />
        ) : journals.length === 0 ? (
          <EmptyState icon={<BookOpen aria-hidden className="size-8 text-muted" />} title="No journals yet" />
        ) : (
          <Table>
            <Table.ScrollContainer>
              <Table.Content aria-label="Recent journals" className="min-w-[520px]">
                <Table.Header>
                  <Table.Column isRowHeader className="w-16">
                    ID
                  </Table.Column>
                  <Table.Column>Idempotency Key</Table.Column>
                  <Table.Column>Source</Table.Column>
                  <Table.Column className="text-end">Amount</Table.Column>
                  <Table.Column className="text-end">Created</Table.Column>
                </Table.Header>
                <Table.Body>
                  {journals.map((j) => (
                    <Table.Row key={j.uid} id={j.uid}>
                      <Table.Cell className="max-w-[200px]">
                        <Link
                          href={`/journals/${j.uid}`}
                          className="text-accent block underline-offset-4 hover:underline"
                        >
                          <span className="block truncate" title={j.uid}>#{j.uid}</span>
                        </Link>
                      </Table.Cell>
                      <Table.Cell className="max-w-[200px]">
                        <span title={j.idempotency_key} className="block truncate font-mono text-xs">
                          {j.idempotency_key}
                        </span>
                      </Table.Cell>
                      <Table.Cell className="text-muted">{j.source}</Table.Cell>
                      <Table.Cell className="text-end font-mono">
                        {formatAmount(j.total_debit)}
                      </Table.Cell>
                      <Table.Cell className="text-end text-xs text-muted">
                        {formatUTC(j.created_at)}
                      </Table.Cell>
                    </Table.Row>
                  ))}
                </Table.Body>
              </Table.Content>
            </Table.ScrollContainer>
          </Table>
        )}
      </Card.Content>
    </Card>
  );
}
