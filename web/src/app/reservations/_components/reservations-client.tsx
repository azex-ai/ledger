"use client";

import { useState } from "react";
import { useReservations, useSettleReservation, useReleaseReservation } from "@/lib/hooks/use-reservations";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { AlertCircle, Lock } from "lucide-react";
import { toast } from "sonner";

function SettleDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  const mutation = useSettleReservation();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Settle</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Settle Reservation #{id}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="rsv-settle-amount">Actual Amount</Label>
            <Input id="rsv-settle-amount" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="95.50" />
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={() => {
              const DECIMAL_RE = /^\d+(\.\d+)?$/;
              if (!DECIMAL_RE.test(amount)) {
                toast.error("Amount must be a valid decimal number");
                return;
              }
              mutation.mutate({ id, actualAmount: amount }, {
                onSuccess: () => {
                  toast.success("Reservation settled");
                  setOpen(false);
                },
              });
            }}
            disabled={mutation.isPending || !amount}
          >
            {mutation.isPending ? "Settling..." : "Settle"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ReleaseConfirmDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const mutation = useReleaseReservation();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" />}>Release</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Release Reservation #{id}</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground py-4">
          This will release the reserved funds back to the account. This action cannot be undone.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() => mutation.mutate(id, {
              onSuccess: () => {
                toast.success("Reservation released");
                setOpen(false);
              },
            })}
            disabled={mutation.isPending}
          >
            {mutation.isPending ? "Releasing..." : "Release"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function ReservationsClient() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  const { data, isLoading, isError } = useReservations({
    status: statusFilter || undefined,
  });
  const reservations = data ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Reservations" description="Balance reservations (pessimistic locks)" />

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
            <SelectItem value="active">Active</SelectItem>
            <SelectItem value="settling">Settling</SelectItem>
            <SelectItem value="settled">Settled</SelectItem>
            <SelectItem value="released">Released</SelectItem>
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
          <p className="text-sm font-medium">Failed to load reservations</p>
        </div>
      ) : reservations.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-12 text-center">
          <Lock className="mx-auto h-8 w-8 text-muted-foreground mb-2" />
          <p className="text-sm font-medium">No reservations found</p>
          <p className="text-xs text-muted-foreground mt-1">
            {statusFilter ? "Try a different status filter." : "No reservations have been created yet."}
          </p>
        </div>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead>Currency</TableHead>
                <TableHead className="text-right">Reserved</TableHead>
                <TableHead className="text-right">Settled</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {reservations.map((r) => (
                <TableRow key={r.id}>
                  <TableCell>#{r.id}</TableCell>
                  <TableCell>{r.account_holder}</TableCell>
                  <TableCell>{r.currency_id}</TableCell>
                  <TableCell className="text-right font-mono">{r.reserved_amount}</TableCell>
                  <TableCell className="text-right font-mono">{r.settled_amount ?? "-"}</TableCell>
                  <TableCell><StatusBadge status={r.status} /></TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(r.expires_at).toLocaleString()}
                  </TableCell>
                  <TableCell>
                    {r.status === "active" && (
                      <div className="flex gap-1">
                        <SettleDialog id={r.id} />
                        <ReleaseConfirmDialog id={r.id} />
                      </div>
                    )}
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
