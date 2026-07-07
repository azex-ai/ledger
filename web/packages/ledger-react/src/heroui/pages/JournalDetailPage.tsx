"use client";

import { useState } from "react";
import { Button, Card, Chip, Input, Label, Modal, Skeleton, Table, TextField, toast } from "@heroui/react";
import { formatAmount, formatUTC } from "../../lib/utils";
import { useJournal, useReverseJournal } from "../../hooks/use-journals";
import { useJournalTypes } from "../../hooks/use-metadata";
import { DefaultLink, type LinkComponent } from "../../components/nav";
import { ErrorState, PageHeader, StatusChip, TableSkeleton } from "../shared";
import type { Entry } from "../../client/types";

export interface JournalDetailPageProps {
  /** Journal id — the host extracts this from its route param and passes it. */
  id: string;
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so the
   * page works without a host router. Used by the reversal-of link to
   * `/journals/{id}`.
   */
  linkComponent?: LinkComponent;
}

/** Entry direction → Chip color. Not a lifecycle status, so it doesn't route
 * through the shared StatusChip's status color map. */
const ENTRY_TYPE_COLOR = { debit: "success", credit: "danger" } as const;

function EntryTypeChip({ entryType }: { entryType: Entry["entry_type"] }) {
  return (
    <Chip color={ENTRY_TYPE_COLOR[entryType]} size="sm">
      {entryType}
    </Chip>
  );
}

function EntryFlow({ entries }: { entries: Entry[] }) {
  const debits = entries.filter((e) => e.entry_type === "debit");
  const credits = entries.filter((e) => e.entry_type === "credit");

  return (
    <Card>
      <Card.Header>
        <Card.Title className="text-sm font-medium">Fund Flow</Card.Title>
      </Card.Header>
      <Card.Content>
        <div className="flex items-start gap-8">
          <div className="flex-1 space-y-2">
            <p className="text-xs font-medium uppercase text-muted">Debit</p>
            {debits.map((e) => (
              <div key={`${e.entry_type}-${e.account_holder}-${e.classification_uid}`} className="rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-3">
                <div className="flex justify-between">
                  <span className="text-sm">Holder {e.account_holder}</span>
                  <span className="tabular-nums text-sm text-emerald-500">{formatAmount(e.amount)}</span>
                </div>
                <p className="text-xs text-muted">
                  Class {e.classification_uid} / Currency {e.currency_uid}
                </p>
              </div>
            ))}
          </div>
          <div className="flex flex-col items-center justify-center pt-6" aria-hidden="true">
            <div className="text-2xl text-muted">&rarr;</div>
          </div>
          <div className="flex-1 space-y-2">
            <p className="text-xs font-medium uppercase text-muted">Credit</p>
            {credits.map((e) => (
              <div key={`${e.entry_type}-${e.account_holder}-${e.classification_uid}`} className="rounded-lg border border-rose-500/20 bg-rose-500/5 p-3">
                <div className="flex justify-between">
                  <span className="text-sm">Holder {e.account_holder}</span>
                  <span className="tabular-nums text-sm text-rose-500">{formatAmount(e.amount)}</span>
                </div>
                <p className="text-xs text-muted">
                  Class {e.classification_uid} / Currency {e.currency_uid}
                </p>
              </div>
            ))}
          </div>
        </div>
      </Card.Content>
    </Card>
  );
}

