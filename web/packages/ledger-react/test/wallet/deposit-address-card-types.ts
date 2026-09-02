/*
 * Compile-time pin (J-22, 2026-09-02 web audit) — not a vitest suite (no
 * `.test.` in the filename, so vitest never tries to run it as one, same
 * convention as ../client/types-conform.ts), checked by `tsc --noEmit`
 * (part of `npm run typecheck`, which CI runs on every push).
 *
 * `DepositAddressCardProps.assets` must be a non-empty tuple
 * (`[string, ...string[]]`), not `string[]` — passing `[]` produced "Only
 * send  on Ethereum" (empty asset list) with no warning at all, the one
 * thing standing between a user and unrecoverable loss. This file's only
 * job is to make sure `[]` is a compile error on BOTH skins; if either ever
 * regresses back to a bare `string[]`, this file itself fails to compile
 * (the `@ts-expect-error` would report "this expression is not expected to
 * error").
 */

import type { DepositAddressCardProps as ShadcnProps } from "../../src/wallet/components/deposit-address-card";
import type { DepositAddressCardProps as HeroUiProps } from "../../src/wallet/heroui/deposit-address-card";

// @ts-expect-error — assets must be non-empty; `[]` is a compile error (shadcn skin).
const _shadcnEmptyAssets: ShadcnProps = { network: "Ethereum", assets: [] };
void _shadcnEmptyAssets;

// @ts-expect-error — assets must be non-empty; `[]` is a compile error (HeroUI skin).
const _herouiEmptyAssets: HeroUiProps = { network: "Ethereum", assets: [] };
void _herouiEmptyAssets;

// A non-empty array IS assignable — this line must compile clean, so a
// future change that makes the type too strict (e.g. requiring a specific
// literal) would also be caught here.
const _shadcnOk: ShadcnProps = { network: "Ethereum", assets: ["USDC"] };
void _shadcnOk;
const _herouiOk: HeroUiProps = { network: "Ethereum", assets: ["USDC"] };
void _herouiOk;
