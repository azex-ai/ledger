"use client";

import { useState } from "react";
import {
  Button,
  Input,
  Label,
  ListBox,
  Modal,
  Select,
  Table,
  TextArea,
  TextField,
  toast,
} from "@heroui/react";
import { BookOpen } from "lucide-react";
import { formatAmount, formatUTC } from "../../lib/utils";
import { useJournals, usePostJournal, usePostTemplateJournal } from "../../hooks/use-journals";
import { useCurrencies, useJournalTypes, useTemplates } from "../../hooks/use-metadata";
import { DefaultLink, type LinkComponent } from "../../components/nav";
import { EmptyState, ErrorState, PageHeader, StatusChip, TableSkeleton } from "../shared";

export interface JournalsPageProps {
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so the
   * page works without a host router. The journal-id cell links to
   * `/journals/{id}`.
   */
  linkComponent?: LinkComponent;
}

interface RawEntry {
  account_holder?: unknown;
  currency_uid?: unknown;
  classification_uid?: unknown;
  entry_type?: unknown;
  amount?: unknown;
}

type ValidEntry = {
  account_holder: number;
  currency_uid: string;
  classification_uid: string;
  entry_type: "debit" | "credit";
  amount: string;
};

function validateEntries(input: unknown): ValidEntry[] | string {
  if (!Array.isArray(input)) {
    return "Entries must be a JSON array";
  }
  if (input.length === 0) {
    return "Entries array must not be empty";
  }
  const out: ValidEntry[] = [];
  for (let i = 0; i < input.length; i++) {
    const e = input[i] as RawEntry;
    if (!e || typeof e !== "object") return `Entry ${i}: must be an object`;
    if (typeof e.account_holder !== "number") return `Entry ${i}: account_holder must be a number`;
    if (typeof e.currency_uid !== "string" || e.currency_uid === "") return `Entry ${i}: currency_uid must be a non-empty string`;
    if (typeof e.classification_uid !== "string" || e.classification_uid === "") return `Entry ${i}: classification_uid must be a non-empty string`;
    if (e.entry_type !== "debit" && e.entry_type !== "credit") {
      return `Entry ${i}: entry_type must be "debit" or "credit"`;
    }
    if (typeof e.amount !== "string" || e.amount === "") {
      return `Entry ${i}: amount must be a non-empty string`;
    }
    out.push({
      account_holder: e.account_holder,
      currency_uid: e.currency_uid,
      classification_uid: e.classification_uid,
      entry_type: e.entry_type,
      amount: e.amount,
    });
  }
  return out;
}

function PostJournalDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    journal_type_uid: "",
    idempotency_key: "",
    source: "api",
    entries: "",
  });
  const mutation = usePostJournal();
  const { data: journalTypes } = useJournalTypes(true);

  function handleSubmit() {
    const journalTypeUid = form.journal_type_uid.trim();
    if (journalTypeUid === "") {
      toast.danger("Select a journal type");
      return;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(form.entries);
    } catch {
      toast.danger("Invalid JSON in entries field");
      return;
    }
    const entries = validateEntries(parsed);
    if (typeof entries === "string") {
      toast.danger(entries);
      return;
    }
    mutation.mutate(
      {
        journal_type_uid: journalTypeUid,
        idempotency_key: form.idempotency_key,
        source: form.source,
        entries,
      },
      {
        onSuccess: () => {
          toast.success("Journal posted");
          setOpen(false);
        },
      },
    );
  }

  return (
    <>
      <Button size="sm" onPress={() => setOpen(true)}>
        Post Journal
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-xl">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Post Manual Journal</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <Select
                fullWidth
                placeholder="Select a journal type"
                value={form.journal_type_uid === "" ? null : form.journal_type_uid}
                onChange={(v) => {
                  if (typeof v === "string") setForm({ ...form, journal_type_uid: v });
                }}
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
              <TextField
                fullWidth
                value={form.idempotency_key}
                onChange={(v) => setForm({ ...form, idempotency_key: v })}
              >
                <Label>Idempotency Key</Label>
                <Input placeholder="deposit:user1001:1" />
              </TextField>
              <TextField
                fullWidth
                value={form.source}
                onChange={(v) => setForm({ ...form, source: v })}
              >
                <Label>Source</Label>
                <Input />
              </TextField>
              <TextField
                fullWidth
                value={form.entries}
                onChange={(v) => setForm({ ...form, entries: v })}
              >
                <Label>Entries (JSON array)</Label>
                <TextArea
                  rows={6}
                  className="font-mono text-xs"
                  placeholder={`[{"account_holder":1001,"currency_uid":1,"classification_uid":1,"entry_type":"debit","amount":"100.00"},{"account_holder":-1001,"currency_uid":1,"classification_uid":2,"entry_type":"credit","amount":"100.00"}]`}
                />
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button variant="secondary" onPress={() => setOpen(false)}>
                Cancel
              </Button>
              <Button onPress={handleSubmit} isPending={mutation.isPending}>
                {mutation.isPending ? "Posting..." : "Post"}
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function TemplateJournalDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    template_code: "",
    holder_id: "",
    currency_uid: "",
    idempotency_key: "",
    amounts: "",
    source: "",
  });
  const mutation = usePostTemplateJournal();
  const { data: templates } = useTemplates(true);
  const { data: currencies } = useCurrencies(true);

  function handleSubmit() {
    const holderId = parseInt(form.holder_id, 10);
    const currencyUid = form.currency_uid.trim();
    if (isNaN(holderId)) {
      toast.danger("Holder ID must be a number");
      return;
    }
    if (currencyUid === "") {
      toast.danger("Select a currency");
      return;
    }
    let amounts: unknown;
    try {
      amounts = JSON.parse(form.amounts);
    } catch {
      toast.danger("Invalid JSON in amounts field");
      return;
    }
    if (
      !amounts ||
      typeof amounts !== "object" ||
      Array.isArray(amounts) ||
      !Object.values(amounts).every((v) => typeof v === "string")
    ) {
      toast.danger("Amounts must be a JSON object with string values");
      return;
    }
    const amountsRecord = amounts as Record<string, string>;
    mutation.mutate(
      {
        template_code: form.template_code,
        holder_id: holderId,
        currency_uid: currencyUid,
        idempotency_key: form.idempotency_key,
        amounts: amountsRecord,
        source: form.source || undefined,
      },
      {
        onSuccess: () => {
          toast.success("Template journal posted");
          setOpen(false);
        },
      },
    );
  }

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Template Journal
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-xl">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Post Template Journal</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <Select
                fullWidth
                placeholder="Select a template"
                value={form.template_code === "" ? null : form.template_code}
                onChange={(v) => {
                  if (typeof v === "string") setForm({ ...form, template_code: v });
                }}
              >
                <Label>Template</Label>
                <Select.Trigger>
                  <Select.Value />
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    {(templates ?? []).map((t) => (
                      <ListBox.Item key={t.uid} id={t.code} textValue={t.name}>
                        {t.name}
                        <ListBox.ItemIndicator />
                      </ListBox.Item>
                    ))}
                  </ListBox>
                </Select.Popover>
              </Select>
              <div className="grid grid-cols-2 gap-4">
                <TextField
                  fullWidth
                  value={form.holder_id}
                  onChange={(v) => setForm({ ...form, holder_id: v })}
                >
                  <Label>Holder ID</Label>
                  <Input placeholder="1001" />
                </TextField>
                <Select
                  fullWidth
                  placeholder="Select"
                  value={form.currency_uid === "" ? null : form.currency_uid}
                  onChange={(v) => {
                    if (typeof v === "string") setForm({ ...form, currency_uid: v });
                  }}
                >
                  <Label>Currency</Label>
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
              </div>
              <TextField
                fullWidth
                value={form.idempotency_key}
                onChange={(v) => setForm({ ...form, idempotency_key: v })}
              >
                <Label>Idempotency Key</Label>
                <Input />
              </TextField>
              <TextField
                fullWidth
                value={form.amounts}
                onChange={(v) => setForm({ ...form, amounts: v })}
              >
                <Label>Amounts (JSON object)</Label>
                <TextArea
                  rows={3}
                  className="font-mono text-xs"
                  placeholder='{"amount": "500.00", "fee": "2.50"}'
                />
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button variant="secondary" onPress={() => setOpen(false)}>
                Cancel
              </Button>
              <Button onPress={handleSubmit} isPending={mutation.isPending}>
                {mutation.isPending ? "Posting..." : "Post"}
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

