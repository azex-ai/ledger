"use client";

import { ExactAmount } from "../exact-amount";

import { errorText } from "../../lib/error-message";
import { useMemo, useState } from "react";
import { validateAmount, formatUTC } from "../../lib/utils";
import {
  useFinalizeReservationSettlement,
  useReservations,
  useReleaseReservation,
  useSettlePartialReservation,
  useSettleReservation,
} from "../../hooks/use-reservations";
import { useUidCodeLookups } from "../../hooks/use-metadata";
import { usePayloadIdempotencyKey } from "../../hooks/use-idempotency-key";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogTrigger, DialogFooter,
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
import { LoadMoreBar } from "../pagination-bar";

function SettleDialog({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  // One key per submitted amount — settle's receipt matches on the amount, so a
  // corrected amount after a rejection must not replay the previous amount's key
  // (M2, web audit). See use-idempotency-key.ts.
  const idempotencyKey = usePayloadIdempotencyKey();
  const mutation = useSettleReservation();

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) idempotencyKey.reset();
        setOpen(next);
      }}
    >
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Settle</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Settle Reservation #{id}</DialogTitle>
          <DialogDescription>Records the settled amount and releases the entire hold. Booked balances do not change; this does not post a charge.</DialogDescription>
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
              mutation.mutate({ id, actualAmount: amount, idempotencyKey: idempotencyKey.keyFor(amount) }, {
                onSuccess: () => {
                  toast.success("Reservation settled");
                  setOpen(false);
                },
                onError: (err) => toast.error(errorText(err, "Failed to settle reservation")),
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

function SettlePartialDialog({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  // One key per submitted amount, not per dialog open — a corrected amount
  // after a rejection must not replay the previous amount's key (M7,
  // 2026-08-26 web audit). See use-idempotency-key.ts.
  const idempotencyKey = usePayloadIdempotencyKey();
  const mutation = useSettlePartialReservation();

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) idempotencyKey.reset();
        setOpen(next);
      }}
    >
      <DialogTrigger render={<Button size="sm" variant="secondary" />}>Settle Partial</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Partially Settle Reservation #{id}</DialogTitle>
          <DialogDescription>Records this settlement and reduces the hold by this amount. Booked balances do not change; this does not post a charge.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label htmlFor="rsv-settle-partial-amount">Partial Amount</Label>
            <Input id="rsv-settle-partial-amount" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="25.00" />
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
              mutation.mutate({ id, amount, idempotencyKey: idempotencyKey.keyFor(amount) }, {
                onSuccess: () => {
                  toast.success("Partial settlement recorded");
                  setOpen(false);
                  setAmount("");
                },
                onError: (err) => toast.error(errorText(err, "Failed to record partial settlement")),
              });
            }}
            disabled={mutation.isPending || !amount}
          >
            {mutation.isPending ? "Settling..." : "Settle Partial"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function FinalizeConfirmDialog({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useFinalizeReservationSettlement();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Finalize</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Finalize Reservation #{id}</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground py-4">
          Closes the reservation and releases any remaining hold after partial settlements. Booked balances do not change; this does not post a charge. This action cannot be undone.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            onClick={() => mutation.mutate(id, {
              onSuccess: () => {
                toast.success("Reservation finalized");
                setOpen(false);
              },
              onError: (err) => toast.error(errorText(err, "Failed to finalize reservation")),
            })}
            disabled={mutation.isPending}
          >
            {mutation.isPending ? "Finalizing..." : "Finalize"}
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
          Releases the hold without changing booked balances. This action cannot be undone.
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
              onError: (err) => toast.error(errorText(err, "Failed to release reservation")),
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
  const { data, isLoading, isError, refetch, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useReservations(params);
  const { currencyCode } = useUidCodeLookups();
  const reservations = data?.pages.flatMap((p) => p.list) ?? [];

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
        <ErrorState message="Failed to load reservations" onRetry={refetch} />
      ) : reservations.length === 0 ? (
        <EmptyState
          icon={Lock}
          title="No reservations found"
          description={statusFilter ? "Try a different status filter." : "No reservations have been created yet."}
        />
      ) : (
        <>
          <Table aria-label="Reservations" className="min-w-[900px]">
            <TableHeader>
              <TableRow>
                <TableHead className="w-[220px]">ID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead>Currency</TableHead>
                <TableHead className="text-right">Reserved amount</TableHead>
                <TableHead className="text-right">Recorded settlement</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {reservations.map((r) => (
                <TableRow key={r.uid}>
                  <TableCell className="max-w-[220px]">
                    <span className="block truncate font-mono text-xs" title={r.uid}>#{r.uid}</span>
                  </TableCell>
                  <TableCell>{r.account_holder}</TableCell>
                  <TableCell title={r.currency_uid}>{currencyCode(r.currency_uid)}</TableCell>
                  <TableCell className="text-right tabular-nums"><ExactAmount value={r.reserved_amount} currencyCode={currencyCode(r.currency_uid)} /></TableCell>
                  <TableCell className="text-right tabular-nums">{r.settled_amount !== null && r.settled_amount !== undefined && r.settled_amount !== "" ? <ExactAmount value={r.settled_amount} currencyCode={currencyCode(r.currency_uid)} /> : "-"}</TableCell>
                  <TableCell><StatusBadge status={r.status} /></TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {formatUTC(r.expires_at)}
                  </TableCell>
                  <TableCell>
                    <div className="flex gap-1 flex-wrap">
                      {r.status === "active" && (
                        <>
                          <SettleDialog id={r.uid} />
                          <SettlePartialDialog id={r.uid} />
                          <ReleaseConfirmDialog id={r.uid} />
                        </>
                      )}
                      {r.status === "settling" && <FinalizeConfirmDialog id={r.uid} />}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </>
      )}
    </div>
  );
}
