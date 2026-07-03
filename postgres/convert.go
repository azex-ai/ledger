package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// --- pgtype <-> decimal ---

func decimalToNumeric(d decimal.Decimal) pgtype.Numeric {
	// Use the big.Int representation
	coeff := d.Coefficient()
	exp := int32(d.Exponent())
	return pgtype.Numeric{
		Int:              coeff,
		Exp:              exp,
		Valid:            true,
		NaN:              false,
		InfinityModifier: pgtype.Finite,
	}
}

func numericToDecimal(n pgtype.Numeric) (decimal.Decimal, error) {
	if !n.Valid {
		return decimal.Zero, nil
	}
	if n.NaN {
		return decimal.Zero, fmt.Errorf("postgres: convert: NaN numeric")
	}
	if n.Int == nil {
		return decimal.Zero, nil
	}
	// decimal = Int * 10^Exp
	d := decimal.NewFromBigInt(n.Int, n.Exp)
	return d, nil
}

func mustNumericToDecimal(n pgtype.Numeric) decimal.Decimal {
	d, err := numericToDecimal(n)
	if err != nil {
		slog.Error("postgres: mustNumericToDecimal: conversion failed, this should not happen with valid DB constraints", "error", err, "numeric", n)
		panic(err)
	}
	return d
}

func numericPtrToDecimalPtr(n pgtype.Numeric) *decimal.Decimal {
	if !n.Valid {
		return nil
	}
	d := mustNumericToDecimal(n)
	return &d
}

// --- pgtype nullable helpers ---

func int64ToInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{Valid: false}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func zeroInt64ToNil(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func timeToTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func metadataToJSON(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func jsonToMetadata(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("postgres: jsonToMetadata: unmarshal failed", "error", err, "raw", string(b[:min(len(b), 200)]))
	}
	return m
}

// anyToDecimal converts the any value returned by COALESCE(SUM(...), 0) to decimal.
func anyToDecimal(v any) (decimal.Decimal, error) {
	if v == nil {
		return decimal.Zero, nil
	}
	switch val := v.(type) {
	case pgtype.Numeric:
		return numericToDecimal(val)
	case *big.Int:
		return decimal.NewFromBigInt(val, 0), nil
	case int64:
		return decimal.NewFromInt(val), nil
	case float64:
		slog.Warn("postgres: anyToDecimal: float64 path hit, possible precision loss", "value", val)
		return decimal.NewFromFloat(val), nil
	case string:
		return decimal.NewFromString(val)
	default:
		// Try numeric scan
		var n pgtype.Numeric
		if err := n.Scan(v); err == nil {
			return numericToDecimal(n)
		}
		return decimal.Zero, fmt.Errorf("postgres: convert: unsupported type %T for decimal", v)
	}
}

func anyToTime(v any) (time.Time, error) {
	if v == nil {
		return time.Time{}, nil
	}
	switch val := v.(type) {
	case time.Time:
		return val, nil
	case pgtype.Timestamptz:
		if !val.Valid {
			return time.Time{}, nil
		}
		return val.Time, nil
	case string:
		return time.Parse(time.RFC3339Nano, val)
	default:
		var ts pgtype.Timestamptz
		if err := ts.Scan(v); err == nil {
			if !ts.Valid {
				return time.Time{}, nil
			}
			return ts.Time, nil
		}
		return time.Time{}, fmt.Errorf("postgres: convert: unsupported type %T for time", v)
	}
}

// --- sqlcgen model -> core model converters ---

func journalFromRow(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, row sqlcgen.Journal) (*core.Journal, error) {
	jt, err := dims.jtByIDOrErr(ctx, q, row.JournalTypeID)
	if err != nil {
		return nil, err
	}
	reversalOfUID := ""
	if row.ReversalOf.Valid {
		u, err := q.GetJournalUIDByID(ctx, row.ReversalOf.Int64)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve reversal_of uid: %w", err)
		}
		reversalOfUID = pgToUID(u)
	}
	eventUID := ""
	if row.EventID != 0 {
		u, err := q.GetEventUIDByID(ctx, row.EventID)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve event uid: %w", err)
		}
		eventUID = pgToUID(u)
	}
	return &core.Journal{
		UID:            pgToUID(row.Uid),
		JournalTypeUID: jt.UID,
		IdempotencyKey: row.IdempotencyKey,
		TotalDebit:     mustNumericToDecimal(row.TotalDebit),
		TotalCredit:    mustNumericToDecimal(row.TotalCredit),
		Metadata:       jsonToMetadata(row.Metadata),
		ActorID:        row.ActorID,
		Source:         row.Source,
		ReversalOfUID:  reversalOfUID,
		EventUID:       eventUID,
		EffectiveAt:    row.EffectiveAt,
		CreatedAt:      row.CreatedAt,
	}, nil
}

