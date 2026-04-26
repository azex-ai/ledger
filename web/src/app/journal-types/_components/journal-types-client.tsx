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
import { FileType2 } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import { EmptyState } from "@/components/empty-state";
import { TableSkeleton } from "@/components/loading-skeleton";

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
            <Label htmlFor="jt-code">Code</Label>
            <Input id="jt-code" value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="deposit" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="jt-name">Name</Label>
            <Input id="jt-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Deposit Confirmation" />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => mutation.mutate(form, {
            onSuccess: () => {
              toast.success("Journal type created");
              setOpen(false);
            },
          })} disabled={mutation.isPending || !form.code || !form.name}>
            {mutation.isPending ? "Creating..." : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeactivateDialog({ id, name }: { id: number; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateJournalType();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Deactivate</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deactivate &quot;{name}&quot;</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground py-4">
          This journal type will be marked inactive. Existing journals using it will be unaffected.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate(id, {
              onSuccess: () => {
                toast.success("Journal type deactivated");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending}
          >
            {mutation.isPending ? "Deactivating..." : "Deactivate"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function JournalTypesClient() {
  const { data, isLoading, isError } = useJournalTypes();
  const types = Array.isArray(data) ? data : [];

  return (
    <div className="space-y-6">
      <PageHeader title="Journal Types" description="Journal type definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load journal types" />
      ) : types.length === 0 ? (
        <EmptyState
          icon={FileType2}
          title="No journal types yet"
          description="Create your first journal type to get started."
        />
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
                <TableCell><StatusBadge status={t.is_active ? "active" : "inactive"} /></TableCell>
                <TableCell className="text-xs text-muted-foreground">{new Date(t.created_at).toLocaleDateString()}</TableCell>
                <TableCell>
                  {t.is_active && <DeactivateDialog id={t.id} name={t.name} />}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
