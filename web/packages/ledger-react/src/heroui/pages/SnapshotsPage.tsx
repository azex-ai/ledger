"use client";

import { useState } from "react";
import { Button, Input, Label, Table, TextField } from "@heroui/react";
import { toast } from "sonner";
import { useSnapshots } from "../../hooks/use-system";
import { useUidCodeLookups } from "../../hooks/use-metadata";
import { formatAmount } from "../../lib/utils";
import { errorText } from "../../lib/error-message";
import { EmptyState, ErrorState, PageHeader, TableSkeleton } from "../shared";
import { PaginationBar } from "../pagination-bar";
import { useClientPage } from "../../lib/use-client-page";

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

      <div className="flex flex-wrap items-end gap-3">
        <TextField
          className="w-28"
          value={form.holder}
          onChange={(v) => setForm({ ...form, holder: v })}
        >
          <Label className="text-xs">Holder</Label>
          <Input placeholder="1001" />
        </TextField>
        <TextField
          className="w-28"
          value={form.currency_uid}
          onChange={(v) => setForm({ ...form, currency_uid: v })}
        >
          <Label className="text-xs">Currency</Label>
          <Input placeholder="1" />
        </TextField>
        <TextField
          className="w-40"
          type="date"
          value={form.start}
          onChange={(v) => setForm({ ...form, start: v })}
        >
          <Label className="text-xs">Start Date</Label>
          <Input />
        </TextField>
        <TextField
          className="w-40"
          type="date"
          value={form.end}
          onChange={(v) => setForm({ ...form, end: v })}
        >
          <Label className="text-xs">End Date</Label>
          <Input />
        </TextField>
        <Button onPress={handleSearch}>Search</Button>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message={errorText(error, "Failed to load snapshots")} onRetry={refetch} />
      ) : snapshots.length === 0 ? (
        <EmptyState
          title={searched ? "No snapshots found" : "Enter search criteria to view snapshots"}
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Balance snapshots" className="min-w-[560px]">
              <Table.Header>
                <Table.Column isRowHeader>Date</Table.Column>
                <Table.Column>Holder</Table.Column>
                <Table.Column>Currency</Table.Column>
                <Table.Column>Classification</Table.Column>
                <Table.Column className="text-end">Balance</Table.Column>
              </Table.Header>
              <Table.Body>
                {pageItems.map((s) => {
                  const rowId = `${s.snapshot_date}-${s.account_holder}-${s.currency_uid}-${s.classification_uid}`;
                  return (
                    <Table.Row key={rowId} id={rowId}>
                      <Table.Cell>{s.snapshot_date}</Table.Cell>
                      <Table.Cell>{s.account_holder}</Table.Cell>
                      <Table.Cell><span title={s.currency_uid}>{currencyCode(s.currency_uid)}</span></Table.Cell>
                      <Table.Cell><span title={s.classification_uid}>{classCode(s.classification_uid)}</span></Table.Cell>
                      <Table.Cell className="text-end font-mono">
                        {formatAmount(s.balance)}
                      </Table.Cell>
                    </Table.Row>
                  );
                })}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          <Table.Footer>
            <PaginationBar page={page} pageCount={pageCount} onPageChange={setPage} />
          </Table.Footer>
        </Table>
      )}
    </div>
  );
}
