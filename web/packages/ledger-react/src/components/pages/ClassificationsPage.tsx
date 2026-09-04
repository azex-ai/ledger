"use client";

import { errorText, apiFieldErrors } from "../../lib/error-message";
import { useState } from "react";
import {
  useClassifications,
  useCreateClassification,
  useDeactivateClassification,
} from "../../hooks/use-metadata";
import type { BalanceRole } from "../../client/types";
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
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "../ui/select";
import { Tags } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";
import { PaginationBar } from "../pagination-bar";
import { useClientPage } from "../../lib/use-client-page";

function CreateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<{ code: string; name: string; normal_side: "debit" | "credit"; is_system: boolean; balance_role: BalanceRole }>({ code: "", name: "", normal_side: "debit", is_system: false, balance_role: "available" });
  // J-8 (2026-09-02 web audit): server-side field-level validation errors
  // (api-contract.md §1's message.fields — e.g. a duplicate `code`) used to
  // collapse into the same generic toast as any other error. No
  // react-hook-form in this codebase, so a sibling useState carries them;
  // cleared per-field on the next edit so a stale error doesn't linger next
  // to a since-corrected value.
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
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
            <Input
              id="cls-code"
              value={form.code}
              onChange={(e) => {
                setForm({ ...form, code: e.target.value });
                setFieldErrors(({ code: _code, ...rest }) => rest);
              }}
              placeholder="main_wallet"
              aria-invalid={fieldErrors.code ? true : undefined}
            />
            {fieldErrors.code && <p className="text-xs text-destructive">{fieldErrors.code}</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cls-name">Name</Label>
            <Input
              id="cls-name"
              value={form.name}
              onChange={(e) => {
                setForm({ ...form, name: e.target.value });
                setFieldErrors(({ name: _name, ...rest }) => rest);
              }}
              placeholder="Main Wallet"
              aria-invalid={fieldErrors.name ? true : undefined}
            />
            {fieldErrors.name && <p className="text-xs text-destructive">{fieldErrors.name}</p>}
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
          <div className="grid gap-2">
            <Label htmlFor="cls-balance-role">Balance Role</Label>
            <Select value={form.balance_role} onValueChange={(v) => { if (v) setForm({ ...form, balance_role: v as BalanceRole }); }}>
              <SelectTrigger id="cls-balance-role"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="available">Available — spendable funds</SelectItem>
                <SelectItem value="pending">Pending — inbound, awaiting confirmation</SelectItem>
                <SelectItem value="locked">Locked — held for an in-flight withdrawal</SelectItem>
                <SelectItem value="memo">Memo — cost / memo account, not a liability</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">How this classification counts in a holder&apos;s balance breakdown. Required by the server.</p>
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => mutation.mutate(form, {
            onSuccess: () => {
              toast.success("Classification created");
              setOpen(false);
            },
            onError: (err) => {
              const fields = apiFieldErrors(err);
              if (Object.keys(fields).length > 0) {
                setFieldErrors(fields);
              } else {
                toast.error(errorText(err, "Failed to create classification"));
              }
            },
          })} disabled={mutation.isPending || !form.code || !form.name}>
            {mutation.isPending ? "Creating..." : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeactivateDialog({ id, name }: { id: string; name: string }) {
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
              onError: (err) => toast.error(errorText(err, "Failed to deactivate classification")),
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

export function ClassificationsPage() {
  const { data, isLoading, isError, refetch } = useClassifications();
  const classifications = Array.isArray(data) ? data : [];
  const { pageItems, page, pageCount, setPage } = useClientPage(classifications);

  return (
    <div className="space-y-6">
      <PageHeader title="Classifications" description="Account classification definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load classifications" onRetry={refetch} />
      ) : classifications.length === 0 ? (
        <EmptyState
          icon={Tags}
          title="No classifications yet"
          description="Create your first classification to get started."
        />
      ) : (
        <>
        <Table aria-label="Classifications" className="min-w-[820px]">
          <TableHeader>
            <TableRow>
              <TableHead className="w-[220px]">ID</TableHead>
              <TableHead>Code</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Normal Side</TableHead>
              <TableHead>Balance Role</TableHead>
              <TableHead>System</TableHead>
              <TableHead>Active</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageItems.map((c) => (
              <TableRow key={c.uid}>
                <TableCell className="max-w-[220px]">
                  <span className="block truncate font-mono text-xs" title={c.uid}>{c.uid}</span>
                </TableCell>
                <TableCell className="font-mono text-xs">{c.code}</TableCell>
                <TableCell>{c.name}</TableCell>
                <TableCell><StatusBadge status={c.normal_side} /></TableCell>
                <TableCell className="text-xs">{c.balance_role || "—"}</TableCell>
                <TableCell>{c.is_system ? "Yes" : "No"}</TableCell>
                <TableCell><StatusBadge status={c.is_active ? "active" : "inactive"} /></TableCell>
                <TableCell>
                  {c.is_active && <DeactivateDialog id={c.uid} name={c.name} />}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <PaginationBar page={page} pageCount={pageCount} onPageChange={setPage} />
        </>
      )}
    </div>
  );
}
