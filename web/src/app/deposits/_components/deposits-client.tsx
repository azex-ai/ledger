"use client";

import { useState } from "react";
import { formatAmount, validateAmount } from "@/lib/utils";
import { useDeposits, useConfirmingDeposit, useConfirmDeposit, useFailDeposit } from "@/lib/hooks/use-deposits";
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
import { ArrowDownToLine } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "@/components/error-state";
import { EmptyState } from "@/components/empty-state";
import { TableSkeleton } from "@/components/loading-skeleton";

const DEPOSIT_STATES = ["pending", "confirming", "confirmed", "failed", "expired"];

function DepositStepper({ status }: { status: string }) {
  const idx = DEPOSIT_STATES.indexOf(status);
  return (
    <div className="flex items-center gap-1" role="img" aria-label={`Status: ${status}`}>
      {DEPOSIT_STATES.map((s, i) => (
        <div key={s} className="flex items-center gap-1">
          <div
            className={`h-2 w-2 rounded-full ${
              i < idx
                ? "bg-emerald-400"
                : i === idx && (status === "failed" || status === "expired")
                  ? "bg-rose-400"
                  : i === idx
                    ? "bg-emerald-400"
                    : "bg-muted"
            }`}
          />
          {i < DEPOSIT_STATES.length - 1 && (
            <div className={`h-px w-4 ${i < idx ? "bg-emerald-400" : "bg-muted"}`} />
          )}
        </div>
      ))}
    </div>
  );
}

function ConfirmingDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [channelRef, setChannelRef] = useState("");
  const mutation = useConfirmingDeposit();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Confirming</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Move Deposit #{id} to Confirming</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="dep-confirming-ref">Channel Reference</Label>
            <Input id="dep-confirming-ref" value={channelRef} onChange={(e) => setChannelRef(e.target.value)} placeholder="0xabc... or tx hash" />
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={() => mutation.mutate({ id, channelRef: channelRef || "manual" }, {
              onSuccess: () => {
                toast.success("Deposit moved to confirming");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending}
          >
            {mutation.isPending ? "Updating..." : "Confirm"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ConfirmDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  const [channelRef, setChannelRef] = useState("");
  const mutation = useConfirmDeposit();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Confirm</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Confirm Deposit #{id}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="dep-confirm-amount">Actual Amount</Label>
            <Input id="dep-confirm-amount" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="500.00" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="dep-confirm-ref">Channel Ref</Label>
            <Input id="dep-confirm-ref" value={channelRef} onChange={(e) => setChannelRef(e.target.value)} placeholder="0xabc..." />
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={() => {
              const amountErr = validateAmount(amount);
              if (amountErr) {
                toast.error(amountErr);
                return;
              }
              mutation.mutate({ id, actual_amount: amount, channel_ref: channelRef }, {
                onSuccess: () => {
                  toast.success("Deposit confirmed");
                  setOpen(false);
                },
              });
            }}
            disabled={mutation.isPending || !amount || !channelRef}
            title={
              mutation.isPending
                ? "Submitting…"
                : !amount
                  ? "Enter the actual settled amount"
                  : !channelRef
                    ? "Enter the channel reference (tx hash)"
                    : undefined
            }
          >
            {mutation.isPending ? "Confirming..." : "Confirm"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function FailDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useFailDeposit();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Fail</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Fail Deposit #{id}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <p className="text-sm text-muted-foreground">This will mark the deposit as failed. This action cannot be undone.</p>
          <div className="grid gap-2">
            <Label htmlFor="dep-fail-reason">Reason</Label>
            <Input id="dep-fail-reason" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Invalid transaction, timeout, etc." />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate({ id, reason: reason || "manual fail" }, {
              onSuccess: () => {
                toast.success("Deposit marked as failed");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending}
          >
            {mutation.isPending ? "Failing..." : "Fail Deposit"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function DepositsClient() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  const { data, isLoading, isError } = useDeposits({
    status: statusFilter || undefined,
  });
  const deposits = data ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Deposits" description="Inbound deposit tracking" />

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
            {DEPOSIT_STATES.map((s) => (
              <SelectItem key={s} value={s}>{s}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load deposits" />
      ) : deposits.length === 0 ? (
        <EmptyState
          icon={ArrowDownToLine}
          title="No deposits found"
          description={statusFilter ? "Try a different status filter." : "No deposits have been created yet."}
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead className="text-right">Expected</TableHead>
                <TableHead className="text-right">Actual</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Progress</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {deposits.map((d) => (
                <TableRow key={d.id}>
                  <TableCell>#{d.id}</TableCell>
                  <TableCell>{d.account_holder}</TableCell>
                  <TableCell>{d.channel_name}</TableCell>
                  <TableCell className="text-right font-mono">{formatAmount(d.amount)}</TableCell>
                  <TableCell className="text-right font-mono">
                    {d.settled_amount && d.settled_amount !== "0" ? formatAmount(d.settled_amount) : "—"}
                  </TableCell>
                  <TableCell><StatusBadge status={d.status} /></TableCell>
                  <TableCell><DepositStepper status={d.status} /></TableCell>
                  <TableCell>
                    <div className="flex gap-1">
                      {d.status === "pending" && <ConfirmingDialog id={d.id} />}
                      {d.status === "confirming" && <ConfirmDialog id={d.id} />}
                      {(d.status === "pending" || d.status === "confirming") && (
                        <FailDialog id={d.id} />
                      )}
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
