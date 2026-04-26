"use client";

import { useState } from "react";
import { formatAmount, formatUTC } from "@/lib/utils";
import { useJournals, usePostJournal, usePostTemplateJournal } from "@/lib/hooks/use-journals";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import Link from "next/link";
import { BookOpen } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import { EmptyState } from "@/components/empty-state";
import { TableSkeleton } from "@/components/loading-skeleton";

function PostJournalDialog() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    journal_type_id: "",
    idempotency_key: "",
    source: "api",
    entries: "",
  });
  const mutation = usePostJournal();

  function handleSubmit() {
    const journalTypeId = parseInt(form.journal_type_id, 10);
    if (isNaN(journalTypeId)) {
      toast.error("Journal Type ID must be a number");
      return;
    }
    let entries;
    try {
      entries = JSON.parse(form.entries);
    } catch {
      toast.error("Invalid JSON in entries field");
      return;
    }
    mutation.mutate(
      {
        journal_type_id: journalTypeId,
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
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" />}>Post Journal</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Post Manual Journal</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="pj-type-id">Journal Type ID</Label>
            <Input id="pj-type-id" value={form.journal_type_id} onChange={(e) => setForm({ ...form, journal_type_id: e.target.value })} placeholder="1" />
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
              placeholder={`[{"account_holder":1001,"currency_id":1,"classification_id":1,"entry_type":"debit","amount":"100.00"},{"account_holder":-1001,"currency_id":1,"classification_id":2,"entry_type":"credit","amount":"100.00"}]`}
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
    currency_id: "",
    idempotency_key: "",
    amounts: "",
    source: "",
  });
  const mutation = usePostTemplateJournal();

  function handleSubmit() {
    const holderId = parseInt(form.holder_id, 10);
    const currencyId = parseInt(form.currency_id, 10);
    if (isNaN(holderId) || isNaN(currencyId)) {
      toast.error("Holder ID and Currency ID must be numbers");
      return;
    }
    let amounts;
    try {
      amounts = JSON.parse(form.amounts);
    } catch {
      toast.error("Invalid JSON in amounts field");
      return;
    }
    mutation.mutate(
      {
        template_code: form.template_code,
        holder_id: holderId,
        currency_id: currencyId,
        idempotency_key: form.idempotency_key,
        amounts,
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
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Template Journal</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Post Template Journal</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="tj-tpl-code">Template Code</Label>
            <Input id="tj-tpl-code" value={form.template_code} onChange={(e) => setForm({ ...form, template_code: e.target.value })} placeholder="deposit_confirm" />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="tj-holder">Holder ID</Label>
              <Input id="tj-holder" value={form.holder_id} onChange={(e) => setForm({ ...form, holder_id: e.target.value })} placeholder="1001" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="tj-currency">Currency ID</Label>
              <Input id="tj-currency" value={form.currency_id} onChange={(e) => setForm({ ...form, currency_id: e.target.value })} placeholder="1" />
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

export function JournalsClient() {
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } = useJournals();
  const journals = data?.pages.flatMap((p) => p.data) ?? [];

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
          icon={BookOpen}
          title="No journals yet"
          description="Post your first journal to get started."
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-16">ID</TableHead>
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
                <TableRow key={j.id}>
                  <TableCell>
                    <Link href={`/journals/${j.id}`} className="text-primary underline-offset-4 hover:underline">
                      #{j.id}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono text-xs max-w-[180px] truncate">{j.idempotency_key}</TableCell>
                  <TableCell>{j.journal_type_id}</TableCell>
                  <TableCell>{j.source}</TableCell>
                  <TableCell className="text-right font-mono">{formatAmount(j.total_debit)}</TableCell>
                  <TableCell className="text-right font-mono">{formatAmount(j.total_credit)}</TableCell>
                  <TableCell>
                    {j.reversal_of ? (
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
          {hasNextPage && (
            <div className="flex justify-center">
              <Button variant="outline" size="sm" onClick={() => fetchNextPage()} disabled={isFetchingNextPage}>
                {isFetchingNextPage ? "Loading..." : "Load More"}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