// entryCore assembles a core.Entry from raw entry columns plus the parent
// journal's uid (joined into entry list queries so no per-row lookup is
// needed). Dimension ids resolve through the dims cache.
func entryCore(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, journalUID pgtype.UUID, accountHolder, currencyID, classificationID int64, entryType string, amount pgtype.Numeric, effectiveAt, createdAt time.Time) (*core.Entry, error) {
	cur, err := dims.currencyByIDOrErr(ctx, q, currencyID)
	if err != nil {
		return nil, err
	}
	cls, err := dims.classByIDOrErr(ctx, q, classificationID)
	if err != nil {
		return nil, err
	}
	return &core.Entry{
		JournalUID:        pgToUID(journalUID),
		AccountHolder:     accountHolder,
		CurrencyUID:       cur.UID,
		ClassificationUID: cls.UID,
		EntryType:         core.EntryType(entryType),
		Amount:            mustNumericToDecimal(amount),
		EffectiveAt:       effectiveAt,
		CreatedAt:         createdAt,
	}, nil
}

func classificationFromRow(row sqlcgen.Classification) *core.Classification {
	var lifecycle *core.Lifecycle
	if len(row.Lifecycle) > 2 { // skip empty "{}"
		var lc core.Lifecycle
		if err := json.Unmarshal(row.Lifecycle, &lc); err == nil && lc.Initial != "" {
			lifecycle = &lc
		}
	}
	return &core.Classification{
		UID:         pgToUID(row.Uid),
		Code:        row.Code,
		Name:        row.Name,
		NormalSide:  core.NormalSide(row.NormalSide),
		IsSystem:    row.IsSystem,
		IsActive:    row.IsActive,
		BalanceRole: core.BalanceRole(row.BalanceRole),
		Lifecycle:   lifecycle,
		CreatedAt:   row.CreatedAt,
	}
}

func journalTypeFromRow(row sqlcgen.JournalType) *core.JournalType {
	return &core.JournalType{
		UID:       pgToUID(row.Uid),
		Code:      row.Code,
		Name:      row.Name,
		IsActive:  row.IsActive,
		CreatedAt: row.CreatedAt,
	}
}

func currencyFromRow(row sqlcgen.Currency) *core.Currency {
	return &core.Currency{
		UID:      pgToUID(row.Uid),
		Code:     row.Code,
		Name:     row.Name,
		IsActive: row.IsActive,
		Exponent: int32(row.Exponent),
	}
}

func templateFromRow(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, row sqlcgen.EntryTemplate, lines []sqlcgen.EntryTemplateLine) (*core.EntryTemplate, error) {
	coreLines := make([]core.EntryTemplateLine, len(lines))
	for i, l := range lines {
		classDim, err := dims.classByIDOrErr(ctx, q, l.ClassificationID)
		if err != nil {
			return nil, err
		}
		coreLines[i] = core.EntryTemplateLine{
			ClassificationUID: classDim.UID,
			EntryType:         core.EntryType(l.EntryType),
			HolderRole:        core.HolderRole(l.HolderRole),
			AmountKey:         l.AmountKey,
			SortOrder:         int(l.SortOrder),
		}
	}
	jt, err := dims.jtByIDOrErr(ctx, q, row.JournalTypeID)
	if err != nil {
		return nil, err
	}
	return &core.EntryTemplate{
		UID:            pgToUID(row.Uid),
		Code:           row.Code,
		Name:           row.Name,
		JournalTypeUID: jt.UID,
		IsActive:       row.IsActive,
		Lines:          coreLines,
		CreatedAt:      row.CreatedAt,
	}, nil
}