export function JournalsPage({ linkComponent: Link = DefaultLink }: JournalsPageProps = {}) {
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } = useJournals();
  const journals = data?.pages.flatMap((p) => p.list) ?? [];
  // uid → human name for the Type column; falls back to the uid while the
  // (small, cached) journal-type list loads.
  const { data: journalTypes } = useJournalTypes();
  const typeName = (uid: string) =>
    journalTypes?.find((t) => t.uid === uid)?.name ?? uid;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Journals"
        description="Double-entry journal records"
        actions={
          <>
            <TemplateJournalDialog />
            <PostJournalDialog />
          </>
        }
      />
      {isLoading ? (
        <TableSkeleton rows={8} />
      ) : isError ? (
        <ErrorState message="Failed to load journals" />
      ) : journals.length === 0 ? (
        <EmptyState
          icon={<BookOpen className="size-6 text-muted" aria-hidden="true" />}
          title="No journals yet"
          description="Post your first journal to get started."
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Journals" className="min-w-[900px]">
              <Table.Header>
                <Table.Column isRowHeader className="w-16">ID</Table.Column>
                <Table.Column>Idempotency Key</Table.Column>
                <Table.Column>Type</Table.Column>
                <Table.Column>Source</Table.Column>
                <Table.Column className="text-end">Debit</Table.Column>
                <Table.Column className="text-end">Credit</Table.Column>
                <Table.Column>Reversal</Table.Column>
                <Table.Column className="text-end">Created</Table.Column>
              </Table.Header>
              <Table.Body>
                {journals.map((j) => (
                  <Table.Row key={j.uid} id={j.uid}>
                    <Table.Cell className="max-w-[220px]">
                      <Link
                        href={`/journals/${j.uid}`}
                        className="text-accent block underline-offset-4 hover:underline"
                      >
                        <span className="block truncate" title={j.uid}>#{j.uid}</span>
                      </Link>
                    </Table.Cell>
                    <Table.Cell className="max-w-[180px] truncate font-mono text-xs">{j.idempotency_key}</Table.Cell>
                    <Table.Cell>{typeName(j.journal_type_uid)}</Table.Cell>
                    <Table.Cell>{j.source}</Table.Cell>
                    <Table.Cell className="text-end tabular-nums">{formatAmount(j.total_debit)}</Table.Cell>
                    <Table.Cell className="text-end tabular-nums">{formatAmount(j.total_credit)}</Table.Cell>
                    <Table.Cell>
                      {j.reversal_of_uid ? <StatusChip status="reversed" /> : null}
                    </Table.Cell>
                    <Table.Cell className="text-end text-xs text-muted">
                      {formatUTC(j.created_at)}
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          {hasNextPage && (
            <Table.Footer className="flex justify-center">
              <Button
                variant="secondary"
                size="sm"
                isPending={isFetchingNextPage}
                onPress={() => fetchNextPage()}
              >
                {isFetchingNextPage ? "Loading..." : "Load More"}
              </Button>
            </Table.Footer>
          )}
        </Table>
      )}
    </div>
  );
}
