"use client";

import { useState } from "react";
import { useSnapshots } from "@/lib/hooks/use-system";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";

export default function SnapshotsPage() {
  const [form, setForm] = useState({
    holder: "",
    currency_id: "",
    start: "",
    end: "",
  });
  const [query, setQuery] = useState<{
    holder?: number;
    currency_id?: number;
    start?: string;
    end?: string;
  }>({});

  const { data, isLoading } = useSnapshots(query);
  const snapshots = data?.data ?? [];

  function handleSearch() {
    setQuery({
      holder: form.holder ? parseInt(form.holder) : undefined,
      currency_id: form.currency_id ? parseInt(form.currency_id) : undefined,
      start: form.start || undefined,
      end: form.end || undefined,
    });
  }

  return (
    <div className="space-y-6">
      <PageHeader title="Snapshots" description="Historical balance snapshots" />

      <div className="flex flex-wrap gap-3 items-end">
        <div className="grid gap-1">
          <Label className="text-xs">Holder</Label>
          <Input value={form.holder} onChange={(e) => setForm({ ...form, holder: e.target.value })} placeholder="1001" className="w-28" />
        </div>
        <div className="grid gap-1">
          <Label className="text-xs">Currency</Label>
          <Input value={form.currency_id} onChange={(e) => setForm({ ...form, currency_id: e.target.value })} placeholder="1" className="w-28" />
        </div>
        <div className="grid gap-1">
          <Label className="text-xs">Start Date</Label>
          <Input type="date" value={form.start} onChange={(e) => setForm({ ...form, start: e.target.value })} className="w-40" />
        </div>
        <div className="grid gap-1">
          <Label className="text-xs">End Date</Label>
          <Input type="date" value={form.end} onChange={(e) => setForm({ ...form, end: e.target.value })} className="w-40" />
        </div>
        <Button onClick={handleSearch}>Search</Button>
      </div>

      {isLoading ? (
        <div className="space-y-2">{Array.from({ length: 5 }).map((_, i) => <div key={i} className="h-10 animate-pulse rounded bg-muted" />)}</div>
      ) : snapshots.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          {Object.keys(query).length === 0 ? "Enter search criteria to view snapshots" : "No snapshots found"}
        </p>
      ) : (
        <Table>
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
            {snapshots.map((s, i) => (
              <TableRow key={i}>
                <TableCell>{s.snapshot_date}</TableCell>
                <TableCell>{s.account_holder}</TableCell>
                <TableCell>{s.currency_id}</TableCell>
                <TableCell>{s.classification_id}</TableCell>
                <TableCell className="text-right font-mono">{s.balance}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
