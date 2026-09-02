"use client";

import { useState } from "react";
import { toast } from "sonner";
import { formatAmount } from "../../lib/utils";
import { useSnapshots } from "../../hooks/use-system";
import { useUidCodeLookups } from "../../hooks/use-metadata";
import { errorText } from "../../lib/error-message";
import { ErrorState } from "../error-state";
import { TableSkeleton } from "../loading-skeleton";
import { PaginationBar } from "../pagination-bar";
import { useClientPage } from "../../lib/use-client-page";
import { PageHeader } from "../page-header";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";

interface SnapshotQuery {
  holder?: number;
  currency_uid?: string;
  start?: string;
  end?: string;
}

export function SnapshotsPage() {
  const [form, setForm] = useState({
    holder: "",
    currency_uid: "",
    start: "",
    end: "",
  });
  // `query` is the params object passed to useSnapshots. It lives in state and
  // only changes identity when handleSearch runs (setQuery), so it stays stable
  // across unrelated re-renders — no inline-object refetch storm.
  const [query, setQuery] = useState<SnapshotQuery>({});
  // Explicit "did a real search run" flag (J-2, 2026-09-02 web audit) — NOT
  // derived from Object.keys(query).length, which is 4 the instant
  // handleSearch has ever run once (every key is always written, some just
  // undefined), so it can never distinguish "never searched" from "searched
  // with holder missing".
  const [searched, setSearched] = useState(false);

  const { data, isLoading, isError, error, refetch } = useSnapshots(query);
  const { classCode, currencyCode } = useUidCodeLookups();
  const snapshots = data ?? [];
  const { pageItems, page, pageCount, setPage } = useClientPage(snapshots);

  // holder/currency_uid/start/end are ALL hard-required by the server
  // (server/handler_system.go's handleListSnapshots 400s on any one missing).
  // Validate client-side before ever writing `query` — previously a
  // holder-less search silently produced zero requests and rendered "No
  // snapshots found", indistinguishable from a real empty result (J-2).
  function handleSearch() {
    const holder = form.holder ? parseInt(form.holder, 10) : undefined;
    if (!holder) {
      toast.error("Holder is required");
      return;
    }
    if (!form.currency_uid.trim()) {
      toast.error("Currency is required");
      return;
    }
    if (!form.start || !form.end) {
      toast.error("Start and end date are required");
      return;
    }
    setQuery({ holder, currency_uid: form.currency_uid.trim(), start: form.start, end: form.end });
    setSearched(true);
  }

  return (
    <div className="space-y-6">
      <PageHeader title="Snapshots" description="Historical balance snapshots" />

      <div className="flex flex-wrap gap-3 items-end">
        <div className="grid gap-1">
          <Label htmlFor="snap-holder" className="text-xs">Holder</Label>
          <Input id="snap-holder" value={form.holder} onChange={(e) => setForm({ ...form, holder: e.target.value })} placeholder="1001" className="w-28" />
        </div>
        <div className="grid gap-1">
          <Label htmlFor="snap-currency" className="text-xs">Currency</Label>
          <Input id="snap-currency" value={form.currency_uid} onChange={(e) => setForm({ ...form, currency_uid: e.target.value })} placeholder="1" className="w-28" />
        </div>
        <div className="grid gap-1">
          <Label htmlFor="snap-start" className="text-xs">Start Date</Label>
          <Input id="snap-start" type="date" value={form.start} onChange={(e) => setForm({ ...form, start: e.target.value })} className="w-40" />
        </div>
        <div className="grid gap-1">
          <Label htmlFor="snap-end" className="text-xs">End Date</Label>
          <Input id="snap-end" type="date" value={form.end} onChange={(e) => setForm({ ...form, end: e.target.value })} className="w-40" />
        </div>
        <Button onClick={handleSearch}>Search</Button>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message={errorText(error, "Failed to load snapshots")} onRetry={refetch} />
      ) : snapshots.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          {searched ? "No snapshots found" : "Enter search criteria to view snapshots"}
        </p>
      ) : (
        <>
        <Table aria-label="Balance snapshots">
          <TableHeader>
            <TableRow>
              <TableHead>Date</TableHead>
              <TableHead>Holder</TableHead>
              <TableHead>Currency</TableHead>
              <TableHead>Classification</TableHead>
              <TableHead className="text-right">Balance</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageItems.map((s) => (
              <TableRow key={`${s.snapshot_date}-${s.account_holder}-${s.currency_uid}-${s.classification_uid}`}>
                <TableCell>{s.snapshot_date}</TableCell>
                <TableCell>{s.account_holder}</TableCell>
                <TableCell title={s.currency_uid}>{currencyCode(s.currency_uid)}</TableCell>
                <TableCell title={s.classification_uid}>{classCode(s.classification_uid)}</TableCell>
                <TableCell className="text-right tabular-nums">{formatAmount(s.balance)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <PaginationBar page={page} pageCount={pageCount} onPageChange={setPage} />
        </>
      )}
    </div>
  );
}
