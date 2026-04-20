package postgres

import (
	"encoding/json"
	"fmt"
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

func decimalPtrToNumeric(d *decimal.Decimal) pgtype.Numeric {
	if d == nil {
		return pgtype.Numeric{Valid: false}
	}
	return decimalToNumeric(*d)
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

func stringToText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: s, Valid: true}
}

func textToStringPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func textToString(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func timeToTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timePtrToTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func timestamptzToTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
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
	_ = json.Unmarshal(b, &m) // Returns nil map on corrupt data; acceptable for metadata
	return m
}

// anyToDecimal converts the interface{} returned by COALESCE(SUM(...), 0) to decimal.
func anyToDecimal(v interface{}) (decimal.Decimal, error) {
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

// --- sqlcgen model -> core model converters ---

func journalFromRow(row sqlcgen.Journal) *core.Journal {
	return &core.Journal{
		ID:             row.ID,
		JournalTypeID:  row.JournalTypeID,
		IdempotencyKey: row.IdempotencyKey,
		TotalDebit:     mustNumericToDecimal(row.TotalDebit),
		TotalCredit:    mustNumericToDecimal(row.TotalCredit),
		Metadata:       jsonToMetadata(row.Metadata),
		ActorID:        int8ToInt64Ptr(row.ActorID),
		Source:         textToString(row.Source),
		ReversalOf:     int8ToInt64Ptr(row.ReversalOf),
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
	return &core.Classification{
		ID:         row.ID,
		Code:       row.Code,
		Name:       row.Name,
		NormalSide: core.NormalSide(row.NormalSide),
		IsSystem:   row.IsSystem,
		IsActive:   row.IsActive,
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
	return &core.Reservation{
		ID:             row.ID,
		AccountHolder:  row.AccountHolder,
		CurrencyID:     row.CurrencyID,
		ReservedAmount: mustNumericToDecimal(row.ReservedAmount),
		SettledAmount:  numericPtrToDecimalPtr(row.SettledAmount),
		Status:         core.ReservationStatus(row.Status),
		JournalID:      int8ToInt64Ptr(row.JournalID),
		IdempotencyKey: row.IdempotencyKey,
		ExpiresAt:      row.ExpiresAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func depositFromRow(row sqlcgen.Deposit) *core.Deposit {
	return &core.Deposit{
		ID:             row.ID,
		AccountHolder:  row.AccountHolder,
		CurrencyID:     row.CurrencyID,
		ExpectedAmount: mustNumericToDecimal(row.ExpectedAmount),
		ActualAmount:   numericPtrToDecimalPtr(row.ActualAmount),
		Status:         core.DepositStatus(row.Status),
		ChannelName:    row.ChannelName,
		ChannelRef:     textToStringPtr(row.ChannelRef),
		JournalID:      int8ToInt64Ptr(row.JournalID),
		IdempotencyKey: row.IdempotencyKey,
		Metadata:       jsonToMetadata(row.Metadata),
		ExpiresAt:      timestamptzToTimePtr(row.ExpiresAt),
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func withdrawalFromRow(row sqlcgen.Withdrawal) *core.Withdrawal {
	return &core.Withdrawal{
		ID:             row.ID,
		AccountHolder:  row.AccountHolder,
		CurrencyID:     row.CurrencyID,
		Amount:         mustNumericToDecimal(row.Amount),
		Status:         core.WithdrawStatus(row.Status),
		ChannelName:    row.ChannelName,
		ChannelRef:     textToStringPtr(row.ChannelRef),
		ReservationID:  int8ToInt64Ptr(row.ReservationID),
		JournalID:      int8ToInt64Ptr(row.JournalID),
		IdempotencyKey: row.IdempotencyKey,
		Metadata:       jsonToMetadata(row.Metadata),
		ReviewRequired: row.ReviewRequired,
		ExpiresAt:      timestamptzToTimePtr(row.ExpiresAt),
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}
