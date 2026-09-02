"use client";

import { errorText, apiFieldErrors } from "../../lib/error-message";
import { useState } from "react";
import {
  useJournalTypes,
  useCreateJournalType,
  useDeactivateJournalType,
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
import { FileType2 } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";
import { PaginationBar } from "../pagination-bar";
import { useClientPage } from "../../lib/use-client-page";

function CreateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ code: "", name: "" });
  // J-8 (2026-09-02 web audit): see ClassificationsPage's matching comment.
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
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
            <Input
              id="jt-code"
              value={form.code}
              onChange={(e) => {
                setForm({ ...form, code: e.target.value });
                setFieldErrors(({ code: _code, ...rest }) => rest);
              }}
              placeholder="deposit"
              aria-invalid={fieldErrors.code ? true : undefined}
            />
            {fieldErrors.code && <p className="text-xs text-destructive">{fieldErrors.code}</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="jt-name">Name</Label>
            <Input
              id="jt-name"
              value={form.name}
              onChange={(e) => {
                setForm({ ...form, name: e.target.value });
                setFieldErrors(({ name: _name, ...rest }) => rest);
              }}
              placeholder="Deposit Confirmation"
              aria-invalid={fieldErrors.name ? true : undefined}
            />
            {fieldErrors.name && <p className="text-xs text-destructive">{fieldErrors.name}</p>}
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => mutation.mutate(form, {
            onSuccess: () => {
              toast.success("Journal type created");
              setOpen(false);
            },
            onError: (err) => {
              const fields = apiFieldErrors(err);
              if (Object.keys(fields).length > 0) {
                setFieldErrors(fields);
              } else {
                toast.error(errorText(err, "Failed to create journal type"));
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
              onError: (err) => toast.error(errorText(err, "Failed to deactivate journal type")),
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

export function JournalTypesPage() {
  const { data, isLoading, isError, refetch } = useJournalTypes();
  const types = Array.isArray(data) ? data : [];
  const { pageItems, page, pageCount, setPage } = useClientPage(types);

  return (
    <div className="space-y-6">
      <PageHeader title="Journal Types" description="Journal type definitions" actions={<CreateDialog />} />

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load journal types" onRetry={refetch} />
      ) : types.length === 0 ? (
        <EmptyState
          icon={FileType2}
          title="No journal types yet"
          description="Create your first journal type to get started."
        />
      ) : (
        <>
        <Table aria-label="Journal types" className="min-w-[820px]">
          <TableHeader>
            <TableRow>
              <TableHead className="w-[220px]">ID</TableHead>
              <TableHead>Code</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Active</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageItems.map((t) => (
              <TableRow key={t.uid}>
                <TableCell className="max-w-[220px]">
                  <span className="block truncate font-mono text-xs" title={t.uid}>{t.uid}</span>
                </TableCell>
                <TableCell className="font-mono text-xs">{t.code}</TableCell>
                <TableCell>{t.name}</TableCell>
                <TableCell><StatusBadge status={t.is_active ? "active" : "inactive"} /></TableCell>
                <TableCell className="text-xs text-muted-foreground">{new Date(t.created_at).toLocaleDateString()}</TableCell>
                <TableCell>
                  {t.is_active && <DeactivateDialog id={t.uid} name={t.name} />}
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
