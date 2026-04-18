"use client";

import { useState } from "react";
import { useReconcileGlobal, useReconcileAccount } from "@/lib/hooks/use-system";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";

export default function ReconciliationPage() {
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
            {globalResult && (
              <div className="space-y-2 pt-2">
                <div className="flex items-center gap-2">
                  <StatusBadge status={globalResult.balanced ? "confirmed" : "failed"} />
                  <span className="text-sm">
                    {globalResult.balanced ? "Balanced" : `Unbalanced (gap: ${globalResult.gap})`}
                  </span>
                </div>
                <p className="text-xs text-muted-foreground">
                  Checked at {new Date(globalResult.checked_at).toLocaleString()}
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
                <Label className="text-xs">Holder</Label>
                <Input value={holder} onChange={(e) => setHolder(e.target.value)} placeholder="1001" className="w-28" />
              </div>
              <div className="grid gap-1">
                <Label className="text-xs">Currency</Label>
                <Input value={currencyId} onChange={(e) => setCurrencyId(e.target.value)} placeholder="1" className="w-28" />
              </div>
              <div className="flex items-end">
                <Button
                  onClick={() =>
                    accountMutation.mutate({
                      holder: parseInt(holder),
                      currencyId: parseInt(currencyId),
                    })
                  }
                  disabled={accountMutation.isPending || !holder || !currencyId}
                >
                  {accountMutation.isPending ? "Running..." : "Check"}
                </Button>
              </div>
            </div>
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
                      {accountResult.details.map((d, i) => (
                        <TableRow key={i}>
                          <TableCell>{d.account_holder}</TableCell>
                          <TableCell>{d.currency_id}</TableCell>
                          <TableCell>{d.classification_id}</TableCell>
                          <TableCell className="text-right font-mono">{d.expected}</TableCell>
                          <TableCell className="text-right font-mono">{d.actual}</TableCell>
                          <TableCell className="text-right font-mono text-red-400">{d.drift}</TableCell>
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
