"use client";

import { useMemo, useState } from "react";
import {
  Label, ListBox, Select, Table,
} from "@heroui/react";
import { Combine } from "lucide-react";
import { formatAmount, formatUTC } from "../../lib/utils";
import { useSweeps } from "../../hooks/use-sweeps";
import { EmptyState, ErrorState, PageHeader, StatusChip, TableSkeleton } from "../shared";
import { LoadMoreBar } from "../pagination-bar";

const SWEEP_STATES = ["pending", "sent", "confirmed", "failed"];

function metaString(metadata: Record<string, unknown>, key: string): string {
  const v = metadata[key];
  return typeof v === "string" && v !== "" ? v : "—";
}

function addressCount(metadata: Record<string, unknown>): number {
  const v = metadata["addresses"];
  return typeof v === "string" && v !== "" ? v.split(",").filter(Boolean).length : 0;
}

export function SweepMonitorPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const params = useMemo(() => ({ status: statusFilter || undefined }), [statusFilter]);
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useSweeps(params);
  const sweeps = data?.pages.flatMap((p) => p.list) ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Sweep Monitor" description="On-chain custody collection audit trail" />

      <div className="flex gap-2">
        <Select
          className="w-40"
          value={statusFilter || "all"}
          onChange={(key) => setStatusFilter(key && key !== "all" ? String(key) : "")}
        >
          <Label className="sr-only">Status</Label>
          <Select.Trigger>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              <ListBox.Item id="all" textValue="All">
                All
                <ListBox.ItemIndicator />
              </ListBox.Item>
              {SWEEP_STATES.map((s) => (
                <ListBox.Item key={s} id={s} textValue={s}>
                  {s}
                  <ListBox.ItemIndicator />
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load sweeps" />
      ) : sweeps.length === 0 ? (
        <EmptyState
          icon={<Combine aria-hidden className="size-8 text-muted" />}
          title="No sweeps found"
          description={statusFilter ? "Try a different status filter." : "No sweep batches have run yet."}
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Sweeps" className="min-w-[860px]">
              <Table.Header>
                <Table.Column isRowHeader>UID</Table.Column>
                <Table.Column>Chain</Table.Column>
                <Table.Column>Token</Table.Column>
                <Table.Column className="text-end">Addresses</Table.Column>
                <Table.Column className="text-end">Total</Table.Column>
                <Table.Column>Status</Table.Column>
                <Table.Column>Tx Hash</Table.Column>
                <Table.Column className="text-end">Created</Table.Column>
              </Table.Header>
              <Table.Body items={sweeps}>
                {(s) => (
                  <Table.Row id={s.uid}>
                    <Table.Cell className="max-w-32">
                      <span className="block truncate font-mono text-xs" title={s.uid}>#{s.uid}</span>
                    </Table.Cell>
                    <Table.Cell>{metaString(s.metadata, "chain_id")}</Table.Cell>
                    <Table.Cell>{metaString(s.metadata, "token")}</Table.Cell>
                    <Table.Cell className="text-end tabular-nums">{addressCount(s.metadata)}</Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {formatAmount(s.amount)}
                    </Table.Cell>
                    <Table.Cell>
                      <StatusChip status={s.status} />
                    </Table.Cell>
                    <Table.Cell className="max-w-40">
                      <span className="block truncate font-mono text-xs" title={s.channel_ref}>
                        {s.channel_ref || "—"}
                      </span>
                    </Table.Cell>
                    <Table.Cell className="text-muted text-end text-xs">
                      {formatUTC(s.created_at)}
                    </Table.Cell>
                  </Table.Row>
                )}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </Table>
      )}
    </div>
  );
}
