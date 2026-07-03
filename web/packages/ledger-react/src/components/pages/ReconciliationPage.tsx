"use client";

import { useState } from "react";
import { formatAmount, formatSignedAmount, formatUTC, cn } from "../../lib/utils";
import { useReconcileGlobal, useReconcileAccount } from "../../hooks/use-system";
import { PageHeader } from "../page-header";
import { StatusBadge } from "../status-badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import { AlertCircle } from "lucide-react";

export function ReconciliationPage() {
  const globalMutation = useReconcileGlobal();
  const accountMutation = useReconcileAccount();
  const [holder, setHolder] = useState("");
  const [currencyId, setCurrencyId] = useState("");

  const globalResult = globalMutation.data;
  const accountResult = accountMutation.data;

  return (
    <div className="space-y-6">
      <PageHeader title="Reconciliation" description="Verify ledger integrity" />

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Global Check</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <p className="text-xs text-muted-foreground">
              Verifies SUM(all debits) == SUM(all credits) across the entire ledger.
            </p>
            <Button onClick={() => globalMutation.mutate()} disabled={globalMutation.isPending}>
              {globalMutation.isPending ? "Running..." : "Run Global Check"}
            </Button>
            {globalMutation.isError && (
              <div className="flex items-center gap-2 text-sm text-destructive">
                <AlertCircle className="h-4 w-4" />
                Reconciliation failed. Check the API logs.
              </div>
            )}
            {globalResult && (
              <div className="space-y-2 pt-2">
                <div className="flex items-center gap-2">
                  <StatusBadge status={globalResult.balanced ? "confirmed" : "failed"} />
                  <span className="text-sm">
                    {globalResult.balanced ? "Balanced" : `Unbalanced (gap: ${globalResult.gap})`}
                  </span>
                </div>
                <p className="text-xs text-muted-foreground">
                  Checked at {formatUTC(globalResult.checked_at)}
                </p>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Account Check</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <p className="text-xs text-muted-foreground">
              Verifies checkpoint balances match entry sums for a specific account.
            </p>
            <div className="flex gap-2">
              <div className="grid gap-1">
                <Label htmlFor="recon-holder" className="text-xs">Holder</Label>
                <Input id="recon-holder" value={holder} onChange={(e) => setHolder(e.target.value)} placeholder="1001" className="w-28" />
              </div>
              <div className="grid gap-1">
                <Label htmlFor="recon-currency" className="text-xs">Currency</Label>
                <Input id="recon-currency" value={currencyId} onChange={(e) => setCurrencyId(e.target.value)} placeholder="1" className="w-28" />
              </div>
              <div className="flex items-end">
                <Button
                  onClick={() => {
                    const h = parseInt(holder, 10);
                    const c = currencyId.trim();
                    if (isNaN(h) || c === "") return;
                    accountMutation.mutate({ holder: h, currencyUid: c });
                  }}
                  disabled={accountMutation.isPending || !holder || !currencyId}
                >
                  {accountMutation.isPending ? "Running..." : "Check"}
                </Button>
              </div>
            </div>
            {accountMutation.isError && (
              <div className="flex items-center gap-2 text-sm text-destructive">
                <AlertCircle className="h-4 w-4" />
                Account check failed.
              </div>
            )}
            {accountResult && (
              <div className="space-y-2 pt-2">
                <div className="flex items-center gap-2">
                  <StatusBadge status={accountResult.balanced ? "confirmed" : "failed"} />
                  <span className="text-sm">
                    {accountResult.balanced ? "Balanced" : `Drift detected (gap: ${accountResult.gap})`}
                  </span>
                </div>
                {accountResult.details && accountResult.details.length > 0 && (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Holder</TableHead>
                        <TableHead>Currency</TableHead>
                        <TableHead>Classification</TableHead>
                        <TableHead className="text-right">Expected</TableHead>
                        <TableHead className="text-right">Actual</TableHead>
                        <TableHead className="text-right">Drift</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {accountResult.details.map((d) => (
                        <TableRow key={`${d.account_holder}-${d.currency_uid}-${d.classification_uid}`}>
                          <TableCell>{d.account_holder}</TableCell>
                          <TableCell>{d.currency_uid}</TableCell>
                          <TableCell>{d.classification_uid}</TableCell>
                          <TableCell className="text-right font-mono">{formatAmount(d.expected)}</TableCell>
                          <TableCell className="text-right font-mono">{formatAmount(d.actual)}</TableCell>
                          <TableCell className="text-right font-mono">
                            {(() => {
                              const drift = formatSignedAmount(d.drift);
                              return (
                                <span className={cn(drift.isPositive && "text-emerald-400", drift.isNegative && "text-red-400")}>
                                  {drift.isPositive ? "+" : ""}{drift.text}
                                </span>
                              );
                            })()}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