func reservationFromRow(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, row sqlcgen.Reservation) (*core.Reservation, error) {
	cur, err := dims.currencyByIDOrErr(ctx, q, row.CurrencyID)
	if err != nil {
		return nil, err
	}
	// reservations.journal_id is a nullable FK since migration 035 (NULL = no
	// journal linked -> empty uid), same exception shape as bookings.journal_id.
	journalUID := ""
	if row.JournalID.Valid {
		u, err := q.GetJournalUIDByID(ctx, row.JournalID.Int64)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve reservation journal uid: %w", err)
		}
		journalUID = pgToUID(u)
	}
	return &core.Reservation{
		UID:            pgToUID(row.Uid),
		AccountHolder:  row.AccountHolder,
		CurrencyUID:    cur.UID,
		ReservedAmount: mustNumericToDecimal(row.ReservedAmount),
		SettledAmount:  numericPtrToDecimalPtr(row.SettledAmount),
		Status:         core.ReservationStatus(row.Status),
		JournalUID:     journalUID,
		IdempotencyKey: row.IdempotencyKey,
		ExpiresAt:      row.ExpiresAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}

func bookingFromRow(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, row sqlcgen.Booking) (*core.Booking, error) {
	cls, err := dims.classByIDOrErr(ctx, q, row.ClassificationID)
	if err != nil {
		return nil, err
	}
	cur, err := dims.currencyByIDOrErr(ctx, q, row.CurrencyID)
	if err != nil {
		return nil, err
	}
	reservationUID := ""
	if row.ReservationID.Valid {
		u, err := q.GetReservationUIDByID(ctx, row.ReservationID.Int64)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve booking reservation uid: %w", err)
		}
		reservationUID = pgToUID(u)
	}
	journalUID := ""
	if row.JournalID.Valid {
		u, err := q.GetJournalUIDByID(ctx, row.JournalID.Int64)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve booking journal uid: %w", err)
		}
		journalUID = pgToUID(u)
	}
	return &core.Booking{
		UID:               pgToUID(row.Uid),
		ClassificationUID: cls.UID,
		AccountHolder:     row.AccountHolder,
		CurrencyUID:       cur.UID,
		Amount:            mustNumericToDecimal(row.Amount),
		SettledAmount:     mustNumericToDecimal(row.SettledAmount),
		Status:            core.Status(row.Status),
		ChannelName:       row.ChannelName,
		ChannelRef:        row.ChannelRef,
		ReservationUID:    reservationUID,
		JournalUID:        journalUID,
		IdempotencyKey:    row.IdempotencyKey,
		Metadata:          jsonToStringMetadata(row.Metadata),
		ExpiresAt:         row.ExpiresAt,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}, nil
}

