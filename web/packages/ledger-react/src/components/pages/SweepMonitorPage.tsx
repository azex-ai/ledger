"use client";

import { useMemo, useState } from "react";
import { formatAmount, formatUTC } from "../../lib/utils";
import { useSweeps } from "../../hooks/use-sweeps";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "../ui/select";
import { Combine } from "lucide-react";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";
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
  const params = useMemo(
    () => ({ status: statusFilter || undefined }),
    [statusFilter],
  );
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useSweeps(params);
  const sweeps = data?.pages.flatMap((p) => p.list) ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Sweep Monitor" description="On-chain custody collection audit trail" />

      <div className="flex gap-2">
        <Select
          value={statusFilter || "all"}
          onValueChange={(v) => setStatusFilter(!v || v === "all" ? "" : v)}
        >
          <SelectTrigger className="w-40">
            <SelectValue placeholder="All statuses" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All</SelectItem>
            {SWEEP_STATES.map((s) => (
              <SelectItem key={s} value={s}>{s}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load sweeps" />
      ) : sweeps.length === 0 ? (
        <EmptyState
          icon={Combine}
          title="No sweeps found"
          description={statusFilter ? "Try a different status filter." : "No sweep batches have run yet."}
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>UID</TableHead>
                <TableHead>Chain</TableHead>
                <TableHead>Token</TableHead>
                <TableHead className="text-right">Addresses</TableHead>
                <TableHead className="text-right">Total</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Tx Hash</TableHead>
                <TableHead className="text-right">Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sweeps.map((s) => (
                <TableRow key={s.uid}>
                  <TableCell className="font-mono text-xs max-w-[140px] truncate" title={s.uid}>
                    #{s.uid}
                  </TableCell>
                  <TableCell>{metaString(s.metadata, "chain_id")}</TableCell>
                  <TableCell>{metaString(s.metadata, "token")}</TableCell>
                  <TableCell className="text-right tabular-nums">{addressCount(s.metadata)}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatAmount(s.amount)}</TableCell>
                  <TableCell><StatusBadge status={s.status} /></TableCell>
                  <TableCell className="font-mono text-xs max-w-[160px] truncate" title={s.channel_ref}>
                    {s.channel_ref || "—"}
                  </TableCell>
                  <TableCell className="text-right text-xs text-muted-foreground">
                    {formatUTC(s.created_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </>
      )}
    </div>
  );
}
