"use client";

import { useState } from "react";
import {
  useWithdrawals,
  useReserveWithdraw,
  useReviewWithdraw,
  useProcessWithdraw,
  useConfirmWithdraw,
  useFailWithdraw,
  useRetryWithdraw,
} from "@/lib/hooks/use-withdrawals";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";

const WITHDRAW_STATES = ["locked", "reserved", "reviewing", "processing", "confirmed", "failed", "expired"];

function ProcessDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [channelRef, setChannelRef] = useState("");
  const mutation = useProcessWithdraw();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Process</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Process Withdrawal #{id}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label>Channel Ref</Label>
            <Input value={channelRef} onChange={(e) => setChannelRef(e.target.value)} placeholder="0xdef..." />
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={() => mutation.mutate({ id, channelRef }, { onSuccess: () => setOpen(false) })}
            disabled={mutation.isPending || !channelRef}
          >
            {mutation.isPending ? "Processing..." : "Submit"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function WithdrawalsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  const { data, isLoading, hasNextPage, fetchNextPage, isFetchingNextPage } = useWithdrawals({
    status: statusFilter || undefined,
  });
  const withdrawals = data?.pages.flatMap((p) => p.data) ?? [];

  const reserveMutation = useReserveWithdraw();
  const reviewMutation = useReviewWithdraw();
  const confirmMutation = useConfirmWithdraw();
  const failMutation = useFailWithdraw();
  const retryMutation = useRetryWithdraw();

  return (
    <div className="space-y-6">
      <PageHeader title="Withdrawals" description="Outbound withdrawal tracking" />

      <div className="flex gap-2">
        <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v ?? "")}>
          <SelectTrigger className="w-40">
            <SelectValue placeholder="All statuses" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All</SelectItem>
            {WITHDRAW_STATES.map((s) => (
              <SelectItem key={s} value={s}>{s}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="h-10 animate-pulse rounded bg-muted" />
          ))}
        </div>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead className="text-right">Amount</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Review</TableHead>
                <TableHead className="text-right">Created</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {withdrawals.map((w) => (
                <TableRow key={w.id}>
                  <TableCell>#{w.id}</TableCell>
                  <TableCell>{w.account_holder}</TableCell>
                  <TableCell>{w.channel_name}</TableCell>
                  <TableCell className="text-right font-mono">{w.amount}</TableCell>
                  <TableCell><StatusBadge status={w.status} /></TableCell>
                  <TableCell>{w.review_required ? "Required" : "Auto"}</TableCell>
                  <TableCell className="text-right text-xs text-muted-foreground">
                    {new Date(w.created_at).toLocaleString()}
                  </TableCell>
                  <TableCell>
                    <div className="flex gap-1 flex-wrap">
                      {w.status === "locked" && (
                        <Button size="sm" variant="outline" onClick={() => reserveMutation.mutate(w.id)} disabled={reserveMutation.isPending}>
                          Reserve
                        </Button>
                      )}
                      {w.status === "reviewing" && (
                        <>
                          <Button size="sm" variant="outline" onClick={() => reviewMutation.mutate({ id: w.id, approved: true })} disabled={reviewMutation.isPending}>
                            Approve
                          </Button>
                          <Button size="sm" variant="ghost" onClick={() => reviewMutation.mutate({ id: w.id, approved: false })} disabled={reviewMutation.isPending}>
                            Reject
                          </Button>
                        </>
                      )}
                      {(w.status === "reserved" || (w.status === "reviewing")) && (
                        <ProcessDialog id={w.id} />
                      )}
                      {w.status === "processing" && (
                        <>
                          <Button size="sm" variant="outline" onClick={() => confirmMutation.mutate(w.id)} disabled={confirmMutation.isPending}>
                            Confirm
                          </Button>
                          <Button size="sm" variant="ghost" onClick={() => failMutation.mutate({ id: w.id, reason: "manual fail" })} disabled={failMutation.isPending}>
                            Fail
                          </Button>
                        </>
                      )}
                      {w.status === "failed" && (
                        <Button size="sm" variant="outline" onClick={() => retryMutation.mutate(w.id)} disabled={retryMutation.isPending}>
                          Retry
                        </Button>
                      )}
                    </div>
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
