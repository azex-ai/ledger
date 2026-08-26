// Type-level conformance between the hand-written client types (`types.ts`)
// and the generated OpenAPI schema (`schema.ts`, machine-generated from
// docs/openapi.yaml and pinned by the `codegen:check` CI gate).
//
// M5 (2026-08-26 web audit): `codegen:check` proves `schema.ts` matches
// `docs/openapi.yaml` — it proves nothing about whether the *hand-written*
// types the client/hooks/components actually compile against (`types.ts`)
// still agree with that generated schema. `types.ts`'s own header says "keep
// them aligned... when the backend contract changes", enforced by nothing.
// This file turns that prose promise into a compile error: `tsc --noEmit`
// (part of `npm run typecheck`) fails the moment either side drifts further.
//
// This file is intentionally NOT a `*.test.ts`/`*.test-d.ts` — it has no
// runtime assertions and is not meant to execute under `vitest run`. It is
// picked up by `tsc --noEmit` via tsconfig's `test/**/*.ts` include and
// exists purely to be type-checked.
//
// Two kinds of line below:
//   - A bare assignment with no `@ts-expect-error`: the hand type and the
//     generated schema currently agree on required/optional shape for that
//     entity. If they ever disagree, this file fails to compile.
//   - An assignment prefixed `@ts-expect-error`: they currently DISAGREE
//     (docs/openapi.yaml has no `required:` list for that schema, so
//     openapi-typescript marks every field optional, while `types.ts`
//     declares the fields the Go handler actually always populates as
//     required — verified against server/handler_*.go, which has no
//     `omitempty` tag on these fields). The `@ts-expect-error` both pins
//     that the disagreement exists today AND fails to compile ("Unused
//     '@ts-expect-error' directive") the moment someone silently resolves
//     it without removing this comment — so the gap can't regress quietly
//     in either direction. Escalated to team-lead (M5 boundary: the correct
//     fix is `docs/openapi.yaml` gaining `required:` lists, which sits
//     outside this worktree's `web/`-only file boundary); this file removes
//     the corresponding `@ts-expect-error` once that lands.

import type { components } from "../../src/client/schema";
import type {
  Journal,
  Entry,
  Balance,
  Reservation,
  Booking,
  DepositAddress,
  Event,
  ReconcileResult,
  SystemBalance,
  BalanceBreakdown,
  Lifecycle,
} from "../../src/client/types";

// --- Agree today (regresses loudly if either side drifts) -----------------

const _journal: Journal = {} as components["schemas"]["Journal"];
const _entry: Entry = {} as components["schemas"]["Entry"];
void _journal;
void _entry;

// --- Disagree today (docs/openapi.yaml missing `required:` lists) ---------

// @ts-expect-error — Balance: openapi.yaml has no `required:` list; every
// field is optional in schema.ts even though balanceResponse (Go) has no
// omitempty tag and always populates all four.
const _balance: Balance = {} as components["schemas"]["Balance"];
void _balance;

// @ts-expect-error — Reservation: same gap. Note `settled_amount` and
// `journal_uid` are genuinely optional server-side (reservationResponse.go
// uses `*string`/`omitempty`) — only those two SHOULD be `?` in types.ts;
// the rest are always present and should be `required:` in the spec.
const _reservation: Reservation = {} as components["schemas"]["Reservation"];
void _reservation;

// @ts-expect-error — Booking: same gap. `reservation_uid`/`journal_uid` are
// genuinely optional (bookingResponse.go: `omitempty`); the rest are always
// present.
const _booking: Booking = {} as components["schemas"]["Booking"];
void _booking;

// @ts-expect-error — DepositAddress: same gap; depositAddressResponse (Go)
// has no omitempty, all four fields always present.
const _depositAddress: DepositAddress = {} as components["schemas"]["DepositAddress"];
void _depositAddress;

// @ts-expect-error — Event: same gap. `journal_uid` is genuinely optional
// (eventResponse.go: `omitempty`); the rest are always present. `actor_id`/
// `source` exist in the spec but are not part of types.ts's Event at all —
// a separate, narrower drift not covered by this assertion.
const _event: Event = {} as components["schemas"]["Event"];
void _event;

// @ts-expect-error — ReconcileResult: same gap. `handleReconcileGlobal` /
// reconcileResponse (Go) has no omitempty anywhere in this struct or its
// nested detail rows — `details` is always a real (possibly empty) array,
// never absent — so this one's fix direction is unambiguous: the spec
// should mark the whole shape `required:`, not `types.ts` gain `?`. This is
// also the concrete crash the heroui ReconciliationPage guard-drift fix
// (M5) had to defend against until the spec catches up.
const _reconcile: ReconcileResult = {} as components["schemas"]["ReconcileResult"];
void _reconcile;

// @ts-expect-error — SystemBalance / SystemRollup: same gap, plus the two
// sides use different names for the same shape (systemBalanceResp in Go,
// `SystemRollup` in the spec, `SystemBalance` in types.ts) — a naming drift
// on top of the optionality drift, not asserted here beyond structural
// compatibility.
const _systemBalance: SystemBalance = {} as components["schemas"]["SystemRollup"];
void _systemBalance;

// @ts-expect-error — BalanceBreakdown: same gap.
const _balanceBreakdown: BalanceBreakdown = {} as components["schemas"]["BalanceBreakdown"];
void _balanceBreakdown;

// @ts-expect-error — Lifecycle: `openapi.yaml` marks `terminal` optional
// (a classification's lifecycle can legitimately have zero terminal
// states), but types.ts declares it required as `string[]`. Real callers
// already default it to `[]` when absent (see presets/), so the correct
// fix is `terminal?: string[]` in types.ts — tracked alongside the rest of
// this file's gaps rather than fixed here to keep this PR's diff to the
// five assigned findings (M1/M2/M3/M5/M7).
const _lifecycle: Lifecycle = {} as components["schemas"]["Lifecycle"];
void _lifecycle;

// --- Entities with NO generated schema counterpart at all ------------------
//
// docs/openapi.yaml has no schema for these response shapes at all — not a
// `required:` gap but a missing schema entirely, so there is nothing here to
// assert against. Discovered while writing this file; out of M5's assigned
// scope (fixing it means authoring new OpenAPI schemas, a spec-authoring
// task, not a type-conformance test) — flagged to team-lead separately:
//   Classification, JournalType, EntryTemplate, Currency, HealthStatus,
//   Snapshot, PreviewResult, HolderBalances, BalanceByCurrency,
//   JournalWithEntries (standalone; only nested inside
//   JournalWithEntriesEnvelope.data in the spec).
