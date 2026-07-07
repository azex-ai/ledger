"use client";

import { useState } from "react";
import {
  useCurrencies,
  useCreateCurrency,
  useDeactivateCurrency,
} from "../../hooks/use-metadata";
import {
  AlertDialog,
  Button,
  Description,
  Input,
  Label,
  Modal,
  Table,
  TextField,
  toast,
} from "@heroui/react";
import { Coins } from "lucide-react";
import { PageHeader, EmptyState, ErrorState, StatusChip, TableSkeleton } from "../shared";
import { PaginationBar } from "../pagination-bar";
import { useClientPage } from "../../lib/use-client-page";

function CreateCurrencyModal() {
  const [open, setOpen] = useState(false);
  // exponent kept as string while typing so the field can be empty; "0" is a
  // legal value (JPY) and must stay distinguishable from "not filled in".
  const [form, setForm] = useState({ code: "", name: "", exponent: "" });
  const mutation = useCreateCurrency();

  const exponentInvalid =
    form.exponent.trim() === "" ||
    Number.isNaN(Number(form.exponent)) ||
    Number(form.exponent) < 0 ||
    Number(form.exponent) > 18;

  function handleSubmit() {
    mutation.mutate(
      { code: form.code, name: form.name, exponent: Number(form.exponent) },
      {
        onSuccess: () => {
          toast.success("Currency created");
          setOpen(false);
          setForm({ code: "", name: "", exponent: "" });
        },
        onError: () => toast.danger("Failed to create currency"),
      },
    );
  }

  return (
    <>
      <Button size="sm" onPress={() => setOpen(true)}>Create</Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-md">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Create Currency</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <TextField fullWidth name="code" value={form.code} onChange={(v) => setForm({ ...form, code: v })}>
                <Label>Code</Label>
                <Input placeholder="USDT" />
              </TextField>
              <TextField fullWidth name="name" value={form.name} onChange={(v) => setForm({ ...form, name: v })}>
                <Label>Name</Label>
                <Input placeholder="Tether USD" />
              </TextField>
              <TextField
                fullWidth
                name="exponent"
                type="number"
                value={form.exponent}
                onChange={(v) => setForm({ ...form, exponent: v })}
              >
                <Label>Decimal places (0-18)</Label>
                <Input min={0} max={18} step={1} placeholder="e.g. 2 for USD, 0 for JPY, 18 for wei" />
                <Description>Number of decimal places this currency tracks.</Description>
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button variant="secondary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                onPress={handleSubmit}
                isPending={mutation.isPending}
                isDisabled={!form.code || !form.name || exponentInvalid}
              >
                Create
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function DeactivateCurrencyDialog({ id, name }: { id: string; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateCurrency();

  return (
    <>
      <Button size="sm" variant="tertiary" onPress={() => setOpen(true)}>Deactivate</Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="warning" />
              <AlertDialog.Heading>Deactivate &quot;{name}&quot;?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This currency will be marked inactive. Existing entries referencing it will be unaffected.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button variant="tertiary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() => mutation.mutate(id, {
                  onSuccess: () => {
                    toast.success("Currency deactivated");
                    setOpen(false);
                  },
                  onError: () => toast.danger("Failed to deactivate currency"),
                })}
              >
                Deactivate
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

export function CurrenciesPage() {
  const { data, isLoading, isError } = useCurrencies();
  const currencies = Array.isArray(data) ? data : [];
  const { pageItems, page, pageCount, setPage } = useClientPage(currencies);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="Currencies" description="Supported currency definitions" actions={<CreateCurrencyModal />} />

      {isLoading ? (
        <TableSkeleton rows={3} />
      ) : isError ? (
        <ErrorState message="Failed to load currencies" />
      ) : currencies.length === 0 ? (
        <EmptyState
          icon={<Coins className="size-8 text-muted" aria-hidden />}
          title="No currencies yet"
          description="Create your first currency to get started."
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Currencies" className="min-w-[560px]">
              <Table.Header>
                <Table.Column isRowHeader>ID</Table.Column>
                <Table.Column>Code</Table.Column>
                <Table.Column>Name</Table.Column>
                <Table.Column>Active</Table.Column>
                <Table.Column className="text-end">Actions</Table.Column>
              </Table.Header>
              <Table.Body>
                {pageItems.map((c) => (
                  <Table.Row key={c.uid} id={c.uid}>
                    <Table.Cell>
                      <span className="block max-w-[160px] truncate font-mono text-xs" title={c.uid}>{c.uid}</span>
                    </Table.Cell>
                    <Table.Cell className="font-mono">{c.code}</Table.Cell>
                    <Table.Cell>{c.name}</Table.Cell>
                    <Table.Cell><StatusChip status={c.is_active ? "active" : "inactive"} /></Table.Cell>
                    <Table.Cell className="text-end">
                      {c.is_active && <DeactivateCurrencyDialog id={c.uid} name={c.name} />}
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          <Table.Footer>
            <PaginationBar page={page} pageCount={pageCount} onPageChange={setPage} />
          </Table.Footer>
        </Table>
      )}
    </div>
  );
}
