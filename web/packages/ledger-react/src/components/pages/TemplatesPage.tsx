"use client";

import { errorText } from "../../lib/error-message";
import { addAmounts, formatAmount } from "../../lib/utils";
import { useState } from "react";
import {
  useTemplates, useCreateTemplate, useDeactivateTemplate, usePreviewTemplate,
  useClassifications, useCurrencies, useJournalTypes, useUidCodeLookups,
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
  // query-consumption-allow: populates the journal-type/classification <Select>s below; a failed fetch empties the dropdown, a self-evident degradation the user can see and retry — not a false claim like J-1/J-2/J-3.
  const { data: journalTypes } = useJournalTypes(true);
  // query-consumption-allow: same as journalTypes above.
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
        onError: (err) => toast.error(errorText(err, "Failed to create template")),
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
                      {/* J-12 (2026-09-02 web audit): unlabeled input — mirrors HeroUI's `aria-label="Amount key"`. */}
                      <Input aria-label="Amount key" placeholder="amount_key" value={l.amount_key} onChange={(e) => updateLine(idx, { amount_key: e.target.value })} className="min-w-0 flex-1" />
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
                      {/* J-12 (2026-09-02 web audit): unlabeled input — mirrors HeroUI's `aria-label="Amount key"`. */}
                      <Input aria-label="Amount key" placeholder="amount_key" value={l.amount_key} onChange={(e) => updateLine(idx, { amount_key: e.target.value })} className="min-w-0 flex-1" />
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
              onError: (err) => toast.error(errorText(err, "Failed to deactivate template")),
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
  // query-consumption-allow: populates the currency <Select> below; a failed fetch empties the dropdown, a self-evident degradation the user can see and retry — not a false claim like J-1/J-2/J-3.
  const { data: currencies } = useCurrencies(true);

  return (
    <div className="space-y-2 mt-2">
      <div className="flex gap-2">
        {/* J-12 (2026-09-02 web audit): unlabeled inputs — mirrors HeroUI's `aria-label`s on the same fields. */}
        <Input aria-label="Holder ID" placeholder="Holder ID" value={params.holder_id} onChange={(e) => setParams({ ...params, holder_id: e.target.value })} className="w-28" />
        <Select value={params.currency_uid === "" ? null : params.currency_uid} onValueChange={(v) => { if (typeof v === "string") setParams({ ...params, currency_uid: v }); }}>
          <SelectTrigger className="w-28"><SelectValue placeholder="Currency" /></SelectTrigger>
          <SelectContent>
            {(currencies ?? []).map((c) => (
              <SelectItem key={c.uid} value={c.uid}>{c.code}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input aria-label="Amount" placeholder="Amount" value={params.amount} onChange={(e) => setParams({ ...params, amount: e.target.value })} className="w-28" />
        <Button
          size="sm"
          variant="outline"
          onClick={() =>
            // mutation-feedback-allow: inline isError rendered below (J-20)
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
      {previewMutation.isError && (
        // J-20 (2026-09-02 web audit): this mutation had no onError and no
        // inline isError check at all — a failed preview left the button
        // simply re-enabled, no feedback that anything went wrong.
        <p className="text-xs text-destructive">
          {errorText(previewMutation.error, "Failed to preview template")}
        </p>
      )}
      {preview && (
        <div className="rounded bg-muted p-3 text-xs font-mono">
          {/*
           * J-7 (2026-09-02 web audit): the server never sends
           * total_debit/total_credit (server/handler_metadata.go's
           * previewTemplateResponse has only `entries`) — PreviewResult used
           * to claim both as required strings, so this line rendered
           * "Total Debit: undefined | Total Credit: undefined" for every
           * preview. Summed client-side from entries instead.
           */}
          <p>
            Total Debit: {formatAmount(
              preview.entries
                .filter((e) => e.entry_type === "debit")
                .reduce((sum, e) => addAmounts(sum, e.amount), "0"),
            )}{" "}
            | Total Credit: {formatAmount(
              preview.entries
                .filter((e) => e.entry_type === "credit")
                .reduce((sum, e) => addAmounts(sum, e.amount), "0"),
            )}
          </p>
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
  const { data, isLoading, isError, refetch } = useTemplates();
  const templates = Array.isArray(data) ? data : [];
  const { pageItems, page, pageCount, setPage } = useClientPage(templates);
  // uid → human code for template lines — raw uids are unreadable in review.
  const { classCode } = useUidCodeLookups();
  const [expandedId, setExpandedId] = useState<string | null>(null);

  return (
    <div className="space-y-6">
      <PageHeader title="Templates" description="Entry template definitions" actions={<CreateTemplateDialog />} />

      {isLoading ? (
        <TableSkeleton rows={3} />
      ) : isError ? (
        <ErrorState message="Failed to load templates" onRetry={refetch} />
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
                <div className="flex flex-wrap items-center justify-between gap-3">
                  {/*
                   * J-12 (2026-09-02 web audit): `min-w-0` on the flex child
                   * + `truncate` on the title — without both, a long
                   * template name silently overflows the card instead of
                   * eliding (nextjs.md "内容溢出"). Mirrors the HeroUI skin,
                   * which already had this (`src/heroui/pages/TemplatesPage.tsx`).
                   */}
                  <div className="flex min-w-0 items-center gap-3">
                    <CardTitle className="truncate text-base">{t.name}</CardTitle>
                    <span className="shrink-0 font-mono text-xs text-muted-foreground">{t.code}</span>
                    <StatusBadge status={t.is_active ? "active" : "inactive"} />
                  </div>
                  <div className="flex shrink-0 gap-2">
                    <Button size="sm" variant="outline" onClick={() => setExpandedId(expandedId === t.uid ? null : t.uid)}>
                      {expandedId === t.uid ? "Collapse" : "Preview"}
                    </Button>
                    {t.is_active && <DeactivateDialog id={t.uid} name={t.name} />}
                  </div>
                </div>
              </CardHeader>
              <CardContent>
                <div className="grid grid-cols-2 gap-4">
                  {/* J-12: min-w-0 on each grid column + truncate on the line
                      text — a long classification code/holder_role/amount_key
                      combination would otherwise overflow the column instead
                      of eliding. Mirrors the HeroUI skin. */}
                  <div className="min-w-0">
                    <p className="text-xs font-medium text-green-400 mb-1">DEBIT</p>
                    {t.lines.filter((l) => l.entry_type === "debit").map((l) => (
                      <div key={l.sort_order} className="truncate text-xs text-muted-foreground">
                        {classCode(l.classification_uid)} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                  <div className="min-w-0">
                    <p className="text-xs font-medium text-red-400 mb-1">CREDIT</p>
                    {t.lines.filter((l) => l.entry_type === "credit").map((l) => (
                      <div key={l.sort_order} className="truncate text-xs text-muted-foreground">
                        {classCode(l.classification_uid)} / {l.holder_role} / key: {l.amount_key}
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
