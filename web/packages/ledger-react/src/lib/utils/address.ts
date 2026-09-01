/**
 * Ethereum address utilities — wraps viem for validation, checksumming,
 * and display formatting.
 *
 * The ledger's EVM channel adapter uses CREATE2 deposit addresses,
 * so these are directly relevant for the dashboard.
 */

import { getAddress as checksumAddress } from "viem";

export { isAddress, isAddressEqual } from "viem";

/** Re-export getAddress under its original name. */
export const getAddress = checksumAddress;

/**
 * Shorten an address for display: "0x8ba1...78AC".
 *
 *   shortenAddress("0x8ba1f109551bd432803012645ac136ddd64dba72")
 *   → "0x8ba1...DBA72"
 *
 *   shortenAddress("0x8ba1f109551bd432803012645ac136ddd64dba72", 6)
 *   → "0x8ba1f1...64DBA72"
 */
export function shortenAddress(address: string, chars = 4): string {
  let formatted: string;
  try {
    formatted = checksumAddress(address);
  } catch {
    formatted = address;
  }
  if (formatted.length <= chars * 2 + 5) return formatted;
  return `${formatted.slice(0, chars + 2)}...${formatted.slice(-chars)}`;
}

/**
 * Shorten a hash (tx hash, etc.) for display: "0x1234…abcd". Unlike
 * shortenAddress this does NOT checksum — a 66-char tx hash is not a 20-byte
 * address, so viem's getAddress would throw on it (and shortenAddress only
 * hid that behind a try/catch). Purely a string trim.
 *
 *   shortenHash("0xabc…def", 6) → "0xabcdef…89abcdef"
 */
export function shortenHash(hash: string, chars = 4): string {
  if (hash.length <= chars * 2 + 5) return hash;
  return `${hash.slice(0, chars + 2)}...${hash.slice(-chars)}`;
}
