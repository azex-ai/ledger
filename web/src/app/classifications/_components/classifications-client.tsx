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
import { AlertCircle, Tags } from "lucide-react";
import { toast } from "sonner";

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
          <Button onClick={() => mutation.mutate(form, {
            onSuccess: () => {
              toast.success("Classification created");
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
  const mutation = useDeactivateClassification();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Deactivate</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deactivate &quot;{name}&quot;</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground py-4">
          This classification will be marked inactive. Existing entries referencing it will be unaffected.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate(id, {
              onSuccess: () => {
                toast.success("Classification deactivated");
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

export function ClassificationsClient() {
  const { data, isLoading, isError } = useClassifications();
  const classifications = Array.isArray(data) ? data : [];

  return (
    <div className="space-y-6">
      <PageHeader title="Classifications" description="Account classification definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <div className="space-y-2">{Array.from({ length: 5 }).map((_, i) => <div key={i} className="h-10 animate-pulse rounded bg-muted" />)}</div>
      ) : isError ? (
        <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-8 text-center">
          <AlertCircle className="mx-auto h-8 w-8 text-destructive mb-2" />
          <p className="text-sm font-medium">Failed to load classifications</p>
        </div>
      ) : classifications.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-12 text-center">
          <Tags className="mx-auto h-8 w-8 text-muted-foreground mb-2" />
          <p className="text-sm font-medium">No classifications yet</p>
          <p className="text-xs text-muted-foreground mt-1">Create your first classification to get started.</p>
        </div>
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
                <TableCell><StatusBadge status={c.normal_side} /></TableCell>
                <TableCell>{c.is_system ? "Yes" : "No"}</TableCell>
                <TableCell><StatusBadge status={c.is_active ? "active" : "inactive"} /></TableCell>
                <TableCell>
                  {c.is_active && <DeactivateDialog id={c.id} name={c.name} />}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
