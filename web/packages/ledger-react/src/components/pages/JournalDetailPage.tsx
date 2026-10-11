"use client";

import { errorText } from "../../lib/error-message";
import { useState } from "react";
import { formatUTC } from "../../lib/utils";
import { useJournal, useReverseJournal } from "../../hooks/use-journals";
import { useJournalTypes, useUidCodeLookups } from "../../hooks/use-metadata";
import { PageHeader } from "../page-header";
import { Button } from "../ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { StatusBadge } from "../status-badge";
import { DefaultLink, type LinkComponent } from "../nav";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { PageHeaderSkeleton, TableSkeleton } from "../loading-skeleton";
import { ExactAmount } from "../exact-amount";
import { journalTotalsByCurrency } from "../../lib/journal-totals";

export interface JournalDetailPageProps {
  /** Journal id — the host extracts this from its route param and passes it. */
  id: string;
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so the
   * page works without a host router. Used by the reversal-of link to
   * `/journals/{id}`.
   */
  linkComponent?: LinkComponent;
}

function ReverseDialog({ journalId }: { journalId: string }) {
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
              onError: (err) => toast.error(errorText(err, "Failed to reverse journal")),
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

export function JournalDetailPage({ id, linkComponent: Link = DefaultLink }: JournalDetailPageProps) {
  const { data, isLoading, isError, refetch } = useJournal(id);
  // query-consumption-allow: uid→name resolver, falls back to the raw uid via `??` below (incl. on fetch failure) — the fallback IS the degraded state, not a false claim like J-1/J-2/J-3.
  const { data: journalTypes } = useJournalTypes();
  const { currencyCode, classCode } = useUidCodeLookups();

  if (isLoading) {
    return (
      <div className="space-y-6">
        <PageHeaderSkeleton />
        <TableSkeleton rows={6} />
      </div>
    );
  }

  if (isError) {
    return <ErrorState message="Failed to load journal" onRetry={refetch} />;
  }

  if (!data) {
    return <p className="text-muted-foreground">Journal not found</p>;
  }

  const { journal: j, entries } = data;

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Journal #${j.uid}`}
        actions={
          <>
            {j.reversal_of_uid && (
              <Link href={`/journals/${j.reversal_of_uid}`}>
                <StatusBadge status="reversed" />
              </Link>
            )}
            <ReverseDialog journalId={j.uid} />
          </>
        }
      />

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <Card>
          <CardContent className="pt-4">
            <p className="text-xs text-muted-foreground">Type</p>
            <p className="text-lg font-bold">
              {journalTypes?.find((t) => t.uid === j.journal_type_uid)?.name ??
                j.journal_type_uid}
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <p className="text-xs text-muted-foreground">Source</p>
            <p className="text-lg font-bold">{j.source}</p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Details</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2 text-sm">
          <div className="flex justify-between gap-2">
            <span className="text-muted-foreground shrink-0">Idempotency Key</span>
            <span className="font-mono truncate min-w-0 text-right" title={j.idempotency_key}>{j.idempotency_key}</span>
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

      <Card>
        <CardHeader><CardTitle className="text-sm font-medium">Totals by currency</CardTitle></CardHeader>
        <CardContent>
          <Table aria-label="Journal totals by currency">
            <TableHeader><TableRow><TableHead>Currency</TableHead><TableHead className="text-right">Debit</TableHead><TableHead className="text-right">Credit</TableHead></TableRow></TableHeader>
            <TableBody>
              {journalTotalsByCurrency(entries).map((total) => (
                <TableRow key={total.currencyUid}>
                  <TableCell title={total.currencyUid}>{currencyCode(total.currencyUid)}</TableCell>
                  <TableCell className="text-right tabular-nums"><ExactAmount value={total.debit} currencyCode={currencyCode(total.currencyUid)} /></TableCell>
                  <TableCell className="text-right tabular-nums"><ExactAmount value={total.credit} currencyCode={currencyCode(total.currencyUid)} /></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Entries</CardTitle>
        </CardHeader>
        <CardContent>
          <Table aria-label="Journal entries" className="min-w-[720px]">
            <TableHeader>
              <TableRow>
                <TableHead>Holder</TableHead>
                <TableHead>Currency</TableHead>
                <TableHead>Classification</TableHead>
                <TableHead>Type</TableHead>
                <TableHead className="text-right">Amount</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e) => (
                <TableRow key={`${e.entry_type}-${e.account_holder}-${e.classification_uid}-${e.currency_uid}`}>
                  <TableCell>{e.account_holder}</TableCell>
                  <TableCell className="max-w-40">
                    <span className="block truncate font-mono text-xs" title={e.currency_uid}>{currencyCode(e.currency_uid)}</span>
                  </TableCell>
                  <TableCell className="max-w-40">
                    <span className="block truncate font-mono text-xs" title={e.classification_uid}>{classCode(e.classification_uid)}</span>
                  </TableCell>
                  <TableCell>
                    {e.entry_type === "debit" ? "Debit" : "Credit"}
                  </TableCell>
                  <TableCell className="text-right tabular-nums"><ExactAmount value={e.amount} currencyCode={currencyCode(e.currency_uid)} /></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
