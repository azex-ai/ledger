"use client";

import { useState } from "react";
import { useCurrencies, useCreateCurrency } from "@/lib/hooks/use-metadata";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import { Coins } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import { EmptyState } from "@/components/empty-state";
import { TableSkeleton } from "@/components/loading-skeleton";

function CreateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ code: "", name: "" });
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
        </div>
        <DialogFooter>
          <Button onClick={() => mutation.mutate(form, {
            onSuccess: () => {
              toast.success("Currency created");
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

export function CurrenciesClient() {
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
            </TableRow>
          </TableHeader>
          <TableBody>
            {currencies.map((c) => (
              <TableRow key={c.id}>
                <TableCell>{c.id}</TableCell>
                <TableCell className="font-mono">{c.code}</TableCell>
                <TableCell>{c.name}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
