"use client";

import { useMemo, useState } from "react";
import {
  Button,
  Input,
  Label,
  ListBox,
  Modal,
  Select,
  Table,
  TextField,
  cn,
  toast,
} from "@heroui/react";
import {
  useConfirmDeposit,
  useConfirmingDeposit,
  useDeposits,
  useFailDeposit,
} from "../../hooks/use-deposits";
import { formatAmount, validateAmount } from "../../lib/utils";
import { EmptyState, ErrorState, PageHeader, StatusChip, TableSkeleton } from "../shared";
import { LoadMoreBar } from "../pagination-bar";

const DEPOSIT_STATES = ["pending", "confirming", "confirmed", "failed", "expired"];

function DepositStepper({ status }: { status: string }) {
  const idx = DEPOSIT_STATES.indexOf(status);
  return (
    <div className="flex items-center gap-1" role="img" aria-label={`Status: ${status}`}>
      {DEPOSIT_STATES.map((s, i) => (
        <div key={s} className="flex items-center gap-1">
          <div
            className={cn(
              "h-2 w-2 rounded-full",
              i < idx
                ? "bg-success"
                : i === idx && (status === "failed" || status === "expired")
                  ? "bg-danger"
                  : i === idx
                    ? "bg-success"
                    : "bg-muted",
            )}
          />
          {i < DEPOSIT_STATES.length - 1 && (
            <div className={cn("h-px w-4", i < idx ? "bg-success" : "bg-muted")} />
          )}
        </div>
      ))}
    </div>
  );
}

function ConfirmingModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [channelRef, setChannelRef] = useState("");
  const mutation = useConfirmingDeposit();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Confirming
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Move Deposit #{id} to Confirming</Modal.Heading>
            </Modal.Header>
            <Modal.Body>
              <TextField autoFocus aria-label="Channel Reference" value={channelRef} onChange={setChannelRef}>
                <Input placeholder="0xabc... or tx hash" />
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
              <Button
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(
                    { id, channelRef: channelRef || "manual" },
                    {
                      onSuccess: () => {
                        toast.success("Deposit moved to confirming");
                        setOpen(false);
                        setChannelRef("");
                      },
                      onError: () => toast.danger("Failed to update deposit"),
                    },
                  )
                }
              >
                Confirm
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function ConfirmModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  const [channelRef, setChannelRef] = useState("");
  const mutation = useConfirmDeposit();

  const submit = () => {
    const amountErr = validateAmount(amount);
    if (amountErr) {
      toast.danger(amountErr);
      return;
    }
    mutation.mutate(
      { id, actual_amount: amount, channel_ref: channelRef },
      {
        onSuccess: () => {
          toast.success("Deposit confirmed");
          setOpen(false);
          setAmount("");
          setChannelRef("");
        },
        onError: () => toast.danger("Failed to confirm deposit"),
      },
    );
  };

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Confirm
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Confirm Deposit #{id}</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <TextField autoFocus aria-label="Actual Amount" value={amount} onChange={setAmount}>
                <Input placeholder="500.00" />
              </TextField>
              <TextField aria-label="Channel Ref" value={channelRef} onChange={setChannelRef}>
                <Input placeholder="0xabc..." />
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
              <Button
                isPending={mutation.isPending}
                isDisabled={!amount || !channelRef}
                onPress={submit}
              >
                Confirm
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function FailModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useFailDeposit();

  return (
    <>
      <Button size="sm" variant="ghost" onPress={() => setOpen(true)}>
        Fail
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Fail Deposit #{id}</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <p className="text-muted text-sm">
                This will mark the deposit as failed. This action cannot be undone.
              </p>
              <TextField autoFocus aria-label="Reason" value={reason} onChange={setReason}>
                <Input placeholder="Invalid transaction, timeout, etc." />
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
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(
                    { id, reason: reason || "manual fail" },
                    {
                      onSuccess: () => {
                        toast.success("Deposit marked as failed");
                        setOpen(false);
                        setReason("");
                      },
                      onError: () => toast.danger("Failed to update deposit"),
                    },
                  )
                }
              >
                Fail Deposit
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

export function DepositsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const params = useMemo(() => ({ status: statusFilter || undefined }), [statusFilter]);
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useDeposits(params);
  const deposits = data?.pages.flatMap((p) => p.list) ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Deposits" description="Inbound deposit tracking" />

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
              <ListBox.Item id="all" textValue="All">
                All
                <ListBox.ItemIndicator />
              </ListBox.Item>
              {DEPOSIT_STATES.map((s) => (
                <ListBox.Item key={s} id={s} textValue={s}>
                  {s}
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
        <ErrorState message="Failed to load deposits" />
      ) : deposits.length === 0 ? (
        <EmptyState
          title="No deposits found"
          description={
            statusFilter ? "Try a different status filter." : "No deposits have been created yet."
          }
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Deposits" className="min-w-[860px]">
              <Table.Header>
                <Table.Column isRowHeader>ID</Table.Column>
                <Table.Column>Holder</Table.Column>
                <Table.Column>Channel</Table.Column>
                <Table.Column className="text-end">Expected</Table.Column>
                <Table.Column className="text-end">Actual</Table.Column>
                <Table.Column>Status</Table.Column>
                <Table.Column>Progress</Table.Column>
                <Table.Column>Actions</Table.Column>
              </Table.Header>
              <Table.Body items={deposits}>
                {(d) => (
                  <Table.Row id={d.uid}>
                    <Table.Cell className="max-w-32"><span className="block truncate" title={d.uid}>#{d.uid}</span></Table.Cell>
                    <Table.Cell>{d.account_holder}</Table.Cell>
                    <Table.Cell className="max-w-32"><span className="block truncate" title={d.channel_name}>{d.channel_name}</span></Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {formatAmount(d.amount)}
                    </Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {d.settled_amount && d.settled_amount !== "0"
                        ? formatAmount(d.settled_amount)
                        : "—"}
                    </Table.Cell>
                    <Table.Cell>
                      <StatusChip status={d.status} />
                    </Table.Cell>
                    <Table.Cell>
                      <DepositStepper status={d.status} />
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex flex-wrap gap-1">
                        {d.status === "pending" && <ConfirmingModal id={d.uid} />}
                        {d.status === "confirming" && <ConfirmModal id={d.uid} />}
                        {(d.status === "pending" || d.status === "confirming") && (
                          <FailModal id={d.uid} />
                        )}
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
