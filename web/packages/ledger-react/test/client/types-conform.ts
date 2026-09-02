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
// bus #48 (2026-08-27): closed the gap that produced the `@ts-expect-error`
// lines this file used to carry. `docs/openapi.yaml`'s Balance/Reservation/
// Booking/DepositAddress/Event/ReconcileResult/SystemRollup/BalanceBreakdown
// schemas had no `required:` list, so openapi-typescript marked every field
// optional in `schema.ts` while `types.ts` declared them required — every
// assignment below failed to compile without the suppression. Fix, in two
// halves per schema (which half depends on what the Go handler actually
// does — checked against server/handler_*.go, not guessed):
//   - Fields the handler always populates (no `omitempty`, no pointer) ->
//     `docs/openapi.yaml` gained `required:` entries for them.
//   - Fields that are genuinely optional server-side (`omitempty`/`*string`
//     — absent from the wire, not present-as-empty, until something links
//     them: `Reservation.settled_amount`/`journal_uid`,
//     `Booking.reservation_uid`/`journal_uid`, `Event.journal_uid`,
//     `Lifecycle.terminal`) -> `types.ts` gained `?` instead of being forced
//     required in the spec.
// Every line below is now a bare assignment: the hand type and the
// generated schema agree today. If they ever disagree again, this file
// fails to compile — that is the point.
//
// J-7 (2026-09-02 web audit): six more schemas existed in `docs/openapi.yaml`
// by this pass but were never pinned here — the file's own trailing "no
// generated schema counterpart" note (below) had gone stale for
// Classification/JournalType/Template/Currency/HealthStatus/
// TemplatePreviewResult, all six of which the spec had grown since M5.
// Added below. Two of the six caught REAL drift while being added (fixed at
// the SOURCE, not by loosening the assertion — see the referenced fixes):
//   - `HealthStatus`/`Classification`/`JournalType` schemas each require a
//     field (`db`/`balance_role`/`holder_kind`) that `types.ts`'s hand
//     type doesn't declare at all. This is the exact one-directional blind
//     spot noted below (assigning a WIDER source to a narrower target
//     compiles fine) — not fixed here (each is a real but separate gap;
//     nothing in this package currently reads any of the three, so there is
//     no live bug to point to the way there was for PreviewResult below).
//   - `TemplatePreviewResult` has ONLY `entries` — no total_debit/
//     total_credit. `types.ts`'s `PreviewResult` used to declare both as
//     required, which this assertion caught immediately (assigning a
//     NARROWER source, missing fields, to that wider required type is a
//     compile error) — traced to a real bug (TemplatesPage's preview
//     rendered "Total Debit: undefined | Total Credit: undefined" on every
//     currently-shipped build) and fixed by removing the phantom fields
//     from `PreviewResult` and summing `entries` client-side instead. THIS
//     is the direction this gate is actually good at: a hand type that
//     over-promises a field the wire never carries.
// The four still-missing-schema entities from the note below are unchanged.

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
  Classification,
  JournalType,
  EntryTemplate,
  Currency,
  HealthStatus,
  PreviewResult,
} from "../../src/client/types";

const _journal: Journal = {} as components["schemas"]["Journal"];
const _entry: Entry = {} as components["schemas"]["Entry"];
const _balance: Balance = {} as components["schemas"]["Balance"];
const _reservation: Reservation = {} as components["schemas"]["Reservation"];
const _booking: Booking = {} as components["schemas"]["Booking"];
const _depositAddress: DepositAddress = {} as components["schemas"]["DepositAddress"];
const _event: Event = {} as components["schemas"]["Event"];
const _reconcile: ReconcileResult = {} as components["schemas"]["ReconcileResult"];

// SystemBalance / SystemRollup: same shape, different names across the two
// sides (systemRollupResponse in Go, `SystemRollup` in the spec,
// `SystemBalance` in types.ts) — a naming drift, not an optionality one;
// not asserted here beyond structural compatibility.
const _systemBalance: SystemBalance = {} as components["schemas"]["SystemRollup"];

const _balanceBreakdown: BalanceBreakdown = {} as components["schemas"]["BalanceBreakdown"];
const _lifecycle: Lifecycle = {} as components["schemas"]["Lifecycle"];

// J-7 additions (2026-09-02 web audit) — see the file-header note above.
const _classification: Classification = {} as components["schemas"]["Classification"];
const _journalType: JournalType = {} as components["schemas"]["JournalType"];
const _entryTemplate: EntryTemplate = {} as components["schemas"]["Template"];
const _currency: Currency = {} as components["schemas"]["Currency"];
const _healthStatus: HealthStatus = {} as components["schemas"]["HealthStatus"];
const _previewResult: PreviewResult = {} as components["schemas"]["TemplatePreviewResult"];

void _journal;
void _entry;
void _balance;
void _reservation;
void _booking;
void _depositAddress;
void _event;
void _reconcile;
void _systemBalance;
void _balanceBreakdown;
void _lifecycle;
void _classification;
void _journalType;
void _entryTemplate;
void _currency;
void _healthStatus;
void _previewResult;

// --- Entities with NO generated schema counterpart at all ------------------
//
// docs/openapi.yaml has no schema for these response shapes at all — not a
// `required:` gap but a missing schema entirely, so there is nothing here to
// assert against. Out of scope for a type-conformance test (fixing it means
// authoring new OpenAPI schemas, a spec-authoring task) — flagged to
// team-lead separately. Re-verified current as of J-7 (2026-09-02 web
// audit); six other entities that were on this list at M5 time
// (Classification, JournalType, EntryTemplate/Template, Currency,
// HealthStatus, PreviewResult/TemplatePreviewResult) have since grown a spec
// schema and are asserted above instead — this list is exactly the
// remainder still missing one:
//   Snapshot, HolderBalances, BalanceByCurrency,
//   JournalWithEntries (standalone; only nested inside
//   JournalWithEntriesEnvelope.data in the spec).
