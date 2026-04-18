"use client";

import { useState } from "react";
import {
  useTemplates, useCreateTemplate, useDeactivateTemplate, usePreviewTemplate,
} from "@/lib/hooks/use-metadata";
import { useClassifications } from "@/lib/hooks/use-metadata";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import type { PreviewResult } from "@/lib/api";

interface LineForm {
  classification_id: string;
  entry_type: "debit" | "credit";
  holder_role: "user" | "system";
  amount_key: string;
  sort_order: number;
}

function CreateTemplateDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ code: "", name: "", journal_type_id: "" });
  const [lines, setLines] = useState<LineForm[]>([
    { classification_id: "", entry_type: "debit", holder_role: "user", amount_key: "amount", sort_order: 1 },
    { classification_id: "", entry_type: "credit", holder_role: "system", amount_key: "amount", sort_order: 2 },
  ]);
  const mutation = useCreateTemplate();

  function addLine() {
    setLines([...lines, {
      classification_id: "",
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
    mutation.mutate(
      {
        code: form.code,
        name: form.name,
        journal_type_id: parseInt(form.journal_type_id),
        lines: lines.map((l) => ({
          classification_id: parseInt(l.classification_id),
          entry_type: l.entry_type,
          holder_role: l.holder_role,
          amount_key: l.amount_key,
          sort_order: l.sort_order,
        })),
      },
      { onSuccess: () => setOpen(false) },
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
              <Label>Code</Label>
              <Input value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })} placeholder="deposit_confirm" />
            </div>
            <div className="grid gap-2">
              <Label>Name</Label>
              <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Confirm Deposit" />
            </div>
            <div className="grid gap-2">
              <Label>Journal Type ID</Label>
              <Input value={form.journal_type_id} onChange={(e) => setForm({ ...form, journal_type_id: e.target.value })} placeholder="1" />
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
                {lines.filter((l) => l.entry_type === "debit").map((l) => {
                  const idx = lines.indexOf(l);
                  return (
                    <div key={idx} className="mb-2 rounded border border-green-500/20 bg-green-500/5 p-3 space-y-2">
                      <div className="flex gap-2">
                        <Input placeholder="Class ID" value={l.classification_id} onChange={(e) => updateLine(idx, { classification_id: e.target.value })} className="w-24" />
                        <Select value={l.holder_role} onValueChange={(v) => { if (v) updateLine(idx, { holder_role: v as "user" | "system" }); }}>
                          <SelectTrigger className="w-24"><SelectValue /></SelectTrigger>
                          <SelectContent>
                            <SelectItem value="user">User</SelectItem>
                            <SelectItem value="system">System</SelectItem>
                          </SelectContent>
                        </Select>
                        <Input placeholder="amount_key" value={l.amount_key} onChange={(e) => updateLine(idx, { amount_key: e.target.value })} className="flex-1" />
                        <Button size="sm" variant="ghost" onClick={() => removeLine(idx)}>x</Button>
                      </div>
                    </div>
                  );
                })}
              </div>
              <div>
                <p className="text-xs font-medium text-red-400 mb-2">CREDIT SIDE</p>
                {lines.filter((l) => l.entry_type === "credit").map((l) => {
                  const idx = lines.indexOf(l);
                  return (
                    <div key={idx} className="mb-2 rounded border border-red-500/20 bg-red-500/5 p-3 space-y-2">
                      <div className="flex gap-2">
                        <Input placeholder="Class ID" value={l.classification_id} onChange={(e) => updateLine(idx, { classification_id: e.target.value })} className="w-24" />
                        <Select value={l.holder_role} onValueChange={(v) => { if (v) updateLine(idx, { holder_role: v as "user" | "system" }); }}>
                          <SelectTrigger className="w-24"><SelectValue /></SelectTrigger>
                          <SelectContent>
                            <SelectItem value="user">User</SelectItem>
                            <SelectItem value="system">System</SelectItem>
                          </SelectContent>
                        </Select>
                        <Input placeholder="amount_key" value={l.amount_key} onChange={(e) => updateLine(idx, { amount_key: e.target.value })} className="flex-1" />
                        <Button size="sm" variant="ghost" onClick={() => removeLine(idx)}>x</Button>
                      </div>
                    </div>
                  );
                })}
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

function PreviewSection({ code }: { code: string }) {
  const [params, setParams] = useState({ holder_id: "", currency_id: "", amount: "" });
  const previewMutation = usePreviewTemplate();
  const preview = previewMutation.data as PreviewResult | undefined;

  return (
    <div className="space-y-2 mt-2">
      <div className="flex gap-2">
        <Input placeholder="Holder ID" value={params.holder_id} onChange={(e) => setParams({ ...params, holder_id: e.target.value })} className="w-28" />
        <Input placeholder="Currency ID" value={params.currency_id} onChange={(e) => setParams({ ...params, currency_id: e.target.value })} className="w-28" />
        <Input placeholder="Amount" value={params.amount} onChange={(e) => setParams({ ...params, amount: e.target.value })} className="w-28" />
        <Button
          size="sm"
          variant="outline"
          onClick={() =>
            previewMutation.mutate({
              code,
              holder_id: parseInt(params.holder_id),
              currency_id: parseInt(params.currency_id),
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
              {e.entry_type.toUpperCase()} holder={e.account_holder} class={e.classification_id} cur={e.currency_id} amt={e.amount}
            </p>
          ))}
        </div>
      )}
    </div>
  );
}

