"use client";

import { useMemo, useState } from "react";
import { formatAmount, validateAmount, formatUTC } from "../../lib/utils";
import {
  useReservations,
  useSettleReservation,
  useReleaseReservation,
} from "../../hooks/use-reservations";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "../ui/dialog";
import { Label } from "../ui/label";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "../ui/select";
import { Lock } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";

function SettleDialog({ id }: { id: string }) {
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
              const amountErr = validateAmount(amount);
              if (amountErr) {
                toast.error(amountErr);
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

function ReleaseConfirmDialog({ id }: { id: string }) {
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

export function ReservationsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const params = useMemo(
    () => ({ status: statusFilter || undefined }),
    [statusFilter],
  );
  const { data, isLoading, isError } = useReservations(params);
  const reservations = data?.list ?? [];

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
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load reservations" />
      ) : reservations.length === 0 ? (
        <EmptyState
          icon={Lock}
          title="No reservations found"
          description={statusFilter ? "Try a different status filter." : "No reservations have been created yet."}
        />
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
                <TableRow key={r.uid}>
                  <TableCell>#{r.uid}</TableCell>
                  <TableCell>{r.account_holder}</TableCell>
                  <TableCell>{r.currency_uid}</TableCell>
                  <TableCell className="text-right font-mono">{formatAmount(r.reserved_amount)}</TableCell>
                  <TableCell className="text-right font-mono">{r.settled_amount ? formatAmount(r.settled_amount) : "-"}</TableCell>
                  <TableCell><StatusBadge status={r.status} /></TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {formatUTC(r.expires_at)}
                  </TableCell>
                  <TableCell>
                    {r.status === "active" && (
                      <div className="flex gap-1">
                        <SettleDialog id={r.uid} />
                        <ReleaseConfirmDialog id={r.uid} />
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
