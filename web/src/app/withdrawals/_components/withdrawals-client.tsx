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
import { formatAmount, formatUTC } from "@/lib/utils";
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
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { ArrowUpFromLine } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import { EmptyState } from "@/components/empty-state";
import { TableSkeleton } from "@/components/loading-skeleton";

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
          <p className="text-sm text-muted-foreground">
            This will mark the withdrawal as failed. You can retry from the failed state.
          </p>
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
    <AlertDialog>
      <AlertDialogTrigger render={<Button size="sm" variant="outline" disabled={mutation.isPending} />}>
        {mutation.isPending ? "Reserving..." : "Reserve"}
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Reserve Withdrawal #{id}?</AlertDialogTitle>
          <AlertDialogDescription>
            This will lock funds for this withdrawal. The reserved amount will be deducted from available balance.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction onClick={() => mutation.mutate(id, { onSuccess: () => toast.success("Withdrawal reserved") })}>
            Reserve
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function ReviewButtons({ id }: { id: number }) {
  const mutation = useReviewWithdraw();
  return (
    <>
      <AlertDialog>
        <AlertDialogTrigger render={<Button size="sm" variant="outline" disabled={mutation.isPending} />}>
          {mutation.isPending ? "..." : "Approve"}
        </AlertDialogTrigger>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Approve Withdrawal #{id}?</AlertDialogTitle>
            <AlertDialogDescription>
              This will approve the withdrawal for processing.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => mutation.mutate({ id, approved: true }, { onSuccess: () => toast.success("Withdrawal approved") })}>
              Approve
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <AlertDialog>
        <AlertDialogTrigger render={<Button size="sm" variant="ghost" disabled={mutation.isPending} />}>
          {mutation.isPending ? "..." : "Reject"}
        </AlertDialogTrigger>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Reject Withdrawal #{id}?</AlertDialogTitle>
            <AlertDialogDescription>
              This will reject the withdrawal.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => mutation.mutate({ id, approved: false }, { onSuccess: () => toast.success("Withdrawal rejected") })}>
              Reject
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function ConfirmButton({ id }: { id: number }) {
  const mutation = useConfirmWithdraw();
  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button size="sm" variant="outline" disabled={mutation.isPending} />}>
        {mutation.isPending ? "Confirming..." : "Confirm"}
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Confirm Withdrawal #{id}?</AlertDialogTitle>
          <AlertDialogDescription>
            This will confirm the withdrawal as completed. This action cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction onClick={() => mutation.mutate(id, { onSuccess: () => toast.success("Withdrawal confirmed") })}>
            Confirm
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function RetryButton({ id }: { id: number }) {
  const mutation = useRetryWithdraw();
  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button size="sm" variant="outline" disabled={mutation.isPending} />}>
        {mutation.isPending ? "Retrying..." : "Retry"}
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Retry Withdrawal #{id}?</AlertDialogTitle>
          <AlertDialogDescription>
            This will retry the failed withdrawal by re-reserving funds.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction onClick={() => mutation.mutate(id, { onSuccess: () => toast.success("Withdrawal retrying") })}>
            Retry
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
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
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load withdrawals" />
      ) : withdrawals.length === 0 ? (
        <EmptyState
          icon={ArrowUpFromLine}
          title="No withdrawals found"
          description={statusFilter ? "Try a different status filter." : "No withdrawals have been created yet."}
        />
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
                  <TableCell className="text-right font-mono">{formatAmount(w.amount)}</TableCell>
                  <TableCell><StatusBadge status={w.status} /></TableCell>
                  <TableCell>{w.review_required ? "Required" : "Auto"}</TableCell>
                  <TableCell className="text-right text-xs text-muted-foreground">
                    {formatUTC(w.created_at)}
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
