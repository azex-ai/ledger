// Compile-only contract checks, included by `npm run typecheck`.
import type { LedgerClient } from "../../src/client/client";
import type { components } from "../../src/client/schema";

type ReserveInput = components["schemas"]["ReserveInput"];
type ClientReserveInput = Parameters<LedgerClient["createReservation"]>[0];
type Equal<A, B> =
  (<T>() => T extends A ? 1 : 2) extends
  (<T>() => T extends B ? 1 : 2) ? true : false;
type Assert<T extends true> = T;

// Exact equality catches missing optional fields that assignment checks miss.
type _ReserveInputMatchesSchema = Assert<Equal<ClientReserveInput, ReserveInput>>;

declare const client: LedgerClient;
const required = {
  account_holder: 1,
  currency_uid: "currency-uid",
  amount: "5",
  idempotency_key: "reserve-key",
};

void client.createReservation(required);
void client.createReservation({
  ...required,
  expires_in_sec: 3600,
  require_verified_balance: true,
});

void client.createReservation({
  ...required,
  // @ts-expect-error The wire takes numeric expires_in_sec, never expires_in.
  expires_in: "1h",
});

void client.createReservation({
  ...required,
  // @ts-expect-error Durations on the wire are seconds as numbers.
  expires_in_sec: "3600",
});

void client.createReservation({
  ...required,
  // @ts-expect-error Verification is a boolean, not a truthy string.
  require_verified_balance: "true",
});