func eventFromRow(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, row sqlcgen.Event) (*core.Event, error) {
	cur, err := dims.currencyByIDOrErr(ctx, q, row.CurrencyID)
	if err != nil {
		return nil, err
	}
	bookingUID := ""
	if row.BookingID != 0 {
		u, err := q.GetBookingUIDByID(ctx, row.BookingID)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve event booking uid: %w", err)
		}
		bookingUID = pgToUID(u)
	}
	journalUID := ""
	if row.JournalID.Valid {
		u, err := q.GetJournalUIDByID(ctx, row.JournalID.Int64)
		if err != nil {
			return nil, fmt.Errorf("postgres: resolve event journal uid: %w", err)
		}
		journalUID = pgToUID(u)
	}
	return &core.Event{
		UID:                pgToUID(row.Uid),
		BookingUID:         bookingUID,
		CurrencyUID:        cur.UID,
		JournalUID:         journalUID,
		ClassificationCode: row.ClassificationCode,
		AccountHolder:      row.AccountHolder,
		FromStatus:         core.Status(row.FromStatus),
		ToStatus:           core.Status(row.ToStatus),
		Amount:             mustNumericToDecimal(row.Amount),
		SettledAmount:      mustNumericToDecimal(row.SettledAmount),
		Metadata:           jsonToStringMetadata(row.Metadata),
		OccurredAt:         row.OccurredAt,
		ActorID:            row.ActorID,
		Source:             row.Source,
		Attempts:           row.Attempts,
		MaxAttempts:        row.MaxAttempts,
		NextAttemptAt:      row.NextAttemptAt,
	}, nil
}

func accountPolicyFromRow(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, row sqlcgen.AccountPolicy) (*core.AccountPolicy, error) {
	currencyUID := ""
	if row.CurrencyID != 0 {
		d, err := dims.currencyByIDOrErr(ctx, q, row.CurrencyID)
		if err != nil {
			return nil, err
		}
		currencyUID = d.UID
	}
	classUID := ""
	if row.ClassificationID != 0 {
		d, err := dims.classByIDOrErr(ctx, q, row.ClassificationID)
		if err != nil {
			return nil, err
		}
		classUID = d.UID
	}
	return &core.AccountPolicy{
		UID:               pgToUID(row.Uid),
		AccountHolder:     row.AccountHolder,
		CurrencyUID:       currencyUID,
		ClassificationUID: classUID,
		Status:            core.AccountPolicyStatus(row.Status),
		MinBalance:        mustNumericToDecimal(row.MinBalance),
		EnforceMinBalance: row.EnforceMinBalance,
		Note:              row.Note,
		UpdatedAt:         row.UpdatedAt,
		CreatedAt:         row.CreatedAt,
	}, nil
}

// jsonToStringMetadata reads a JSONB metadata blob into the canonical
// map[string]string form. Rows written before the v0.3 metadata unification
// may carry non-string values (numbers, bools, nested objects) — those are
// rendered to their compact JSON text rather than dropped, so old data stays
// readable without a data migration.
func jsonToStringMetadata(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		slog.Warn("postgres: jsonToStringMetadata: unmarshal failed", "error", err, "raw", string(b[:min(len(b), 200)]))
		return nil
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			m[k] = s
			continue
		}
		m[k] = string(v)
	}
	return m
}

func stringMetadataToJSON(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// --- uid helpers (api-contract §3: uid is the only external identifier) ---

// newUID mints a UUIDv7 for a new row. V7 is time-ordered, which keeps the
// uq_*_uid indexes append-friendly. Failure is impossible in practice
// (crypto/rand); if the platform's entropy source is broken we cannot safely
// write financial rows anyway, so panic instead of threading an error through
// every insert path.
func newUID() pgtype.UUID {
	u, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("postgres: uuid v7 generation failed: %v", err))
	}
	return pgtype.UUID{Bytes: u, Valid: true}
}

// uidToPG parses an external uid string into a pgtype.UUID for query params.
// Returns ErrNotFound for malformed input: from the caller's perspective a
// syntactically invalid uid cannot name any row, and mapping it to "not
// found" avoids a distinct error branch on every lookup.
func uidToPG(uid string) (pgtype.UUID, error) {
	u, err := uuid.Parse(uid)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("postgres: invalid uid %q: %w", uid, core.ErrNotFound)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

func pgToUID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// Opaque keyset cursors: base64 of the internal ordering position. The value
// is deliberately opaque to consumers — it is pagination state, not an entity
// identifier (api-contract §3 note in the v0.4 plan).
func encodeCursorString(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeCursorString(cursor string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("postgres: invalid cursor: %w", core.ErrInvalidInput)
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("postgres: invalid cursor: %w", core.ErrInvalidInput)
	}
	return v, nil
}
