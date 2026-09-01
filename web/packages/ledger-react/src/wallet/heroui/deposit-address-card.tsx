"use client";

import { Button, Card, Skeleton } from "@heroui/react";
import { Check, Copy, Wallet, X } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { toast } from "sonner";
import { ErrorState } from "../../heroui/shared";
import { shortenAddress, useCopyToClipboard } from "../../lib/utils";
import { ApiRequestError } from "../../client/client";
import { useWalletDepositAddress, useEnsureWalletDepositAddress } from "../use-deposit-address";

/*
 * Deposit address card (HeroUI skin). Page logic mirrors the shadcn skin
 * (src/wallet/components/deposit-address-card.tsx) — keep in sync. Identity
 * comes exclusively from the holder token via WalletClient — no holder prop.
 */

function DepositAddressCardSkeleton() {
  return (
    <Card>
      <Card.Header>
        <Skeleton className="h-4 w-40 rounded" />
      </Card.Header>
      <Card.Content className="flex flex-col items-center gap-3">
        <Skeleton className="h-[200px] w-[200px] rounded" />
        <Skeleton className="h-4 w-full rounded" />
      </Card.Content>
    </Card>
  );
}

function CopyAddressButton({ address }: { address: string }) {
  const [state, copy] = useCopyToClipboard();
  return (
    <Button
      isIconOnly
      size="sm"
      variant="ghost"
      aria-label="Copy address"
      onPress={() => {
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
        <Check className="text-success" aria-hidden />
      ) : state === "failed" ? (
        <X className="text-danger" aria-hidden />
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
        <Card.Header>
          <Card.Title className="text-muted text-sm font-medium">
            Deposit address
          </Card.Title>
        </Card.Header>
        <Card.Content className="flex flex-col items-center gap-3 py-4 text-center">
          <Wallet className="text-muted size-8" aria-hidden />
          <p className="text-muted text-sm">Generate an address to deposit funds.</p>
          <Button
            isPending={ensure.isPending}
            onPress={() =>
              ensure.mutate(undefined, {
                onError: () =>
                  toast.error("Couldn't generate a deposit address. Please try again."),
              })
            }
          >
            Generate deposit address
          </Button>
        </Card.Content>
      </Card>
    );
  }

  return (
    <Card>
      <Card.Header>
        <Card.Title className="text-muted text-sm font-medium">
          Your deposit address
        </Card.Title>
      </Card.Header>
      <Card.Content className="flex flex-col items-center gap-3">
        <div className="rounded-lg bg-white p-3">
          <QRCodeSVG value={data.address} size={200} />
        </div>
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-sm tabular-nums" title={data.address}>
            {shortenAddress(data.address)}
          </span>
          <CopyAddressButton address={data.address} />
        </div>
        <p className="text-muted text-center text-xs">
          Only send {assets.join(" or ")} on <strong>{network}</strong> to this
          address. Funds sent on any other network cannot be recovered.
        </p>
      </Card.Content>
    </Card>
  );
}
