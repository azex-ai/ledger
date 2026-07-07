"use client";

import { useState } from "react";
import { Button, Card, Input, Label, Table, TextField, toast } from "@heroui/react";
import { useReconcileAccount, useReconcileGlobal } from "../../hooks/use-system";
import { cn, formatAmount, formatSignedAmount, formatUTC } from "../../lib/utils";
import { PageHeader, StatusChip } from "../shared";

export function ReconciliationPage() {
  const globalMutation = useReconcileGlobal();
  const accountMutation = useReconcileAccount();
  const [holder, setHolder] = useState("");
  const [currencyId, setCurrencyId] = useState("");

  const globalResult = globalMutation.data;
  const accountResult = accountMutation.data;

  function runGlobalCheck() {
    toast.promise(globalMutation.mutateAsync(), {
      loading: "Running global check…",
      success: (result) =>
        result.balanced ? "Ledger is balanced" : `Unbalanced — gap: ${result.gap}`,
      error: "Reconciliation failed. Check the API logs.",
    });
  }

  function runAccountCheck() {
    const h = parseInt(holder, 10);
    const c = currencyId.trim();
    if (isNaN(h) || c === "") {
      toast.danger("Enter both a holder and a currency to run the check.");
      return;
    }
    toast.promise(accountMutation.mutateAsync({ holder: h, currencyUid: c }), {
      loading: "Checking account…",
      success: (result) =>
        result.balanced ? "Account is balanced" : `Drift detected — gap: ${result.gap}`,
      error: "Account check failed.",
    });
  }

  return (
    <div className="space-y-6">
      <PageHeader title="Reconciliation" description="Verify ledger integrity" />

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <Card>
          <Card.Header>
            <Card.Title className="text-sm font-medium">Global Check</Card.Title>
          </Card.Header>
          <Card.Content className="gap-4">
            <p className="text-xs text-muted">
              Verifies SUM(all debits) == SUM(all credits) across the entire ledger.
            </p>
            <Button isPending={globalMutation.isPending} onPress={runGlobalCheck}>
              {({ isPending }) => (isPending ? "Running…" : "Run Global Check")}
            </Button>
            {globalResult ? (
              <div className="space-y-2 pt-2">
                <div className="flex items-center gap-2">
                  <StatusChip status={globalResult.balanced ? "confirmed" : "failed"} />
                  <span className="text-sm">
                    {globalResult.balanced ? "Balanced" : `Unbalanced (gap: ${globalResult.gap})`}
                  </span>
                </div>
                <p className="text-xs text-muted">
                  Checked at {formatUTC(globalResult.checked_at)}
                </p>
              </div>
            ) : null}
          </Card.Content>
        </Card>

        <Card>
          <Card.Header>
            <Card.Title className="text-sm font-medium">Account Check</Card.Title>
          </Card.Header>
          <Card.Content className="gap-4">
            <p className="text-xs text-muted">
              Verifies checkpoint balances match entry sums for a specific account.
            </p>
            <div className="flex flex-wrap items-end gap-2">
              <TextField className="w-28" value={holder} onChange={setHolder}>
                <Label className="text-xs">Holder</Label>
                <Input placeholder="1001" />
              </TextField>
              <TextField className="w-28" value={currencyId} onChange={setCurrencyId}>
                <Label className="text-xs">Currency</Label>
                <Input placeholder="1" />
              </TextField>
              <Button isPending={accountMutation.isPending} onPress={runAccountCheck}>
                {({ isPending }) => (isPending ? "Running…" : "Check")}
              </Button>
            </div>
            {accountResult ? (
              <div className="space-y-2 pt-2">
                <div className="flex items-center gap-2">
                  <StatusChip status={accountResult.balanced ? "confirmed" : "failed"} />
                  <span className="text-sm">
                    {accountResult.balanced
                      ? "Balanced"
                      : `Drift detected (gap: ${accountResult.gap})`}
                  </span>
                </div>
                {accountResult.details.length > 0 ? (
                  <Table>
                    <Table.ScrollContainer>
                      <Table.Content aria-label="Account drift details" className="min-w-[560px]">
                        <Table.Header>
                          <Table.Column isRowHeader>Holder</Table.Column>
                          <Table.Column>Currency</Table.Column>
                          <Table.Column>Classification</Table.Column>
                          <Table.Column className="text-end">Expected</Table.Column>
                          <Table.Column className="text-end">Actual</Table.Column>
                          <Table.Column className="text-end">Drift</Table.Column>
                        </Table.Header>
                        <Table.Body>
                          {accountResult.details.map((d) => {
                            const drift = formatSignedAmount(d.drift);
                            const rowId = `${d.account_holder}-${d.currency_uid}-${d.classification_uid}`;
                            return (
                              <Table.Row key={rowId} id={rowId}>
                                <Table.Cell>{d.account_holder}</Table.Cell>
                                <Table.Cell>{d.currency_uid}</Table.Cell>
                                <Table.Cell>{d.classification_uid}</Table.Cell>
                                <Table.Cell className="text-end font-mono">
                                  {formatAmount(d.expected)}
                                </Table.Cell>
                                <Table.Cell className="text-end font-mono">
                                  {formatAmount(d.actual)}
                                </Table.Cell>
                                <Table.Cell className="text-end font-mono">
                                  <span
                                    className={cn(
                                      drift.isPositive && "text-success",
                                      drift.isNegative && "text-danger",
                                    )}
                                  >
                                    {drift.isPositive ? "+" : ""}
                                    {drift.text}
                                  </span>
                                </Table.Cell>
                              </Table.Row>
                            );
                          })}
                        </Table.Body>
                      </Table.Content>
                    </Table.ScrollContainer>
                  </Table>
                ) : null}
              </div>
            ) : null}
          </Card.Content>
        </Card>
      </div>
    </div>
  );
}
