"use client";

import { errorText } from "../../lib/error-message";
import { validateEntries } from "../../lib/validate-entries";
import { useState } from "react";
import { formatAmount, formatUTC } from "../../lib/utils";
import { useJournals, usePostJournal, usePostTemplateJournal } from "../../hooks/use-journals";
import { useCurrencies, useJournalTypes, useTemplates } from "../../hooks/use-metadata";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import { Button } from "../ui/button";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "../ui/select";
import { Textarea } from "../ui/textarea";
import { DefaultLink, type LinkComponent } from "../nav";
import { BookOpen } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";
import { LoadMoreBar } from "../pagination-bar";

export interface JournalsPageProps {
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so the
   * page works without a host router. The journal-id cell links to
   * `/journals/{id}`.
   */
  linkComponent?: LinkComponent;
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
  // query-consumption-allow: populates the journal-type <Select> below; a failed fetch empties the dropdown, a self-evident degradation the user can see and retry — not a false claim like J-1/J-2/J-3.
  const { data: journalTypes } = useJournalTypes(true);

  function handleSubmit() {
    const journalTypeUid = form.journal_type_uid.trim();
    if (journalTypeUid === "") {
      toast.error("Select a journal type");
      return;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(form.entries);
    } catch {
      toast.error("Invalid JSON in entries field");
      return;
    }
    const entries = validateEntries(parsed);
    if (typeof entries === "string") {
      toast.error(entries);
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
        onError: (err) => toast.error(errorText(err, "Failed to post journal")),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" />}>Post Journal</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Post Manual Journal</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="pj-type">Journal Type</Label>
            <Select
              value={form.journal_type_uid === "" ? null : form.journal_type_uid}
              onValueChange={(v) => { if (typeof v === "string") setForm({ ...form, journal_type_uid: v }); }}
            >
              <SelectTrigger id="pj-type" className="w-full">
                <SelectValue placeholder="Select a journal type" />
              </SelectTrigger>
              <SelectContent>
                {(journalTypes ?? []).map((t) => (
                  <SelectItem key={t.uid} value={t.uid}>{t.name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pj-idem-key">Idempotency Key</Label>
            <Input id="pj-idem-key" value={form.idempotency_key} onChange={(e) => setForm({ ...form, idempotency_key: e.target.value })} placeholder="deposit:user1001:1" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pj-source">Source</Label>
            <Input id="pj-source" value={form.source} onChange={(e) => setForm({ ...form, source: e.target.value })} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pj-entries">Entries (JSON array)</Label>
            <Textarea
              id="pj-entries"
              value={form.entries}
              onChange={(e) => setForm({ ...form, entries: e.target.value })}
              rows={6}
              className="font-mono text-xs"
              placeholder={`[{"account_holder":1001,"currency_uid":"<currency-uid>","classification_uid":"<classification-uid>","entry_type":"debit","amount":"100.00"},{"account_holder":-1001,"currency_uid":"<currency-uid>","classification_uid":"<classification-uid>","entry_type":"credit","amount":"100.00"}]`}
            />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={handleSubmit} disabled={mutation.isPending}>
            {mutation.isPending ? "Posting..." : "Post"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
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
  // query-consumption-allow: populates the template/currency <Select>s below; a failed fetch empties the dropdown, a self-evident degradation the user can see and retry — not a false claim like J-1/J-2/J-3.
  const { data: templates } = useTemplates(true);
  // query-consumption-allow: same as templates above.
  const { data: currencies } = useCurrencies(true);

  function handleSubmit() {
    const holderId = parseInt(form.holder_id, 10);
    const currencyUid = form.currency_uid.trim();
    if (isNaN(holderId)) {
      toast.error("Holder ID must be a number");
      return;
    }
    if (currencyUid === "") {
      toast.error("Select a currency");
      return;
    }
    let amounts: unknown;
    try {
      amounts = JSON.parse(form.amounts);
    } catch {
      toast.error("Invalid JSON in amounts field");
      return;
    }
    if (
      !amounts ||
      typeof amounts !== "object" ||
      Array.isArray(amounts) ||
      !Object.values(amounts).every((v) => typeof v === "string")
    ) {
      toast.error("Amounts must be a JSON object with string values");
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
        onError: (err) => toast.error(errorText(err, "Failed to post template journal")),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Template Journal</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Post Template Journal</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="tj-tpl-code">Template</Label>
            <Select
              value={form.template_code === "" ? null : form.template_code}
              onValueChange={(v) => { if (typeof v === "string") setForm({ ...form, template_code: v }); }}
            >
              <SelectTrigger id="tj-tpl-code" className="w-full">
                <SelectValue placeholder="Select a template" />
              </SelectTrigger>
              <SelectContent>
                {(templates ?? []).map((t) => (
                  <SelectItem key={t.uid} value={t.code}>{t.name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="tj-holder">Holder ID</Label>
              <Input id="tj-holder" value={form.holder_id} onChange={(e) => setForm({ ...form, holder_id: e.target.value })} placeholder="1001" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="tj-currency">Currency</Label>
              <Select
                value={form.currency_uid === "" ? null : form.currency_uid}
                onValueChange={(v) => { if (typeof v === "string") setForm({ ...form, currency_uid: v }); }}
              >
                <SelectTrigger id="tj-currency" className="w-full">
                  <SelectValue placeholder="Select" />
                </SelectTrigger>
                <SelectContent>
                  {(currencies ?? []).map((c) => (
                    <SelectItem key={c.uid} value={c.uid}>{c.code}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="tj-idem-key">Idempotency Key</Label>
            <Input id="tj-idem-key" value={form.idempotency_key} onChange={(e) => setForm({ ...form, idempotency_key: e.target.value })} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="tj-amounts">Amounts (JSON object)</Label>
            <Textarea
              id="tj-amounts"
              value={form.amounts}
              onChange={(e) => setForm({ ...form, amounts: e.target.value })}
              rows={3}
              className="font-mono text-xs"
              placeholder='{"amount": "500.00", "fee": "2.50"}'
            />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={handleSubmit} disabled={mutation.isPending}>
            {mutation.isPending ? "Posting..." : "Post"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function JournalsPage({ linkComponent: Link = DefaultLink }: JournalsPageProps = {}) {
  const { data, isLoading, isError, refetch, hasNextPage, fetchNextPage, isFetchingNextPage } = useJournals();
  const journals = data?.pages.flatMap((p) => p.list) ?? [];
  // uid → human name for the Type column; falls back to the uid while the
  // (small, cached) journal-type list loads.
  // query-consumption-allow: falls back to the raw uid via `??` below (incl. on fetch failure) — the fallback IS the degraded state, not a false claim like J-1/J-2/J-3.
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
        <ErrorState message="Failed to load journals" onRetry={refetch} />
      ) : journals.length === 0 ? (
        <EmptyState
          icon={BookOpen}
          title="No journals yet"
          description="Post your first journal to get started."
        />
      ) : (
        <>
          <Table aria-label="Journals" className="min-w-[900px]">
            <TableHeader>
              <TableRow>
                <TableHead className="w-[220px]">ID</TableHead>
                <TableHead>Idempotency Key</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Source</TableHead>
                <TableHead className="text-right">Debit</TableHead>
                <TableHead className="text-right">Credit</TableHead>
                <TableHead>Reversal</TableHead>
                <TableHead className="text-right">Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {journals.map((j) => (
                <TableRow key={j.uid}>
                  <TableCell className="max-w-[220px]">
                    <Link href={`/journals/${j.uid}`} className="block text-primary underline-offset-4 hover:underline">
                      <span className="block truncate" title={j.uid}>#{j.uid}</span>
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono text-xs max-w-[180px] truncate" title={j.idempotency_key}>{j.idempotency_key}</TableCell>
                  <TableCell>{typeName(j.journal_type_uid)}</TableCell>
                  <TableCell>{j.source}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatAmount(j.total_debit)}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatAmount(j.total_credit)}</TableCell>
                  <TableCell>
                    {j.reversal_of_uid ? (
                      <StatusBadge status="reversed" />
                    ) : null}
                  </TableCell>
                  <TableCell className="text-right text-xs text-muted-foreground">
                    {formatUTC(j.created_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </>
      )}
    </div>
  );
}