function ReverseDialog({ journalId }: { journalId: string }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useReverseJournal();

  return (
    <>
      <Button size="sm" variant="danger" onPress={() => setOpen(true)}>
        Reverse
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container size="sm">
          <Modal.Dialog>
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Reverse Journal #{journalId}</Modal.Heading>
            </Modal.Header>
            <Modal.Body>
              <TextField fullWidth value={reason} onChange={setReason}>
                <Label>Reason</Label>
                <Input placeholder="duplicate deposit" />
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button variant="secondary" onPress={() => setOpen(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                isDisabled={!reason}
                onPress={() =>
                  mutation.mutate(
                    { id: journalId, reason },
                    {
                      onSuccess: () => {
                        toast.success("Journal reversed");
                        setOpen(false);
                      },
                    },
                  )
                }
              >
                {mutation.isPending ? "Reversing..." : "Confirm Reverse"}
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

export function JournalDetailPage({ id, linkComponent: Link = DefaultLink }: JournalDetailPageProps) {
  const { data, isLoading, isError } = useJournal(id);
  const { data: journalTypes } = useJournalTypes();

  if (isLoading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-8 w-64 rounded-lg" aria-hidden />
        <TableSkeleton rows={6} />
      </div>
    );
  }

  if (isError) {
    return <ErrorState message="Failed to load journal" />;
  }

  if (!data) {
    return <p className="text-muted">Journal not found</p>;
  }

  const { journal: j, entries } = data;

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Journal #${j.uid}`}
        actions={
          <>
            {j.reversal_of_uid && (
              <Link href={`/journals/${j.reversal_of_uid}`}>
                <StatusChip status="reversed" />
              </Link>
            )}
            <ReverseDialog journalId={j.uid} />
          </>
        }
      />

      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <Card>
          <Card.Content>
            <p className="text-xs text-muted">Type</p>
            <p className="text-lg font-bold">
              {journalTypes?.find((t) => t.uid === j.journal_type_uid)?.name ??
                j.journal_type_uid}
            </p>
          </Card.Content>
        </Card>
        <Card>
          <Card.Content>
            <p className="text-xs text-muted">Source</p>
            <p className="text-lg font-bold">{j.source}</p>
          </Card.Content>
        </Card>
        <Card>
          <Card.Content>
            <p className="text-xs text-muted">Total Debit</p>
            <p className="tabular-nums text-lg font-bold">{formatAmount(j.total_debit)}</p>
          </Card.Content>
        </Card>
        <Card>
          <Card.Content>
            <p className="text-xs text-muted">Total Credit</p>
            <p className="tabular-nums text-lg font-bold">{formatAmount(j.total_credit)}</p>
          </Card.Content>
        </Card>
      </div>

      <Card>
        <Card.Header>
          <Card.Title className="text-sm font-medium">Details</Card.Title>
        </Card.Header>
        <Card.Content className="space-y-2 text-sm">
          <div className="flex justify-between">
            <span className="text-muted">Idempotency Key</span>
            <span className="font-mono">{j.idempotency_key}</span>
          </div>
          <div className="flex justify-between">
            <span className="text-muted">Created At</span>
            <span>{formatUTC(j.created_at)}</span>
          </div>
          {j.actor_id !== 0 && (
            <div className="flex justify-between">
              <span className="text-muted">Actor ID</span>
              <span>{j.actor_id}</span>
            </div>
          )}
          {j.metadata && Object.keys(j.metadata).length > 0 && (
            <div>
              <span className="text-muted">Metadata</span>
              <pre className="mt-1 rounded-lg bg-surface-secondary p-2 font-mono text-xs">
                {JSON.stringify(j.metadata, null, 2)}
              </pre>
            </div>
          )}
        </Card.Content>
      </Card>

      <EntryFlow entries={entries} />

      <Card>
        <Card.Header>
          <Card.Title className="text-sm font-medium">Entries</Card.Title>
        </Card.Header>
        <Card.Content>
          <Table>
            <Table.ScrollContainer>
              <Table.Content aria-label="Journal entries" className="min-w-[600px]">
                <Table.Header>
                  <Table.Column isRowHeader>Holder</Table.Column>
                  <Table.Column>Currency</Table.Column>
                  <Table.Column>Classification</Table.Column>
                  <Table.Column>Type</Table.Column>
                  <Table.Column className="text-end">Amount</Table.Column>
                </Table.Header>
                <Table.Body>
                  {entries.map((e) => {
                    const rowId = `${e.entry_type}-${e.account_holder}-${e.classification_uid}`;
                    return (
                      <Table.Row key={rowId} id={rowId}>
                        <Table.Cell>{e.account_holder}</Table.Cell>
                        <Table.Cell>{e.currency_uid}</Table.Cell>
                        <Table.Cell>{e.classification_uid}</Table.Cell>
                        <Table.Cell>
                          <EntryTypeChip entryType={e.entry_type} />
                        </Table.Cell>
                        <Table.Cell className="text-end tabular-nums">{formatAmount(e.amount)}</Table.Cell>
                      </Table.Row>
                    );
                  })}
                </Table.Body>
              </Table.Content>
            </Table.ScrollContainer>
          </Table>
        </Card.Content>
      </Card>
    </div>
  );
}
