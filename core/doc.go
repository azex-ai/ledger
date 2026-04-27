// Package core is the pure domain layer of the ledger engine. It defines
// the canonical types (Booking, Event, Journal, Entry, Reservation,
// Classification + Lifecycle, Balance) and the consumer-side interfaces
// (Booker, JournalWriter, BalanceReader, Reserver, EventReader,
// Reconciler, Snapshotter, ClassificationStore, JournalTypeStore,
// TemplateStore, CurrencyStore) that the postgres adapter implements.
//
// core has zero external dependencies beyond shopspring/decimal and the
// standard library. It does not import net/http, pgx, slog, chi, redis,
// or any project-internal infrastructure package -- this is enforced as
// an architectural invariant.
//
// All amounts are shopspring/decimal.Decimal. JSON tags use snake_case.
// All struct fields are value types; nullability is encoded via sentinel
// values rather than pointers, except where Postgres needs a real NULL
// (Booking.JournalID, Event.JournalID, Reservation.JournalID).
package core
