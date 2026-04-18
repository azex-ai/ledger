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
            <Label>Code</Label>
            <Input value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="USDT" />
          </div>
          <div className="grid gap-2">
            <Label>Name</Label>
            <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Tether USD" />
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

export default function CurrenciesPage() {
  const { data, isLoading } = useCurrencies();
  const currencies = Array.isArray(data) ? data : [];

  return (
    <div className="space-y-6">
      <PageHeader title="Currencies" description="Supported currency definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <div className="space-y-2">{Array.from({ length: 3 }).map((_, i) => <div key={i} className="h-10 animate-pulse rounded bg-muted" />)}</div>
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
