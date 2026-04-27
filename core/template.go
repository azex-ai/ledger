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

func (r HolderRole) IsValid() bool {
	return r == HolderRoleUser || r == HolderRoleSystem
}

type EntryTemplate struct {
	ID            int64               `json:"id"`
	Code          string              `json:"code"`
	Name          string              `json:"name"`
	JournalTypeID int64               `json:"journal_type_id"`
	IsActive      bool                `json:"is_active"`
	Lines         []EntryTemplateLine `json:"lines"`
	CreatedAt     time.Time           `json:"created_at"`
}

type EntryTemplateLine struct {
	ID               int64      `json:"id"`
	TemplateID       int64      `json:"template_id"`
	ClassificationID int64      `json:"classification_id"`
	EntryType        EntryType  `json:"entry_type"`
	HolderRole       HolderRole `json:"holder_role"`
	AmountKey        string     `json:"amount_key"`
	SortOrder        int        `json:"sort_order"`
}

type TemplateParams struct {
	HolderID       int64                      `json:"holder_id"`
	CurrencyID     int64                      `json:"currency_id"`
	IdempotencyKey string                     `json:"idempotency_key"`
	Amounts        map[string]decimal.Decimal `json:"amounts"`
	ActorID        int64                      `json:"actor_id"`
	Source         string                     `json:"source"`
	Metadata       map[string]string          `json:"metadata"`
}

type TemplateExecutionRequest struct {
	TemplateCode string         `json:"template_code"`
	Params       TemplateParams `json:"params"`
}

func (t TemplateInput) Validate() error {
	if t.Code == "" {
		return fmt.Errorf("core: template: code required: %w", ErrInvalidInput)
	}
	if t.Name == "" {
		return fmt.Errorf("core: template: name required: %w", ErrInvalidInput)
	}
	if t.JournalTypeID <= 0 {
		return fmt.Errorf("core: template: journal_type_id must be positive: %w", ErrInvalidInput)
	}
	if len(t.Lines) == 0 {
		return fmt.Errorf("core: template: lines must not be empty: %w", ErrInvalidInput)
	}
	for i, line := range t.Lines {
		if err := validateTemplateLine(i, line.ClassificationID, line.EntryType, line.HolderRole, line.AmountKey); err != nil {
			return err
		}
	}
	return nil
}

func (t *EntryTemplate) validateDefinition() error {
	if t == nil {
		return fmt.Errorf("core: template: template is nil: %w", ErrInvalidInput)
	}
	if t.Code == "" {
		return fmt.Errorf("core: template: code required: %w", ErrInvalidInput)
	}
	if t.Name == "" {
		return fmt.Errorf("core: template: name required: %w", ErrInvalidInput)
	}
	if t.JournalTypeID <= 0 {
		return fmt.Errorf("core: template: journal_type_id must be positive: %w", ErrInvalidInput)
	}
	if len(t.Lines) == 0 {
		return fmt.Errorf("core: template: lines must not be empty: %w", ErrInvalidInput)
	}
	for i, line := range t.Lines {
		if err := validateTemplateLine(i, line.ClassificationID, line.EntryType, line.HolderRole, line.AmountKey); err != nil {
			return err
		}
	}
	return nil
}

func validateTemplateLine(i int, classificationID int64, entryType EntryType, holderRole HolderRole, amountKey string) error {
	if classificationID <= 0 {
		return fmt.Errorf("core: template: line[%d]: classification_id must be positive: %w", i, ErrInvalidInput)
	}
	if !entryType.IsValid() {
		return fmt.Errorf("core: template: line[%d]: invalid entry type %q: %w", i, entryType, ErrInvalidInput)
	}
	if !holderRole.IsValid() {
		return fmt.Errorf("core: template: line[%d]: invalid holder role %q: %w", i, holderRole, ErrInvalidInput)
	}
	if amountKey == "" {
		return fmt.Errorf("core: template: line[%d]: amount_key required: %w", i, ErrInvalidInput)
	}
	return nil
}

func (t *EntryTemplate) Render(params TemplateParams) (*JournalInput, error) {
	if !t.IsActive {
		return nil, fmt.Errorf("core: template: %q is inactive: %w", t.Code, ErrInvalidInput)
	}
	if err := t.validateDefinition(); err != nil {
		return nil, err
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
	if err := input.Validate(); err != nil {
		return nil, err
	}
	return input, nil
}
