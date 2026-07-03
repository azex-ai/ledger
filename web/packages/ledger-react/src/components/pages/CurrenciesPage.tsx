"use client";

import { useState } from "react";
import {
  useCurrencies,
  useCreateCurrency,
  useDeactivateCurrency,
} from "../../hooks/use-metadata";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "../ui/dialog";
import { Coins } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";

function CreateDialog() {
  const [open, setOpen] = useState(false);
  // exponent kept as string while typing so the field can be empty; "0" is a
  // legal value (JPY) and must stay distinguishable from "not filled in".
  const [form, setForm] = useState({ code: "", name: "", exponent: "" });
  const mutation = useCreateCurrency();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" />}>Create</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create Currency</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="cur-code">Code</Label>
            <Input id="cur-code" value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="USDT" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cur-name">Name</Label>
            <Input id="cur-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Tether USD" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cur-exponent">Decimal places (0-18)</Label>
            <Input id="cur-exponent" type="number" min={0} max={18} step={1} value={form.exponent} onChange={(e) => setForm({ ...form, exponent: e.target.value })} placeholder="e.g. 2 for USD, 0 for JPY, 18 for wei" />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => mutation.mutate({ code: form.code, name: form.name, exponent: Number(form.exponent) }, {
            onSuccess: () => {
              toast.success("Currency created");
              setOpen(false);
            },
          })} disabled={mutation.isPending || !form.code || !form.name || form.exponent.trim() === "" || Number.isNaN(Number(form.exponent)) || Number(form.exponent) < 0 || Number(form.exponent) > 18}>
            {mutation.isPending ? "Creating..." : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeactivateDialog({ id, name }: { id: number; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateCurrency();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Deactivate</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deactivate &quot;{name}&quot;</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground py-4">
          This currency will be marked inactive. Existing entries referencing it will be unaffected.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate(id, {
              onSuccess: () => {
                toast.success("Currency deactivated");
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

export function CurrenciesPage() {
  const { data, isLoading, isError } = useCurrencies();
  const currencies = Array.isArray(data) ? data : [];

  return (
    <div className="space-y-6">
      <PageHeader title="Currencies" description="Supported currency definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <TableSkeleton rows={3} />
      ) : isError ? (
        <ErrorState message="Failed to load currencies" />
      ) : currencies.length === 0 ? (
        <EmptyState
          icon={Coins}
          title="No currencies yet"
          description="Create your first currency to get started."
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Code</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Active</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {currencies.map((c) => (
              <TableRow key={c.id}>
                <TableCell>{c.id}</TableCell>
                <TableCell className="font-mono">{c.code}</TableCell>
                <TableCell>{c.name}</TableCell>
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
