"use client";

import { use, useState } from "react";
import { formatAmount, formatUTC } from "@/lib/utils";
import { useJournal, useReverseJournal } from "@/lib/hooks/use-journals";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { StatusBadge } from "@/components/status-badge";
import Link from "next/link";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import type { Entry } from "@/lib/api";

function EntryFlow({ entries }: { entries: Entry[] }) {
  const debits = entries.filter((e) => e.entry_type === "debit");
  const credits = entries.filter((e) => e.entry_type === "credit");

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-medium">Fund Flow</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="flex items-start gap-8">
          <div className="flex-1 space-y-2">
            <p className="text-xs font-medium text-muted-foreground uppercase">Debit</p>
            {debits.map((e) => (
              <div key={e.id} className="rounded border border-emerald-500/20 bg-emerald-500/5 p-3">
                <div className="flex justify-between">
                  <span className="text-sm">Holder {e.account_holder}</span>
                  <span className="font-mono text-sm text-emerald-400">{formatAmount(e.amount)}</span>
                </div>
                <p className="text-xs text-muted-foreground">
                  Class {e.classification_id} / Currency {e.currency_id}
                </p>
              </div>
            ))}
          </div>
          <div className="flex flex-col items-center justify-center pt-6" aria-hidden="true">
            <div className="text-2xl text-muted-foreground">&rarr;</div>
          </div>
          <div className="flex-1 space-y-2">
            <p className="text-xs font-medium text-muted-foreground uppercase">Credit</p>
            {credits.map((e) => (
              <div key={e.id} className="rounded border border-rose-500/20 bg-rose-500/5 p-3">
                <div className="flex justify-between">
                  <span className="text-sm">Holder {e.account_holder}</span>
                  <span className="font-mono text-sm text-rose-400">{formatAmount(e.amount)}</span>
                </div>
                <p className="text-xs text-muted-foreground">
                  Class {e.classification_id} / Currency {e.currency_id}
                </p>
              </div>
            ))}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function ReverseDialog({ journalId }: { journalId: number }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useReverseJournal();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="destructive" />}>Reverse</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Reverse Journal #{journalId}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="jd-reverse-reason">Reason</Label>
            <Input id="jd-reverse-reason" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="duplicate deposit" />
          </div>
        </div>
        <DialogFooter>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate({ id: journalId, reason }, {
              onSuccess: () => {
                toast.success("Journal reversed");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending || !reason}
          >
            {mutation.isPending ? "Reversing..." : "Confirm Reverse"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function JournalDetailClient({ params }: { params: Promise<{ id: string }> }) {
  const { id: idStr } = use(params);
  const id = parseInt(idStr, 10);
  const { data, isLoading, isError } = useJournal(id);

  if (isLoading) {
    return <div className="space-y-4">
      <div className="h-8 w-48 animate-shimmer rounded" />
      <div className="h-64 animate-shimmer rounded" />
    </div>;
  }

  if (isError) {
    return <ErrorState message="Failed to load journal" />;
  }

  if (!data) {
    return <p className="text-muted-foreground">Journal not found</p>;
  }

  const { journal: j, entries } = data;

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Journal #${j.id}`}
        actions={
          <>
            {j.reversal_of && (
              <Link href={`/journals/${j.reversal_of}`}>
                <StatusBadge status="reversed" />
              </Link>
            )}
            <ReverseDialog journalId={j.id} />
          </>
        }
      />

      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <Card>
          <CardContent className="pt-4">
            <p className="text-xs text-muted-foreground">Type ID</p>
            <p className="text-lg font-bold">{j.journal_type_id}</p>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <p className="text-xs text-muted-foreground">Source</p>
            <p className="text-lg font-bold">{j.source}</p>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <p className="text-xs text-muted-foreground">Total Debit</p>
            <p className="text-lg font-bold font-mono">{formatAmount(j.total_debit)}</p>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <p className="text-xs text-muted-foreground">Total Credit</p>
            <p className="text-lg font-bold font-mono">{formatAmount(j.total_credit)}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Details</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2 text-sm">
          <div className="flex justify-between">
            <span className="text-muted-foreground">Idempotency Key</span>
            <span className="font-mono">{j.idempotency_key}</span>
          </div>
          <div className="flex justify-between">
            <span className="text-muted-foreground">Created At</span>
            <span>{formatUTC(j.created_at)}</span>
          </div>
          {j.actor_id !== 0 && (
            <div className="flex justify-between">
              <span className="text-muted-foreground">Actor ID</span>
              <span>{j.actor_id}</span>
            </div>
          )}
          {j.metadata && Object.keys(j.metadata).length > 0 && (
            <div>
              <span className="text-muted-foreground">Metadata</span>
              <pre className="mt-1 rounded bg-muted p-2 text-xs font-mono">
                {JSON.stringify(j.metadata, null, 2)}
              </pre>
            </div>
          )}
        </CardContent>
      </Card>

      <EntryFlow entries={entries} />

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Entries</CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead>Currency</TableHead>
                <TableHead>Classification</TableHead>
                <TableHead>Type</TableHead>
                <TableHead className="text-right">Amount</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e) => (
                <TableRow key={e.id}>
                  <TableCell>{e.id}</TableCell>
                  <TableCell>{e.account_holder}</TableCell>
                  <TableCell>{e.currency_id}</TableCell>
                  <TableCell>{e.classification_id}</TableCell>
                  <TableCell>
                    <StatusBadge status={e.entry_type} />
                  </TableCell>
                  <TableCell className="text-right font-mono">{formatAmount(e.amount)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
