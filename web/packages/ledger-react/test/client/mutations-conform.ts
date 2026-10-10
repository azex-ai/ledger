// Compile real public calls against generated operation bodies. This file is
// checked by tsc, not executed by Vitest; no hand-written DTO mock is involved.
import type { components, paths } from "../../src/client/schema";
import type { LedgerClient } from "../../src/client/client";

declare const client: LedgerClient;
declare const journal: components["schemas"]["JournalInput"];
declare const templateJournal: components["schemas"]["TemplateExecutionRequest"];
declare const classification: components["schemas"]["ClassificationInput"];
declare const booking: components["schemas"]["CreateBookingInput"];
declare const transition: components["schemas"]["TransitionInput"];
declare const template: components["schemas"]["TemplateInput"];
declare const preview: components["schemas"]["TemplatePreviewRequest"];
declare const journalType: paths["/journal-types"]["post"]["requestBody"]["content"]["application/json"];
declare const currency: paths["/currencies"]["post"]["requestBody"]["content"]["application/json"];
declare const settle: paths["/reservations/{uid}/settle"]["post"]["requestBody"]["content"]["application/json"];
declare const partial: paths["/reservations/{uid}/settle-partial"]["post"]["requestBody"]["content"]["application/json"];

void client.postJournal(journal);
void client.postTemplateJournal(templateJournal);
void client.createClassification({ ...classification, is_system: true, balance_role: "" });
void client.createBooking(booking);
const { idempotency_key, ...transitionBody } = transition;
void client.transitionBooking("booking", transitionBody, idempotency_key);
void client.createJournalType(journalType);
void client.createTemplate(template);
void client.createCurrency(currency);
void client.previewTemplate("template", preview);
void client.previewTemplate("template", { holder_id: 1, currency_uid: "usd", amount: "1.00" });
void client.settleReservation("reservation", settle.actual_amount, settle.idempotency_key);
void client.settlePartialReservation("reservation", partial.amount, partial.idempotency_key);

// New optional fields must be accepted on fresh public-call literals, where
// excess-property checks actually run (a wide-to-narrow alias assignment would
// silently miss the fields that this change is meant to expose).
void client.postJournal({ ...journal, event_uid: "event", actor_id: 42, effective_at: "2026-10-10T00:00:00Z" });
void client.postTemplateJournal({ ...templateJournal, event_uid: "event", actor_id: 42, metadata: { memo: "settled" } });
void client.createClassification({ code: "available", name: "Available", normal_side: "credit", is_system: false, balance_role: "available", lifecycle: null });
void client.createJournalType({ code: "deposit", name: "Deposit", holder_kind: "deposit" });
void client.createBooking({ classification_code: "deposit", account_holder: 42, currency_uid: "usd", amount: "1", idempotency_key: "key" });
void client.transitionBooking("booking", { to_status: "confirmed", source: "operator", metadata: { note: "checked" } }, "key");

// @ts-expect-error journal entries remain required
void client.postJournal({ journal_type_uid: "type", idempotency_key: "key" });
// @ts-expect-error effective_at is an RFC3339 string, not a number
void client.postJournal({ ...journal, effective_at: 42 });
// @ts-expect-error monetary values are decimal strings
void client.postJournal({ ...journal, entries: [{ account_holder: 1, currency_uid: "usd", classification_uid: "available", entry_type: "debit", amount: 1 }] });
// @ts-expect-error template amounts are required
void client.postTemplateJournal({ template_code: "deposit", holder_id: 42, currency_uid: "usd", idempotency_key: "key" });
// @ts-expect-error journal metadata values are strings
void client.postTemplateJournal({ ...templateJournal, metadata: { nested: { note: "bad" } } });
// @ts-expect-error booking metadata is string tags, never nested JSON
void client.createBooking({ ...booking, metadata: { nested: { note: "bad" } } });
// @ts-expect-error transition metadata is string tags too
void client.transitionBooking("booking", { to_status: "confirmed", metadata: { n: 42 } }, "key");
// @ts-expect-error transition must retain its required scalar idempotency key
void client.transitionBooking("booking", { to_status: "confirmed" });
// @ts-expect-error do not supply a conflicting body key alongside the scalar header key
void client.transitionBooking("booking", { to_status: "confirmed", idempotency_key: "body-key" }, "header-key");
// @ts-expect-error non-system classification must explicitly declare a role
void client.createClassification({ code: "available", name: "Available", normal_side: "credit", is_system: false });
// @ts-expect-error holder_kind is the generated enum
void client.createJournalType({ code: "invalid", name: "Invalid", holder_kind: "invented" });
// @ts-expect-error currency exponent remains required, including for zero-decimal currencies
void client.createCurrency({ code: "USD", name: "Dollar" });
// @ts-expect-error template lines remain required
void client.createTemplate({ code: "template", name: "Template", journal_type_uid: "type" });
// @ts-expect-error preview requires either canonical amounts or the single-amount adapter
void client.previewTemplate("template", { holder_id: 42, currency_uid: "usd" });
// @ts-expect-error preview amount-map values are decimal strings
void client.previewTemplate("template", { holder_id: 42, currency_uid: "usd", amounts: { amount: 1 } });
// @ts-expect-error single-amount adapter does not accept lossy numbers
void client.previewTemplate("template", { holder_id: 42, currency_uid: "usd", amount: 1 });
// @ts-expect-error preview does not accept posting metadata
void client.previewTemplate("template", { ...preview, actor_id: 42 });
// @ts-expect-error settlement amount is a decimal string
void client.settleReservation("reservation", 1, "key");
// @ts-expect-error a partial settlement key is required
void client.settlePartialReservation("reservation", "1");
// @ts-expect-error a finalization key is required even without a JSON body
void client.finalizeReservationSettlement("reservation");
// @ts-expect-error release key is required even without a JSON body
void client.releaseReservation("reservation");
// @ts-expect-error reversal reason is a string
void client.reverseJournal("journal", 1);
// @ts-expect-error reconciliation holder is numeric on this wire
void client.reconcileAccount("42", "usd");
