"use client";

import { useState } from "react";
import {
  useTemplates, useCreateTemplate, useDeactivateTemplate, usePreviewTemplate,
  useClassifications, useCurrencies, useJournalTypes,
} from "../../hooks/use-metadata";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "../ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "../ui/select";
import { FileCode2 } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";
import { PaginationBar } from "../pagination-bar";
import { useClientPage } from "../../lib/use-client-page";
import type { PreviewResult } from "../../client/types";

interface LineForm {
  _id: string;
  classification_uid: string;
  entry_type: "debit" | "credit";
  holder_role: "user" | "system";
  amount_key: string;
  sort_order: number;
}

function CreateTemplateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ code: "", name: "", journal_type_uid: "" });
  const [lines, setLines] = useState<LineForm[]>([
    { _id: crypto.randomUUID(), classification_uid: "", entry_type: "debit", holder_role: "user", amount_key: "amount", sort_order: 1 },
    { _id: crypto.randomUUID(), classification_uid: "", entry_type: "credit", holder_role: "system", amount_key: "amount", sort_order: 2 },
  ]);
  const mutation = useCreateTemplate();
  const { data: journalTypes } = useJournalTypes(true);
  const { data: classifications } = useClassifications(true);

  function addLine() {
    setLines([...lines, {
      _id: crypto.randomUUID(),
      classification_uid: "",
      entry_type: "debit",
      holder_role: "user",
      amount_key: "amount",
      sort_order: lines.length + 1,
    }]);
  }

  function updateLine(idx: number, patch: Partial<LineForm>) {
    setLines(lines.map((l, i) => (i === idx ? { ...l, ...patch } : l)));
  }

  function removeLine(idx: number) {
    setLines(lines.filter((_, i) => i !== idx));
  }

  function handleSubmit() {
    const journalTypeUid = form.journal_type_uid.trim();
    if (journalTypeUid === "") {
      toast.error("Journal Type UID is required");
      return;
    }
    mutation.mutate(
      {
        code: form.code,
        name: form.name,
        journal_type_uid: journalTypeUid,
        lines: lines.map((l) => {
          return {
            classification_uid: l.classification_uid.trim(),
            entry_type: l.entry_type,
            holder_role: l.holder_role,
            amount_key: l.amount_key,
            sort_order: l.sort_order,
          };
        }),
      },
      {
        onSuccess: () => {
          toast.success("Template created");
          setOpen(false);
        },
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" />}>Create Template</DialogTrigger>
      <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Create Entry Template</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid grid-cols-3 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="tpl-code">Code</Label>
              <Input id="tpl-code" value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="deposit_confirm" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="tpl-name">Name</Label>
              <Input id="tpl-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Confirm Deposit" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="tpl-jtype">Journal Type</Label>
              <Select
                value={form.journal_type_uid === "" ? null : form.journal_type_uid}
                onValueChange={(v) => { if (typeof v === "string") setForm({ ...form, journal_type_uid: v }); }}
              >
                <SelectTrigger id="tpl-jtype" className="w-full">
                  <SelectValue placeholder="Select" />
                </SelectTrigger>
                <SelectContent>
                  {(journalTypes ?? []).map((t) => (
                    <SelectItem key={t.uid} value={t.uid}>{t.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <Label>Template Lines</Label>
              <Button size="sm" variant="outline" onClick={addLine}>+ Add Line</Button>
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div>
                <p className="text-xs font-medium text-green-400 mb-2">DEBIT SIDE</p>
                {lines.map((l, idx) => l.entry_type !== "debit" ? null : (
                  <div key={l._id} className="mb-2 rounded border border-green-500/20 bg-green-500/5 p-3 space-y-2">
                    <div className="flex gap-2">
                      <Select value={l.classification_uid === "" ? null : l.classification_uid} onValueChange={(v) => { if (typeof v === "string") updateLine(idx, { classification_uid: v }); }}>
                        <SelectTrigger className="w-32"><SelectValue placeholder="Class" /></SelectTrigger>
                        <SelectContent>
                          {(classifications ?? []).map((c) => (
                            <SelectItem key={c.uid} value={c.uid}>{c.code}</SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                      <Select value={l.holder_role} onValueChange={(v) => { if (v) updateLine(idx, { holder_role: v as "user" | "system" }); }}>
                        <SelectTrigger className="w-24"><SelectValue /></SelectTrigger>
                        <SelectContent>
                          <SelectItem value="user">User</SelectItem>
                          <SelectItem value="system">System</SelectItem>
                        </SelectContent>
                      </Select>
                      <Input placeholder="amount_key" value={l.amount_key} onChange={(e) => updateLine(idx, { amount_key: e.target.value })} className="flex-1" />
                      <Button size="sm" variant="ghost" onClick={() => removeLine(idx)} aria-label="Remove line">&times;</Button>
                    </div>
                  </div>
                ))}
              </div>
              <div>
                <p className="text-xs font-medium text-red-400 mb-2">CREDIT SIDE</p>
                {lines.map((l, idx) => l.entry_type !== "credit" ? null : (
                  <div key={l._id} className="mb-2 rounded border border-red-500/20 bg-red-500/5 p-3 space-y-2">
                    <div className="flex gap-2">
                      <Select value={l.classification_uid === "" ? null : l.classification_uid} onValueChange={(v) => { if (typeof v === "string") updateLine(idx, { classification_uid: v }); }}>
                        <SelectTrigger className="w-32"><SelectValue placeholder="Class" /></SelectTrigger>
                        <SelectContent>
                          {(classifications ?? []).map((c) => (
                            <SelectItem key={c.uid} value={c.uid}>{c.code}</SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                      <Select value={l.holder_role} onValueChange={(v) => { if (v) updateLine(idx, { holder_role: v as "user" | "system" }); }}>
                        <SelectTrigger className="w-24"><SelectValue /></SelectTrigger>
                        <SelectContent>
                          <SelectItem value="user">User</SelectItem>
                          <SelectItem value="system">System</SelectItem>
                        </SelectContent>
                      </Select>
                      <Input placeholder="amount_key" value={l.amount_key} onChange={(e) => updateLine(idx, { amount_key: e.target.value })} className="flex-1" />
                      <Button size="sm" variant="ghost" onClick={() => removeLine(idx)} aria-label="Remove line">&times;</Button>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button onClick={handleSubmit} disabled={mutation.isPending || !form.code || !form.name}>
            {mutation.isPending ? "Creating..." : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeactivateDialog({ id, name }: { id: string; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateTemplate();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Deactivate</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deactivate &quot;{name}&quot;</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground py-4">
          This template will be marked inactive and can no longer be used for new journals.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate(id, {
              onSuccess: () => {
                toast.success("Template deactivated");
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

function PreviewSection({ code }: { code: string }) {
  const [params, setParams] = useState({ holder_id: "", currency_uid: "", amount: "" });
  const previewMutation = usePreviewTemplate();
  const preview = previewMutation.data as PreviewResult | undefined;
  const { data: currencies } = useCurrencies(true);

  return (
    <div className="space-y-2 mt-2">
      <div className="flex gap-2">
        <Input placeholder="Holder ID" value={params.holder_id} onChange={(e) => setParams({ ...params, holder_id: e.target.value })} className="w-28" />
        <Select value={params.currency_uid === "" ? null : params.currency_uid} onValueChange={(v) => { if (typeof v === "string") setParams({ ...params, currency_uid: v }); }}>
          <SelectTrigger className="w-28"><SelectValue placeholder="Currency" /></SelectTrigger>
          <SelectContent>
            {(currencies ?? []).map((c) => (
              <SelectItem key={c.uid} value={c.uid}>{c.code}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input placeholder="Amount" value={params.amount} onChange={(e) => setParams({ ...params, amount: e.target.value })} className="w-28" />
        <Button
          size="sm"
          variant="outline"
          onClick={() =>
            previewMutation.mutate({
              code,
              holder_id: parseInt(params.holder_id, 10),
              currency_uid: params.currency_uid.trim(),
              amount: params.amount,
            })
          }
          disabled={previewMutation.isPending}
        >
          Preview
        </Button>
      </div>
      {preview && (
        <div className="rounded bg-muted p-3 text-xs font-mono">
          <p>Total Debit: {preview.total_debit} | Total Credit: {preview.total_credit}</p>
          {preview.entries.map((e, i) => (
            <p key={i}>
              {e.entry_type.toUpperCase()} holder={e.account_holder} class={e.classification_uid} cur={e.currency_uid} amt={e.amount}
            </p>
          ))}
        </div>
      )}
    </div>
  );
}

export function TemplatesPage() {
  const { data, isLoading, isError } = useTemplates();
  const templates = Array.isArray(data) ? data : [];
  const { pageItems, page, pageCount, setPage } = useClientPage(templates);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  return (
    <div className="space-y-6">
      <PageHeader title="Templates" description="Entry template definitions" actions={<CreateTemplateDialog />} />

      {isLoading ? (
        <TableSkeleton rows={3} />
      ) : isError ? (
        <ErrorState message="Failed to load templates" />
      ) : templates.length === 0 ? (
        <EmptyState
          icon={FileCode2}
          title="No templates yet"
          description="Create your first template to define reusable journal recipes."
        />
      ) : (
        <div className="space-y-4">
          {pageItems.map((t) => (
            <Card key={t.uid}>
              <CardHeader className="pb-3">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-3">
                    <CardTitle className="text-base">{t.name}</CardTitle>
                    <span className="font-mono text-xs text-muted-foreground">{t.code}</span>
                    <StatusBadge status={t.is_active ? "active" : "inactive"} />
                  </div>
                  <div className="flex gap-2">
                    <Button size="sm" variant="outline" onClick={() => setExpandedId(expandedId === t.uid ? null : t.uid)}>
                      {expandedId === t.uid ? "Collapse" : "Preview"}
                    </Button>
                    {t.is_active && <DeactivateDialog id={t.uid} name={t.name} />}
                  </div>
                </div>
              </CardHeader>
              <CardContent>
                <div className="grid grid-cols-2 gap-4">
                  <div>
                    <p className="text-xs font-medium text-green-400 mb-1">DEBIT</p>
                    {t.lines.filter((l) => l.entry_type === "debit").map((l) => (
                      <div key={l.sort_order} className="text-xs text-muted-foreground">
                        Class {l.classification_uid} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                  <div>
                    <p className="text-xs font-medium text-red-400 mb-1">CREDIT</p>
                    {t.lines.filter((l) => l.entry_type === "credit").map((l) => (
                      <div key={l.sort_order} className="text-xs text-muted-foreground">
                        Class {l.classification_uid} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                </div>
                {expandedId === t.uid && <PreviewSection code={t.code} />}
              </CardContent>
            </Card>
          ))}
          <PaginationBar page={page} pageCount={pageCount} onPageChange={setPage} />
        </div>
      )}
    </div>
  );
}
