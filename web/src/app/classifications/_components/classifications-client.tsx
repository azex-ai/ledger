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
import { Tags } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import { EmptyState } from "@/components/empty-state";
import { TableSkeleton } from "@/components/loading-skeleton";

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
            <Label htmlFor="cls-code">Code</Label>
            <Input id="cls-code" value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="main_wallet" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cls-name">Name</Label>
            <Input id="cls-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Main Wallet" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cls-normal-side">Normal Side</Label>
            <Select value={form.normal_side} onValueChange={(v) => { if (v) setForm({ ...form, normal_side: v as "debit" | "credit" }); }}>
              <SelectTrigger id="cls-normal-side"><SelectValue /></SelectTrigger>
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
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load classifications" />
      ) : classifications.length === 0 ? (
        <EmptyState
          icon={Tags}
          title="No classifications yet"
          description="Create your first classification to get started."
        />
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
