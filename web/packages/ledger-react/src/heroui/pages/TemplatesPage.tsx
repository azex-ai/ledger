"use client";

import { useState } from "react";
import {
  useTemplates, useCreateTemplate, useDeactivateTemplate, usePreviewTemplate,
  useClassifications, useCurrencies, useJournalTypes, useUidCodeLookups,
} from "../../hooks/use-metadata";
import {
  AlertDialog,
  Button,
  Card,
  Input,
  Label,
  ListBox,
  Modal,
  Select,
  TextField,
  toast,
} from "@heroui/react";
import { FileCode2, X } from "lucide-react";
import { PageHeader, EmptyState, ErrorState, StatusChip, TableSkeleton } from "../shared";
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

function HolderRoleSelect({
  value,
  onChange,
  label,
}: {
  value: "user" | "system";
  onChange: (v: "user" | "system") => void;
  label: string;
}) {
  return (
    <Select
      className="w-24"
      aria-label={label}
      value={value}
      onChange={(v) => { if (typeof v === "string") onChange(v as "user" | "system"); }}
    >
      <Select.Trigger>
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          <ListBox.Item id="user" textValue="User">
            User
            <ListBox.ItemIndicator />
          </ListBox.Item>
          <ListBox.Item id="system" textValue="System">
            System
            <ListBox.ItemIndicator />
          </ListBox.Item>
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

function ClassificationSelect({
  value,
  onChange,
  label,
  classifications,
}: {
  value: string;
  onChange: (v: string) => void;
  label: string;
  classifications: { uid: string; code: string }[];
}) {
  return (
    <Select
      className="w-32"
      aria-label={label}
      placeholder="Class"
      value={value === "" ? null : value}
      onChange={(v) => { if (typeof v === "string") onChange(v); }}
    >
      <Select.Trigger>
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {classifications.map((c) => (
            <ListBox.Item key={c.uid} id={c.uid} textValue={c.code}>
              {c.code}
              <ListBox.ItemIndicator />
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

function CreateTemplateModal() {
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

  function resetAndClose() {
    setOpen(false);
    setForm({ code: "", name: "", journal_type_uid: "" });
    setLines([
      { _id: crypto.randomUUID(), classification_uid: "", entry_type: "debit", holder_role: "user", amount_key: "amount", sort_order: 1 },
      { _id: crypto.randomUUID(), classification_uid: "", entry_type: "credit", holder_role: "system", amount_key: "amount", sort_order: 2 },
    ]);
  }

  function handleSubmit() {
    const journalTypeUid = form.journal_type_uid.trim();
    if (journalTypeUid === "") {
      toast.danger("Journal Type is required");
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
          resetAndClose();
        },
        onError: () => toast.danger("Failed to create template"),
      },
    );
  }

  return (
    <>
      <Button size="sm" onPress={() => setOpen(true)}>Create Template</Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-2xl">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Create Entry Template</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <div className="grid grid-cols-3 gap-4">
                <TextFieldCode value={form.code} onChange={(v) => setForm({ ...form, code: v })} />
                <TextFieldName value={form.name} onChange={(v) => setForm({ ...form, name: v })} />
                <Select
                  className="w-full"
                  placeholder="Select"
                  value={form.journal_type_uid === "" ? null : form.journal_type_uid}
                  onChange={(v) => { if (typeof v === "string") setForm({ ...form, journal_type_uid: v }); }}
                >
                  <Label>Journal Type</Label>
                  <Select.Trigger>
                    <Select.Value />
                    <Select.Indicator />
                  </Select.Trigger>
                  <Select.Popover>
                    <ListBox>
                      {(journalTypes ?? []).map((t) => (
                        <ListBox.Item key={t.uid} id={t.uid} textValue={t.name}>
                          {t.name}
                          <ListBox.ItemIndicator />
                        </ListBox.Item>
                      ))}
                    </ListBox>
                  </Select.Popover>
                </Select>
              </div>

              <div className="flex flex-col gap-3">
                <div className="flex items-center justify-between">
                  <Label>Template Lines</Label>
                  <Button size="sm" variant="outline" onPress={addLine}>+ Add Line</Button>
                </div>

                <div className="grid grid-cols-2 gap-4">
                  <div className="min-w-0">
                    <p className="mb-2 text-xs font-medium text-success">DEBIT SIDE</p>
                    {lines.map((l, idx) => l.entry_type !== "debit" ? null : (
                      <div key={l._id} className="mb-2 flex flex-col gap-2 rounded-lg border border-success/20 bg-success-soft/30 p-3">
                        <div className="flex gap-2">
                          <ClassificationSelect
                            label="Classification"
                            value={l.classification_uid}
                            onChange={(v) => updateLine(idx, { classification_uid: v })}
                            classifications={classifications ?? []}
                          />
                          <HolderRoleSelect
                            label="Holder role"
                            value={l.holder_role}
                            onChange={(v) => updateLine(idx, { holder_role: v })}
                          />
                          <TextField
                            aria-label="Amount key"
                            className="min-w-0 flex-1"
                            value={l.amount_key}
                            onChange={(v) => updateLine(idx, { amount_key: v })}
                          >
                            <Input placeholder="amount_key" />
                          </TextField>
                          <Button isIconOnly size="sm" variant="ghost" aria-label="Remove line" onPress={() => removeLine(idx)}>
                            <X className="size-4" />
                          </Button>
                        </div>
                      </div>
                    ))}
                  </div>
                  <div className="min-w-0">
                    <p className="mb-2 text-xs font-medium text-danger">CREDIT SIDE</p>
                    {lines.map((l, idx) => l.entry_type !== "credit" ? null : (
                      <div key={l._id} className="mb-2 flex flex-col gap-2 rounded-lg border border-danger/20 bg-danger-soft/30 p-3">
                        <div className="flex gap-2">
                          <ClassificationSelect
                            label="Classification"
                            value={l.classification_uid}
                            onChange={(v) => updateLine(idx, { classification_uid: v })}
                            classifications={classifications ?? []}
                          />
                          <HolderRoleSelect
                            label="Holder role"
                            value={l.holder_role}
                            onChange={(v) => updateLine(idx, { holder_role: v })}
                          />
                          <TextField
                            aria-label="Amount key"
                            className="min-w-0 flex-1"
                            value={l.amount_key}
                            onChange={(v) => updateLine(idx, { amount_key: v })}
                          >
                            <Input placeholder="amount_key" />
                          </TextField>
                          <Button isIconOnly size="sm" variant="ghost" aria-label="Remove line" onPress={() => removeLine(idx)}>
                            <X className="size-4" />
                          </Button>
                        </div>
                      </div>
                    ))}
                  </div>
                </div>
              </div>
            </Modal.Body>
            <Modal.Footer>
              <Button variant="secondary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                onPress={handleSubmit}
                isPending={mutation.isPending}
                isDisabled={!form.code || !form.name}
              >
                Create
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

// Small wrappers to keep the 3-column header grid readable above.
function TextFieldCode({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <TextField value={value} onChange={onChange}>
      <Label>Code</Label>
      <Input placeholder="deposit_confirm" />
    </TextField>
  );
}

function TextFieldName({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <TextField value={value} onChange={onChange}>
      <Label>Name</Label>
      <Input placeholder="Confirm Deposit" />
    </TextField>
  );
}

function DeactivateTemplateDialog({ id, name }: { id: string; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateTemplate();

  return (
    <>
      <Button size="sm" variant="tertiary" onPress={() => setOpen(true)}>Deactivate</Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="warning" />
              <AlertDialog.Heading>Deactivate &quot;{name}&quot;?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This template will be marked inactive and can no longer be used for new journals.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button variant="tertiary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() => mutation.mutate(id, {
                  onSuccess: () => {
                    toast.success("Template deactivated");
                    setOpen(false);
                  },
                  onError: () => toast.danger("Failed to deactivate template"),
                })}
              >
                Deactivate
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

function PreviewSection({ code }: { code: string }) {
  const [params, setParams] = useState({ holder_id: "", currency_uid: "", amount: "" });
  const previewMutation = usePreviewTemplate();
  const preview = previewMutation.data as PreviewResult | undefined;
  const { data: currencies } = useCurrencies(true);

  return (
    <div className="mt-2 flex flex-col gap-2">
      <div className="flex flex-wrap items-end gap-2">
        <TextField
          aria-label="Holder ID"
          className="w-28"
          value={params.holder_id}
          onChange={(v) => setParams({ ...params, holder_id: v })}
        >
          <Input placeholder="Holder ID" />
        </TextField>
        <Select
          className="w-28"
          aria-label="Currency"
          placeholder="Currency"
          value={params.currency_uid === "" ? null : params.currency_uid}
          onChange={(v) => { if (typeof v === "string") setParams({ ...params, currency_uid: v }); }}
        >
          <Select.Trigger>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {(currencies ?? []).map((c) => (
                <ListBox.Item key={c.uid} id={c.uid} textValue={c.code}>
                  {c.code}
                  <ListBox.ItemIndicator />
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
        <TextField
          aria-label="Amount"
          className="w-28"
          value={params.amount}
          onChange={(v) => setParams({ ...params, amount: v })}
        >
          <Input placeholder="Amount" />
        </TextField>
        <Button
          size="sm"
          variant="outline"
          isPending={previewMutation.isPending}
          onPress={() =>
            previewMutation.mutate({
              code,
              holder_id: parseInt(params.holder_id, 10),
              currency_uid: params.currency_uid.trim(),
              amount: params.amount,
            })
          }
        >
          Preview
        </Button>
      </div>
      {preview && (
        <div className="rounded-lg bg-surface-secondary p-3 font-mono text-xs">
          <p>Total Debit: {preview.total_debit} | Total Credit: {preview.total_credit}</p>
          {preview.entries.map((e, i) => (
            <p key={i} className="truncate">
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
  // uid → human code for template lines — raw uids are unreadable in review.
  const { classCode } = useUidCodeLookups();
  const [expandedId, setExpandedId] = useState<string | null>(null);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="Templates" description="Entry template definitions" actions={<CreateTemplateModal />} />

      {isLoading ? (
        <TableSkeleton rows={3} />
      ) : isError ? (
        <ErrorState message="Failed to load templates" />
      ) : templates.length === 0 ? (
        <EmptyState
          icon={<FileCode2 className="size-8 text-muted" aria-hidden />}
          title="No templates yet"
          description="Create your first template to define reusable journal recipes."
        />
      ) : (
        <div className="flex flex-col gap-4">
          {pageItems.map((t) => (
            <Card key={t.uid}>
              <Card.Header>
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div className="flex min-w-0 items-center gap-3">
                    <Card.Title className="truncate text-base">{t.name}</Card.Title>
                    <span className="shrink-0 font-mono text-xs text-muted">{t.code}</span>
                    <StatusChip status={t.is_active ? "active" : "inactive"} />
                  </div>
                  <div className="flex shrink-0 gap-2">
                    <Button size="sm" variant="outline" onPress={() => setExpandedId(expandedId === t.uid ? null : t.uid)}>
                      {expandedId === t.uid ? "Collapse" : "Preview"}
                    </Button>
                    {t.is_active && <DeactivateTemplateDialog id={t.uid} name={t.name} />}
                  </div>
                </div>
              </Card.Header>
              <Card.Content>
                <div className="grid grid-cols-2 gap-4">
                  <div className="min-w-0">
                    <p className="mb-1 text-xs font-medium text-success">DEBIT</p>
                    {t.lines.filter((l) => l.entry_type === "debit").map((l) => (
                      <div key={l.sort_order} className="truncate text-xs text-muted">
                        {classCode(l.classification_uid)} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                  <div className="min-w-0">
                    <p className="mb-1 text-xs font-medium text-danger">CREDIT</p>
                    {t.lines.filter((l) => l.entry_type === "credit").map((l) => (
                      <div key={l.sort_order} className="truncate text-xs text-muted">
                        {classCode(l.classification_uid)} / {l.holder_role} / key: {l.amount_key}
                      </div>
                    ))}
                  </div>
                </div>
                {expandedId === t.uid && <PreviewSection code={t.code} />}
              </Card.Content>
            </Card>
          ))}
          <PaginationBar page={page} pageCount={pageCount} onPageChange={setPage} />
        </div>
      )}
    </div>
  );
}
