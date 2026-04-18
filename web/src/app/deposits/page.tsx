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

const DEPOSIT_STATES = ["pending", "confirming", "confirmed", "failed", "expired"];

function DepositStepper({ status }: { status: string }) {
  const idx = DEPOSIT_STATES.indexOf(status);
  return (
    <div className="flex items-center gap-1">
      {DEPOSIT_STATES.map((s, i) => (
        <div key={s} className="flex items-center gap-1">
          <div
            className={`h-2 w-2 rounded-full ${
              i <= idx
                ? status === "failed" || status === "expired"
                  ? "bg-red-400"
                  : "bg-green-400"
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
            onClick={() => mutation.mutate({ id, actual_amount: amount, channel_ref: channelRef }, { onSuccess: () => setOpen(false) })}
            disabled={mutation.isPending || !amount || !channelRef}
          >
            {mutation.isPending ? "Confirming..." : "Confirm"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function DepositsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  const { data, isLoading, hasNextPage, fetchNextPage, isFetchingNextPage } = useDeposits({
    status: statusFilter || undefined,
  });
  const deposits = data?.pages.flatMap((p) => p.data) ?? [];
  const confirmingMutation = useConfirmingDeposit();
  const failMutation = useFailDeposit();

  return (
    <div className="space-y-6">
      <PageHeader title="Deposits" description="Inbound deposit tracking" />

      <div className="flex gap-2">
        <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v ?? "")}>
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
                      {d.status === "pending" && (
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => confirmingMutation.mutate({ id: d.id, channelRef: "manual" })}
                          disabled={confirmingMutation.isPending}
                        >
                          Confirming
                        </Button>
                      )}
                      {d.status === "confirming" && <ConfirmDialog id={d.id} />}
                      {(d.status === "pending" || d.status === "confirming") && (
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => failMutation.mutate({ id: d.id, reason: "manual fail" })}
                          disabled={failMutation.isPending}
                        >
                          Fail
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
