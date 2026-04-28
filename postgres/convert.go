package postgres

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"time"

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

func int8ToInt64Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
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

func journalFromRow(row sqlcgen.Journal) *core.Journal {
	reversalOf := int64(0)
	if row.ReversalOf.Valid {
		reversalOf = row.ReversalOf.Int64
	}
	return &core.Journal{
		ID:             row.ID,
		JournalTypeID:  row.JournalTypeID,
		IdempotencyKey: row.IdempotencyKey,
		TotalDebit:     mustNumericToDecimal(row.TotalDebit),
		TotalCredit:    mustNumericToDecimal(row.TotalCredit),
		Metadata:       jsonToMetadata(row.Metadata),
		ActorID:        row.ActorID,
		Source:         row.Source,
		ReversalOf:     reversalOf,
		EventID:        row.EventID,
		CreatedAt:      row.CreatedAt,
	}
}

func entryFromRow(row sqlcgen.JournalEntry) *core.Entry {
	var id int64
	if row.ID.Valid {
		id = row.ID.Int64
	}
	return &core.Entry{
		ID:               id,
		JournalID:        row.JournalID,
		AccountHolder:    row.AccountHolder,
		CurrencyID:       row.CurrencyID,
		ClassificationID: row.ClassificationID,
		EntryType:        core.EntryType(row.EntryType),
		Amount:           mustNumericToDecimal(row.Amount),
		CreatedAt:        row.CreatedAt,
	}
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
		ID:         row.ID,
		Code:       row.Code,
		Name:       row.Name,
		NormalSide: core.NormalSide(row.NormalSide),
		IsSystem:   row.IsSystem,
		IsActive:   row.IsActive,
		Lifecycle:  lifecycle,
		CreatedAt:  row.CreatedAt,
	}
}

func journalTypeFromRow(row sqlcgen.JournalType) *core.JournalType {
	return &core.JournalType{
		ID:        row.ID,
		Code:      row.Code,
		Name:      row.Name,
		IsActive:  row.IsActive,
		CreatedAt: row.CreatedAt,
	}
}

func currencyFromRow(row sqlcgen.Currency) *core.Currency {
	return &core.Currency{
		ID:   row.ID,
		Code: row.Code,
		Name: row.Name,
	}
}

func templateFromRow(row sqlcgen.EntryTemplate, lines []sqlcgen.EntryTemplateLine) *core.EntryTemplate {
	coreLines := make([]core.EntryTemplateLine, len(lines))
	for i, l := range lines {
		coreLines[i] = core.EntryTemplateLine{
			ID:               l.ID,
			TemplateID:       l.TemplateID,
			ClassificationID: l.ClassificationID,
			EntryType:        core.EntryType(l.EntryType),
			HolderRole:       core.HolderRole(l.HolderRole),
			AmountKey:        l.AmountKey,
			SortOrder:        int(l.SortOrder),
		}
	}
	return &core.EntryTemplate{
		ID:            row.ID,
		Code:          row.Code,
		Name:          row.Name,
		JournalTypeID: row.JournalTypeID,
		IsActive:      row.IsActive,
		Lines:         coreLines,
		CreatedAt:     row.CreatedAt,
	}
}

func reservationFromRow(row sqlcgen.Reservation) *core.Reservation {
	// reservations.journal_id is still a NOT NULL DEFAULT 0 column (no FK);
	// migration 017 forced that. Map sentinel 0 -> nil pointer for callers.
	journalID := zeroInt64ToNil(row.JournalID)
	return &core.Reservation{
		ID:             row.ID,
		AccountHolder:  row.AccountHolder,
		CurrencyID:     row.CurrencyID,
		ReservedAmount: mustNumericToDecimal(row.ReservedAmount),
		SettledAmount:  numericPtrToDecimalPtr(row.SettledAmount),
		Status:         core.ReservationStatus(row.Status),
		JournalID:      journalID,
		IdempotencyKey: row.IdempotencyKey,
		ExpiresAt:      row.ExpiresAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func bookingFromRow(row sqlcgen.Booking) *core.Booking {
	return &core.Booking{
		ID:               row.ID,
		ClassificationID: row.ClassificationID,
		AccountHolder:    row.AccountHolder,
		CurrencyID:       row.CurrencyID,
		Amount:           mustNumericToDecimal(row.Amount),
		SettledAmount:    mustNumericToDecimal(row.SettledAmount),
		Status:           core.Status(row.Status),
		ChannelName:      row.ChannelName,
		ChannelRef:       row.ChannelRef,
		ReservationID:    int8ToInt64Ptr(row.ReservationID),
		JournalID:        int8ToInt64Ptr(row.JournalID),
		IdempotencyKey:   row.IdempotencyKey,
		Metadata:         jsonToAnyMetadata(row.Metadata),
		ExpiresAt:        row.ExpiresAt,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

func eventFromRow(row sqlcgen.Event) *core.Event {
	return &core.Event{
		ID:                 row.ID,
		ClassificationCode: row.ClassificationCode,
		BookingID:          row.BookingID,
		AccountHolder:      row.AccountHolder,
		CurrencyID:         row.CurrencyID,
		FromStatus:         core.Status(row.FromStatus),
		ToStatus:           core.Status(row.ToStatus),
		Amount:             mustNumericToDecimal(row.Amount),
		SettledAmount:      mustNumericToDecimal(row.SettledAmount),
		JournalID:          int8ToInt64Ptr(row.JournalID),
		Metadata:           jsonToAnyMetadata(row.Metadata),
		OccurredAt:         row.OccurredAt,
		Attempts:           row.Attempts,
		MaxAttempts:        row.MaxAttempts,
		NextAttemptAt:      row.NextAttemptAt,
	}
}

func jsonToAnyMetadata(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("postgres: jsonToAnyMetadata: unmarshal failed", "error", err, "raw", string(b[:min(len(b), 200)]))
	}
	return m
}

func anyMetadataToJSON(m map[string]any) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
