"use client";

import { useMemo, useRef, useState } from "react";
import {
  AlertDialog,
  Button,
  Input,
  Label,
  ListBox,
  Modal,
  Select,
  Table,
  TextField,
  toast,
} from "@heroui/react";
import {
  useFinalizeReservationSettlement,
  useReleaseReservation,
  useReservations,
  useSettlePartialReservation,
  useSettleReservation,
} from "../../hooks/use-reservations";
import { formatAmount, formatUTC, validateAmount } from "../../lib/utils";
import { EmptyState, ErrorState, PageHeader, StatusChip, TableSkeleton } from "../shared";
import { LoadMoreBar } from "../pagination-bar";

const STATUS_OPTIONS = ["all", "active", "settling", "settled", "released"] as const;

function SettleModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  const mutation = useSettleReservation();

  const submit = () => {
    const amountErr = validateAmount(amount);
    if (amountErr) {
      toast.danger(amountErr);
      return;
    }
    mutation.mutate(
      { id, actualAmount: amount },
      {
        onSuccess: () => {
          toast.success("Reservation settled");
          setOpen(false);
          setAmount("");
        },
        onError: () => toast.danger("Failed to settle reservation"),
      },
    );
  };

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Settle
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Settle Reservation #{id}</Modal.Heading>
            </Modal.Header>
            <Modal.Body>
              <TextField autoFocus aria-label="Actual Amount" value={amount} onChange={setAmount}>
                <Input placeholder="95.50" />
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button
                variant="secondary"
                isDisabled={mutation.isPending}
                onPress={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button isPending={mutation.isPending} isDisabled={!amount} onPress={submit}>
                Settle
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function SettlePartialModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  // Generated once per dialog open, reused across retries of this submission
  // (api-contract.md §9) — never regenerated inside the retry path.
  const idempotencyKeyRef = useRef("");
  const mutation = useSettlePartialReservation();

  const openModal = () => {
    idempotencyKeyRef.current = crypto.randomUUID();
    setOpen(true);
  };

  const submit = () => {
    const amountErr = validateAmount(amount);
    if (amountErr) {
      toast.danger(amountErr);
      return;
    }
    mutation.mutate(
      { id, amount, idempotencyKey: idempotencyKeyRef.current },
      {
        onSuccess: () => {
          toast.success("Partial settlement recorded");
          setOpen(false);
          setAmount("");
        },
        onError: () => toast.danger("Failed to record partial settlement"),
      },
    );
  };

  return (
    <>
      <Button size="sm" variant="tertiary" onPress={openModal}>
        Settle Partial
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Partially Settle Reservation #{id}</Modal.Heading>
            </Modal.Header>
            <Modal.Body>
              <TextField autoFocus aria-label="Partial Amount" value={amount} onChange={setAmount}>
                <Input placeholder="25.00" />
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button
                variant="secondary"
                isDisabled={mutation.isPending}
                onPress={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button isPending={mutation.isPending} isDisabled={!amount} onPress={submit}>
                Settle Partial
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function FinalizeConfirm({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useFinalizeReservationSettlement();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Finalize
      </Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="accent" />
              <AlertDialog.Heading>Finalize Reservation #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This closes out the reservation after its partial settlements. This action
                cannot be undone.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button
                variant="tertiary"
                isDisabled={mutation.isPending}
                onPress={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(id, {
                    onSuccess: () => {
                      toast.success("Reservation finalized");
                      setOpen(false);
                    },
                    onError: () => toast.danger("Failed to finalize reservation"),
                  })
                }
              >
                Finalize
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

function ReleaseConfirm({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useReleaseReservation();

  return (
    <>
      <Button size="sm" variant="ghost" onPress={() => setOpen(true)}>
        Release
      </Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="danger" />
              <AlertDialog.Heading>Release Reservation #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This will release the reserved funds back to the account. This action cannot be
                undone.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button
                variant="tertiary"
                isDisabled={mutation.isPending}
                onPress={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(id, {
                    onSuccess: () => {
                      toast.success("Reservation released");
                      setOpen(false);
                    },
                    onError: () => toast.danger("Failed to release reservation"),
                  })
                }
              >
                Release
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

export function ReservationsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const params = useMemo(() => ({ status: statusFilter || undefined }), [statusFilter]);
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useReservations(params);
  const reservations = data?.pages.flatMap((p) => p.list) ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Reservations" description="Balance reservations (pessimistic locks)" />

      <div className="flex gap-2">
        <Select
          className="w-40"
          value={statusFilter || "all"}
          onChange={(key) => setStatusFilter(key && key !== "all" ? String(key) : "")}
        >
          <Label className="sr-only">Status</Label>
          <Select.Trigger>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              {STATUS_OPTIONS.map((s) => (
                <ListBox.Item key={s} id={s} textValue={s === "all" ? "All" : s}>
                  {s === "all" ? "All" : s}
                  <ListBox.ItemIndicator />
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load reservations" />
      ) : reservations.length === 0 ? (
        <EmptyState
          title="No reservations found"
          description={
            statusFilter ? "Try a different status filter." : "No reservations have been created yet."
          }
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Reservations" className="min-w-[820px]">
              <Table.Header>
                <Table.Column isRowHeader>ID</Table.Column>
                <Table.Column>Holder</Table.Column>
                <Table.Column>Currency</Table.Column>
                <Table.Column className="text-end">Reserved</Table.Column>
                <Table.Column className="text-end">Settled</Table.Column>
                <Table.Column>Status</Table.Column>
                <Table.Column>Expires</Table.Column>
                <Table.Column>Actions</Table.Column>
              </Table.Header>
              <Table.Body items={reservations}>
                {(r) => (
                  <Table.Row id={r.uid}>
                    <Table.Cell className="max-w-32"><span className="block truncate" title={r.uid}>#{r.uid}</span></Table.Cell>
                    <Table.Cell>{r.account_holder}</Table.Cell>
                    <Table.Cell>{r.currency_uid}</Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {formatAmount(r.reserved_amount)}
                    </Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {r.settled_amount && r.settled_amount !== "0"
                        ? formatAmount(r.settled_amount)
                        : "—"}
                    </Table.Cell>
                    <Table.Cell>
                      <StatusChip status={r.status} />
                    </Table.Cell>
                    <Table.Cell className="text-muted text-xs">
                      {formatUTC(r.expires_at)}
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex flex-wrap gap-1">
                        {r.status === "active" && (
                          <>
                            <SettleModal id={r.uid} />
                            <SettlePartialModal id={r.uid} />
                            <ReleaseConfirm id={r.uid} />
                          </>
                        )}
                        {r.status === "settling" && <FinalizeConfirm id={r.uid} />}
                      </div>
                    </Table.Cell>
                  </Table.Row>
                )}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </Table>
      )}
    </div>
  );
}
