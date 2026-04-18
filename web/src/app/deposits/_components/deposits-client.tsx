"use client";

import { useState } from "react";
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
import { AlertCircle, ArrowDownToLine } from "lucide-react";
import { toast } from "sonner";

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
                ? "bg-green-400"
                : i === idx && (status === "failed" || status === "expired")
                  ? "bg-red-400"
                  : i === idx
                    ? "bg-green-400"
                    : "bg-muted"
            }`}
          />
          {i < DEPOSIT_STATES.length - 1 && (
            <div className={`h-px w-4 ${i < idx ? "bg-green-400" : "bg-muted"}`} />
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
            <Label>Channel Reference</Label>
            <Input value={channelRef} onChange={(e) => setChannelRef(e.target.value)} placeholder="0xabc... or tx hash" />
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
            <Label>Actual Amount</Label>
            <Input value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="500.00" />
          </div>
          <div className="grid gap-2">
            <Label>Channel Ref</Label>
            <Input value={channelRef} onChange={(e) => setChannelRef(e.target.value)} placeholder="0xabc..." />
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={() => mutation.mutate({ id, actual_amount: amount, channel_ref: channelRef }, {
              onSuccess: () => {
                toast.success("Deposit confirmed");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending || !amount || !channelRef}
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
            <Label>Reason</Label>
            <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Invalid transaction, timeout, etc." />
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
        <div className="space-y-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="h-10 animate-pulse rounded bg-muted" />
          ))}
        </div>
      ) : isError ? (
        <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-8 text-center">
          <AlertCircle className="mx-auto h-8 w-8 text-destructive mb-2" />
          <p className="text-sm font-medium">Failed to load deposits</p>
        </div>
      ) : deposits.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-12 text-center">
          <ArrowDownToLine className="mx-auto h-8 w-8 text-muted-foreground mb-2" />
          <p className="text-sm font-medium">No deposits found</p>
          <p className="text-xs text-muted-foreground mt-1">
            {statusFilter ? "Try a different status filter." : "No deposits have been created yet."}
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
                  <TableCell className="text-right font-mono">{d.expected_amount}</TableCell>
                  <TableCell className="text-right font-mono">{d.actual_amount ?? "-"}</TableCell>
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
