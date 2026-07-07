"use client";

import { useState } from "react";
import { Button, Input, Label, Table, TextField } from "@heroui/react";
import { useSnapshots } from "../../hooks/use-system";
import { formatAmount } from "../../lib/utils";
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

  const { data, isLoading, isError } = useSnapshots(query);
  const snapshots = data ?? [];
  const { pageItems, page, pageCount, setPage } = useClientPage(snapshots);
  const hasSearched = Object.keys(query).length > 0;

  function handleSearch() {
    setQuery({
      holder: form.holder ? parseInt(form.holder, 10) : undefined,
      currency_uid: form.currency_uid ? form.currency_uid.trim() : undefined,
      start: form.start || undefined,
      end: form.end || undefined,
    });
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
        <ErrorState message="Failed to load snapshots" />
      ) : snapshots.length === 0 ? (
        <EmptyState
          title={hasSearched ? "No snapshots found" : "Enter search criteria to view snapshots"}
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
                      <Table.Cell>{s.currency_uid}</Table.Cell>
                      <Table.Cell>{s.classification_uid}</Table.Cell>
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
