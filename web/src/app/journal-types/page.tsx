"use client";

import { useState } from "react";
import { useJournalTypes, useCreateJournalType, useDeactivateJournalType } from "@/lib/hooks/use-metadata";
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

function CreateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ code: "", name: "" });
  const mutation = useCreateJournalType();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" />}>Create</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create Journal Type</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label>Code</Label>
            <Input value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="deposit" />
          </div>
          <div className="grid gap-2">
            <Label>Name</Label>
            <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Deposit Confirmation" />
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

export default function JournalTypesPage() {
  const { data, isLoading } = useJournalTypes();
  const deactivateMutation = useDeactivateJournalType();
  const types = Array.isArray(data) ? data : [];

  return (
    <div className="space-y-6">
      <PageHeader title="Journal Types" description="Journal type definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <div className="space-y-2">{Array.from({ length: 5 }).map((_, i) => <div key={i} className="h-10 animate-pulse rounded bg-muted" />)}</div>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Code</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Active</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {types.map((t) => (
              <TableRow key={t.id}>
                <TableCell>{t.id}</TableCell>
                <TableCell className="font-mono text-xs">{t.code}</TableCell>
                <TableCell>{t.name}</TableCell>
                <TableCell><StatusBadge status={t.is_active ? "active" : "expired"} /></TableCell>
                <TableCell className="text-xs text-muted-foreground">{new Date(t.created_at).toLocaleDateString()}</TableCell>
                <TableCell>
                  {t.is_active && (
                    <Button size="sm" variant="ghost" onClick={() => deactivateMutation.mutate(t.id)} disabled={deactivateMutation.isPending}>
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
