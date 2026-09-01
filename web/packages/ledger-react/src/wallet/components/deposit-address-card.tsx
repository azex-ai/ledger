"use client";

import { Check, Copy, Wallet, X } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { toast } from "sonner";
import { Button } from "../../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../../components/ui/card";
import { ErrorState } from "../../components/error-state";
import { shortenAddress, useCopyToClipboard } from "../../lib/utils";
import { ApiRequestError } from "../../client/client";
import { useWalletDepositAddress, useEnsureWalletDepositAddress } from "../use-deposit-address";

/*
 * Deposit address card (shadcn skin). User language only: "your deposit
 * address" + QR code — never the CREATE2 factory/init_hash/chain_id this
 * address is derived from, never "sweep"/"webhook"/provider mechanics
 * (user-facing-surfaces.md). Mirrored in the HeroUI skin
 * (src/wallet/heroui/deposit-address-card.tsx) — keep page logic in sync.
 *
 * Identity comes exclusively from the holder token via WalletClient — this
 * component takes no holder prop, so there is no way to point it at another
 * holder's address.
 */

function DepositAddressCardSkeleton() {
  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="h-4 w-40 animate-shimmer rounded" />
      </CardHeader>
      <CardContent className="flex flex-col items-center gap-3">
        <div className="h-[200px] w-[200px] animate-shimmer rounded" />
        <div className="h-4 w-full animate-shimmer rounded" />
      </CardContent>
    </Card>
  );
}

function CopyAddressButton({ address }: { address: string }) {
  const [state, copy] = useCopyToClipboard();
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label="Copy address"
      onClick={() => {
        copy(address).then((ok) => {
          if (ok) {
            toast.success("Address copied");
          } else {
            toast.error("Couldn't copy — please copy the address manually.");
          }
        });
      }}
    >
      {state === "copied" ? (
        <Check className="text-primary" aria-hidden />
      ) : state === "failed" ? (
        <X className="text-destructive" aria-hidden />
      ) : (
        <Copy aria-hidden />
      )}
    </Button>
  );
}

export interface DepositAddressCardProps {
  /**
   * Network display name the address lives on, e.g. "Ethereum" / "Base".
   * REQUIRED — this is a CREATE2 address derived per chain, and the package
   * cannot know which chain the host deployed it on. Sending on the wrong
   * network is unrecoverable loss, so a missing network name must be a compile
   * error, never a silent omission (working-agreements.md §5).
   */
  network: string;
  /**
   * Asset codes accepted at this address, e.g. ["USDC"] or ["USDT", "USDC"].
   * REQUIRED — the deployment's currencies come from the host (its currency
   * config / an admin `useCurrencies()` lookup), never a hardcoded guess. The
   * package is a generic ledger surface and does not know which assets a given
   * deployment accepts.
   */
  assets: string[];
}

/**
 * Shows the token-bound holder's crypto deposit address with a QR code, or a
 * "Generate address" CTA on first use. Read-only about the address itself —
 * generating one is the only write in this otherwise read-only wallet surface.
 */
export function DepositAddressCard({ network, assets }: DepositAddressCardProps) {
  const { data, isLoading, isError, error, refetch } = useWalletDepositAddress();
  const ensure = useEnsureWalletDepositAddress();

  if (isLoading) return <DepositAddressCardSkeleton />;

  const notFound = isError && error instanceof ApiRequestError && error.status === 404;

  if (isError && !notFound) {
    return (
      <ErrorState message="Couldn't load your deposit address. Please try again." onRetry={refetch} />
    );
  }

  if (notFound || !data) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            Deposit address
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col items-center gap-3 py-4 text-center">
          <Wallet className="h-8 w-8 text-muted-foreground" aria-hidden />
          <p className="text-sm text-muted-foreground">
            Generate an address to deposit funds.
          </p>
          <Button
            onClick={() =>
              ensure.mutate(undefined, {
                onError: () =>
                  toast.error("Couldn't generate a deposit address. Please try again."),
              })
            }
            disabled={ensure.isPending}
          >
            {ensure.isPending ? "Generating..." : "Generate deposit address"}
          </Button>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">
          Your deposit address
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col items-center gap-3">
        <div className="rounded-lg bg-white p-3">
          <QRCodeSVG value={data.address} size={200} />
        </div>
        <div className="flex items-center gap-1.5">
          <span
            className="font-mono text-sm tabular-nums"
            title={data.address}
          >
            {shortenAddress(data.address)}
          </span>
          <CopyAddressButton address={data.address} />
        </div>
        <p className="text-center text-xs text-muted-foreground">
          Only send {assets.join(" or ")} on <strong>{network}</strong> to this
          address. Funds sent on any other network cannot be recovered.
        </p>
      </CardContent>
    </Card>
  );
}
