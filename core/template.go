package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

type HolderRole string

const (
	HolderRoleUser   HolderRole = "user"
	HolderRoleSystem HolderRole = "system"
)

type EntryTemplate struct {
	ID            int64
	Code          string
	Name          string
	JournalTypeID int64
	IsActive      bool
	Lines         []EntryTemplateLine
	CreatedAt     time.Time
}

type EntryTemplateLine struct {
	ID               int64
	TemplateID       int64
	ClassificationID int64
	EntryType        EntryType
	HolderRole       HolderRole
	AmountKey        string
	SortOrder        int
}

type TemplateParams struct {
	HolderID       int64
	CurrencyID     int64
	IdempotencyKey string
	Amounts        map[string]decimal.Decimal
	ActorID        *int64
	Source         string
	Metadata       map[string]string
}

func (t *EntryTemplate) Render(params TemplateParams) (*JournalInput, error) {
	if !t.IsActive {
		return nil, fmt.Errorf("core: template: %q is inactive: %w", t.Code, ErrInvalidInput)
	}

	entries := make([]EntryInput, 0, len(t.Lines))
	for i, line := range t.Lines {
		amount, ok := params.Amounts[line.AmountKey]
		if !ok {
			return nil, fmt.Errorf("core: template: line[%d]: missing amount key %q: %w", i, line.AmountKey, ErrInvalidInput)
		}

		var holder int64
		switch line.HolderRole {
		case HolderRoleUser:
			holder = params.HolderID
		case HolderRoleSystem:
			holder = SystemAccountHolder(params.HolderID)
		default:
			return nil, fmt.Errorf("core: template: line[%d]: invalid holder role %q: %w", i, line.HolderRole, ErrInvalidInput)
		}

		entries = append(entries, EntryInput{
			AccountHolder:    holder,
			CurrencyID:       params.CurrencyID,
			ClassificationID: line.ClassificationID,
			EntryType:        line.EntryType,
			Amount:           amount,
		})
	}

	input := &JournalInput{
		JournalTypeID:  t.JournalTypeID,
		IdempotencyKey: params.IdempotencyKey,
		Entries:        entries,
		Metadata:       params.Metadata,
		ActorID:        params.ActorID,
		Source:         params.Source,
	}
	return input, nil
}
