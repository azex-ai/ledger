import { createContext, useContext } from "react";
import type { WalletClient } from "./client";

// Context holding the configured WalletClient. Null when no provider is
// mounted — useWalletClient() turns that into a loud error rather than
// silently handing back null.
export const WalletClientContext = createContext<WalletClient | null>(null);

export function useWalletClient(): WalletClient {
  const client = useContext(WalletClientContext);
  if (client === null) {
    throw new Error("useWalletClient must be used within <WalletProvider>");
  }
  return client;
}