export default function TemplatesPage() {
  const { data, isLoading } = useTemplates();
  const deactivateMutation = useDeactivateTemplate();
  const templates = Array.isArray(data) ? data : [];
  const [expandedId, setExpandedId] = useState<number | null>(null);

  return (
    <div className="space-y-6">
      <PageHeader title="Templates" description="Entry template definitions" actions={<CreateTemplateDialog />} />

      {isLoading ? (
        <div className="space-y-2">{Array.from({ length: 3 }).map((_, i) => <div key={i} className="h-20 animate-pulse rounded bg-muted" />)}</div>
      ) : templates.length === 0 ? (
        <p className="text-sm text-muted-foreground">No templates yet</p>
      ) : (
        <div className="space-y-4">
          {templates.map((t) => (
            <Card key={t.id}>
              <CardHeader className="pb-3">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-3">
                    <CardTitle className="text-base">{t.name}</CardTitle>
                    <span className="font-mono text-xs text-muted-foreground">{t.code}</span>
                    <StatusBadge status={t.is_active ? "active" : "expired"} />
                  </div>
                  <div className="flex gap-2">
                    <Button size="sm" variant="outline" onClick={() => setExpandedId(expandedId === t.id ? null : t.id)}>
                      {expandedId === t.id ? "Collapse" : "Preview"}
                    </Button>
                    {t.is_active && (
                      <Button size="sm" variant="ghost" onClick={() => deactivateMutation.mutate(t.id)} disabled={deactivateMutation.isPending}>
                        Deactivate
                      </Button>
                    )}
                  </div>
                </div>
              </CardHeader>
              <CardContent>
                <div className="grid grid-cols-2 gap-4">
                  <div>
                    <p className="text-xs font-medium text-green-400 mb-1">DEBIT</p>
                    {t.lines.filter((l) => l.entry_type === "debit").map((l) => (
                      <div key={l.id} className="text-xs text-muted-foreground">
                        Class {l.classification_id} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                  <div>
                    <p className="text-xs font-medium text-red-400 mb-1">CREDIT</p>
                    {t.lines.filter((l) => l.entry_type === "credit").map((l) => (
                      <div key={l.id} className="text-xs text-muted-foreground">
                        Class {l.classification_id} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                </div>
                {expandedId === t.id && <PreviewSection code={t.code} />}
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
