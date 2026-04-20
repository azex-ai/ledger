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
import { AlertCircle, ArrowUpFromLine } from "lucide-react";
import { toast } from "sonner";

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
            onClick={() => mutation.mutate({ id, channelRef }, {
              onSuccess: () => {
                toast.success("Withdrawal processing");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending || !channelRef}
          >
            {mutation.isPending ? "Processing..." : "Submit"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function FailDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useFailWithdraw();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Fail</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Fail Withdrawal #{id}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <p className="text-sm text-muted-foreground">This will mark the withdrawal as failed.</p>
          <div className="grid gap-2">
            <Label>Reason</Label>
            <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Insufficient gas, timeout, etc." />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate({ id, reason: reason || "manual fail" }, {
              onSuccess: () => {
                toast.success("Withdrawal marked as failed");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending}
          >
            {mutation.isPending ? "Failing..." : "Fail Withdrawal"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ReserveButton({ id }: { id: number }) {
  const mutation = useReserveWithdraw();
  return (
    <Button size="sm" variant="outline" onClick={() => mutation.mutate(id, { onSuccess: () => toast.success("Withdrawal reserved") })} disabled={mutation.isPending}>
      {mutation.isPending ? "Reserving..." : "Reserve"}
    </Button>
  );
}

function ReviewButtons({ id }: { id: number }) {
  const mutation = useReviewWithdraw();
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => mutation.mutate({ id, approved: true }, { onSuccess: () => toast.success("Withdrawal approved") })} disabled={mutation.isPending}>
        {mutation.isPending ? "..." : "Approve"}
      </Button>
      <Button size="sm" variant="ghost" onClick={() => mutation.mutate({ id, approved: false }, { onSuccess: () => toast.success("Withdrawal rejected") })} disabled={mutation.isPending}>
        {mutation.isPending ? "..." : "Reject"}
      </Button>
    </>
  );
}

function ConfirmButton({ id }: { id: number }) {
  const mutation = useConfirmWithdraw();
  return (
    <Button size="sm" variant="outline" onClick={() => mutation.mutate(id, { onSuccess: () => toast.success("Withdrawal confirmed") })} disabled={mutation.isPending}>
      {mutation.isPending ? "Confirming..." : "Confirm"}
    </Button>
  );
}

function RetryButton({ id }: { id: number }) {
  const mutation = useRetryWithdraw();
  return (
    <Button size="sm" variant="outline" onClick={() => mutation.mutate(id, { onSuccess: () => toast.success("Withdrawal retrying") })} disabled={mutation.isPending}>
      {mutation.isPending ? "Retrying..." : "Retry"}
    </Button>
  );
}

export function WithdrawalsClient() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  const { data, isLoading, isError } = useWithdrawals({
    status: statusFilter || undefined,
  });
  const withdrawals = data ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Withdrawals" description="Outbound withdrawal tracking" />

      <div className="flex gap-2">
        <Select
          value={statusFilter || "all"}
          onValueChange={(v) => setStatusFilter(!v || v === "all" ? "" : v)}
        >
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
            <div key={i} className="h-10 animate-shimmer rounded" />
          ))}
        </div>
      ) : isError ? (
        <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-8 text-center">
          <AlertCircle className="mx-auto h-8 w-8 text-destructive mb-2" />
          <p className="text-sm font-medium">Failed to load withdrawals</p>
        </div>
      ) : withdrawals.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-12 text-center">
          <ArrowUpFromLine className="mx-auto h-8 w-8 text-muted-foreground mb-2" />
          <p className="text-sm font-medium">No withdrawals found</p>
          <p className="text-xs text-muted-foreground mt-1">
            {statusFilter ? "Try a different status filter." : "No withdrawals have been created yet."}
          </p>
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
                      {w.status === "locked" && <ReserveButton id={w.id} />}
                      {w.status === "reviewing" && <ReviewButtons id={w.id} />}
                      {w.status === "reserved" && <ProcessDialog id={w.id} />}
                      {w.status === "processing" && (
                        <>
                          <ConfirmButton id={w.id} />
                          <FailDialog id={w.id} />
                        </>
                      )}
                      {w.status === "failed" && <RetryButton id={w.id} />}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </>
      )}
    </div>
  );
}
