"use client";

import { useState } from "react";
import { useClassifications, useCreateClassification, useDeactivateClassification } from "@/lib/hooks/use-metadata";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";

function CreateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<{ code: string; name: string; normal_side: "debit" | "credit"; is_system: boolean }>({ code: "", name: "", normal_side: "debit", is_system: false });
  const mutation = useCreateClassification();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" />}>Create</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create Classification</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label>Code</Label>
            <Input value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="main_wallet" />
          </div>
          <div className="grid gap-2">
            <Label>Name</Label>
            <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Main Wallet" />
          </div>
          <div className="grid gap-2">
            <Label>Normal Side</Label>
            <Select value={form.normal_side} onValueChange={(v) => { if (v) setForm({ ...form, normal_side: v as "debit" | "credit" }); }}>
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="debit">Debit</SelectItem>
                <SelectItem value="credit">Credit</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => mutation.mutate(form, { onSuccess: () => setOpen(false) })} disabled={mutation.isPending || !form.code || !form.name}>
            {mutation.isPending ? "Creating..." : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function ClassificationsPage() {
  const { data, isLoading } = useClassifications();
  const deactivateMutation = useDeactivateClassification();
  const classifications = Array.isArray(data) ? data : [];

  return (
    <div className="space-y-6">
      <PageHeader title="Classifications" description="Account classification definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <div className="space-y-2">{Array.from({ length: 5 }).map((_, i) => <div key={i} className="h-10 animate-pulse rounded bg-muted" />)}</div>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Code</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Normal Side</TableHead>
              <TableHead>System</TableHead>
              <TableHead>Active</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {classifications.map((c) => (
              <TableRow key={c.id}>
                <TableCell>{c.id}</TableCell>
                <TableCell className="font-mono text-xs">{c.code}</TableCell>
                <TableCell>{c.name}</TableCell>
                <TableCell><StatusBadge status={c.normal_side === "debit" ? "confirmed" : "failed"} /></TableCell>
                <TableCell>{c.is_system ? "Yes" : "No"}</TableCell>
                <TableCell><StatusBadge status={c.is_active ? "active" : "expired"} /></TableCell>
                <TableCell>
                  {c.is_active && (
                    <Button size="sm" variant="ghost" onClick={() => deactivateMutation.mutate(c.id)} disabled={deactivateMutation.isPending}>
                      Deactivate
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
