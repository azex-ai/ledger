package presets

import (
	"context"
	"errors"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

type ClassificationPreset struct {
	Code       string
	Name       string
	NormalSide core.NormalSide
	IsSystem   bool
}

type JournalTypePreset struct {
	Code string
	Name string
}

type TemplateLinePreset struct {
	ClassificationCode string
	EntryType          core.EntryType
	HolderRole         core.HolderRole
	AmountKey          string
	SortOrder          int
}

type TemplatePreset struct {
	Code            string
	Name            string
	JournalTypeCode string
	Lines           []TemplateLinePreset
}

var DefaultTemplateClassifications = []ClassificationPreset{
	{Code: "main_wallet", Name: "Main Wallet", NormalSide: core.NormalSideDebit},
	{Code: "locked", Name: "Locked", NormalSide: core.NormalSideDebit},
	{Code: "pending", Name: "Pending", NormalSide: core.NormalSideCredit},
	{Code: "fee_expense", Name: "Fee Expense", NormalSide: core.NormalSideDebit},
	{Code: "suspense", Name: "Suspense", NormalSide: core.NormalSideDebit, IsSystem: true},
	{Code: "custodial", Name: "Custodial", NormalSide: core.NormalSideCredit, IsSystem: true},
	{Code: "fee_revenue", Name: "Fee Revenue", NormalSide: core.NormalSideCredit, IsSystem: true},
}

var DefaultTemplateJournalTypes = []JournalTypePreset{
	{Code: "deposit_pending", Name: "Deposit Pending"},
	{Code: "deposit_confirm", Name: "Deposit Confirm"},
	{Code: "deposit_confirm_pending", Name: "Deposit Confirm Pending"},
	{Code: "deposit_release_pending", Name: "Deposit Release Pending"},
	{Code: "deposit_record_overage", Name: "Deposit Record Overage"},
	{Code: "deposit_resolve_overage", Name: "Deposit Resolve Overage"},
	{Code: "deposit_release_overage", Name: "Deposit Release Overage"},
	{Code: "lock_funds", Name: "Lock Funds"},
	{Code: "unlock_funds", Name: "Unlock Funds"},
	{Code: "withdraw_confirm", Name: "Withdraw Confirm"},
	{Code: "withdraw_fee", Name: "Withdraw Fee"},
}

var DefaultTemplatePresets = []TemplatePreset{
	{
		Code:            "deposit_pending",
		Name:            "Deposit Pending",
		JournalTypeCode: "deposit_pending",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "suspense", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "pending", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "deposit_confirm",
		Name:            "Deposit Confirm",
		JournalTypeCode: "deposit_confirm",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "deposit_confirm_pending",
		Name:            "Deposit Confirm Pending",
		JournalTypeCode: "deposit_confirm_pending",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "pending", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
			{ClassificationCode: "suspense", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 3},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 4},
		},
	},
	{
		Code:            "deposit_release_pending",
		Name:            "Deposit Release Pending",
		JournalTypeCode: "deposit_release_pending",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "pending", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "suspense", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "deposit_record_overage",
		Name:            "Deposit Record Overage",
		JournalTypeCode: "deposit_record_overage",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "suspense", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "deposit_resolve_overage",
		Name:            "Deposit Resolve Overage",
		JournalTypeCode: "deposit_resolve_overage",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "suspense", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "deposit_release_overage",
		Name:            "Deposit Release Overage",
		JournalTypeCode: "deposit_release_overage",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "suspense", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "lock_funds",
		Name:            "Lock Funds",
		JournalTypeCode: "lock_funds",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "locked", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "unlock_funds",
		Name:            "Unlock Funds",
		JournalTypeCode: "unlock_funds",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "locked", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "withdraw_confirm",
		Name:            "Withdraw Confirm",
		JournalTypeCode: "withdraw_confirm",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "locked", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "withdraw_fee",
		Name:            "Withdraw Fee",
		JournalTypeCode: "withdraw_fee",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "fee_expense", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "fee", SortOrder: 1},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "fee", SortOrder: 2},
			{ClassificationCode: "locked", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "fee", SortOrder: 3},
			{ClassificationCode: "fee_revenue", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "fee", SortOrder: 4},
		},
	},
}

func InstallDefaultTemplatePresets(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplatePresets(
		ctx,
		classifications,
		journalTypes,
		templates,
		DefaultTemplateClassifications,
		DefaultTemplateJournalTypes,
		DefaultTemplatePresets,
	)
}

func InstallTemplatePresets(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
	classificationPresets []ClassificationPreset,
	journalTypePresets []JournalTypePreset,
	templatePresets []TemplatePreset,
) error {
	classificationByCode := make(map[string]*core.Classification, len(classificationPresets))
	for _, preset := range classificationPresets {
		classification, err := ensureClassificationPreset(ctx, classifications, preset)
		if err != nil {
			return fmt.Errorf("presets: ensure classification %q: %w", preset.Code, err)
		}
		classificationByCode[preset.Code] = classification
	}

	journalTypeByCode := make(map[string]*core.JournalType, len(journalTypePresets))
	for _, preset := range journalTypePresets {
		journalType, err := ensureJournalTypePreset(ctx, journalTypes, preset)
		if err != nil {
			return fmt.Errorf("presets: ensure journal type %q: %w", preset.Code, err)
		}
		journalTypeByCode[preset.Code] = journalType
	}

	for _, preset := range templatePresets {
		expected, err := buildTemplateInput(preset, classificationByCode, journalTypeByCode)
		if err != nil {
			return err
		}

		existing, err := templates.GetTemplate(ctx, preset.Code)
		if err == nil {
			if err := validateExistingTemplatePreset(existing, preset, expected); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, core.ErrNotFound) {
			return fmt.Errorf("presets: get template %q: %w", preset.Code, err)
		}

		if _, err := templates.CreateTemplate(ctx, expected); err != nil {
			return fmt.Errorf("presets: create template %q: %w", preset.Code, err)
		}
	}

	return nil
}

func ensureClassificationPreset(
	ctx context.Context,
	classifications core.ClassificationStore,
	preset ClassificationPreset,
) (*core.Classification, error) {
	classification, err := classifications.GetByCode(ctx, preset.Code)
	if errors.Is(err, core.ErrNotFound) {
		return classifications.CreateClassification(ctx, core.ClassificationInput{
			Code:       preset.Code,
			Name:       preset.Name,
			NormalSide: preset.NormalSide,
			IsSystem:   preset.IsSystem,
		})
	}
	if err != nil {
		return nil, err
	}
	if classification.NormalSide != preset.NormalSide {
		return nil, fmt.Errorf(
			"existing classification %q has normal_side=%q, want %q: %w",
			preset.Code, classification.NormalSide, preset.NormalSide, core.ErrInvalidInput,
		)
	}
	if classification.IsSystem != preset.IsSystem {
		return nil, fmt.Errorf(
			"existing classification %q has is_system=%t, want %t: %w",
			preset.Code, classification.IsSystem, preset.IsSystem, core.ErrInvalidInput,
		)
	}
	if !classification.IsActive {
		return nil, fmt.Errorf("existing classification %q is inactive: %w", preset.Code, core.ErrInvalidInput)
	}
	return classification, nil
}

func ensureJournalTypePreset(
	ctx context.Context,
	journalTypes core.JournalTypeStore,
	preset JournalTypePreset,
) (*core.JournalType, error) {
	journalType, err := journalTypes.GetJournalTypeByCode(ctx, preset.Code)
	if errors.Is(err, core.ErrNotFound) {
		return journalTypes.CreateJournalType(ctx, core.JournalTypeInput{
			Code: preset.Code,
			Name: preset.Name,
		})
	}
	if err != nil {
		return nil, err
	}
	if !journalType.IsActive {
		return nil, fmt.Errorf("existing journal type %q is inactive: %w", preset.Code, core.ErrInvalidInput)
	}
	return journalType, nil
}

func buildTemplateInput(
	preset TemplatePreset,
	classificationByCode map[string]*core.Classification,
	journalTypeByCode map[string]*core.JournalType,
) (core.TemplateInput, error) {
	journalType, ok := journalTypeByCode[preset.JournalTypeCode]
	if !ok {
		return core.TemplateInput{}, fmt.Errorf(
			"presets: template %q references unknown journal type %q: %w",
			preset.Code, preset.JournalTypeCode, core.ErrInvalidInput,
		)
	}

	lines := make([]core.TemplateLineInput, len(preset.Lines))
	for i, line := range preset.Lines {
		classification, ok := classificationByCode[line.ClassificationCode]
		if !ok {
			return core.TemplateInput{}, fmt.Errorf(
				"presets: template %q line[%d] references unknown classification %q: %w",
				preset.Code, i, line.ClassificationCode, core.ErrInvalidInput,
			)
		}

		lines[i] = core.TemplateLineInput{
			ClassificationID: classification.ID,
			EntryType:        line.EntryType,
			HolderRole:       line.HolderRole,
			AmountKey:        line.AmountKey,
			SortOrder:        line.SortOrder,
		}
	}

	return core.TemplateInput{
		Code:          preset.Code,
		Name:          preset.Name,
		JournalTypeID: journalType.ID,
		Lines:         lines,
	}, nil
}

func validateExistingTemplatePreset(existing *core.EntryTemplate, preset TemplatePreset, expected core.TemplateInput) error {
	if !existing.IsActive {
		return fmt.Errorf("presets: existing template %q is inactive: %w", preset.Code, core.ErrInvalidInput)
	}
	if existing.JournalTypeID != expected.JournalTypeID {
		return fmt.Errorf(
			"presets: existing template %q has journal_type_id=%d, want %d: %w",
			preset.Code, existing.JournalTypeID, expected.JournalTypeID, core.ErrInvalidInput,
		)
	}
	if len(existing.Lines) != len(expected.Lines) {
		return fmt.Errorf(
			"presets: existing template %q has %d lines, want %d: %w",
			preset.Code, len(existing.Lines), len(expected.Lines), core.ErrInvalidInput,
		)
	}
	for i, line := range expected.Lines {
		existingLine := existing.Lines[i]
		if existingLine.ClassificationID != line.ClassificationID ||
			existingLine.EntryType != line.EntryType ||
			existingLine.HolderRole != line.HolderRole ||
			existingLine.AmountKey != line.AmountKey ||
			existingLine.SortOrder != line.SortOrder {
			return fmt.Errorf(
				"presets: existing template %q line[%d] does not match preset: %w",
				preset.Code, i, core.ErrInvalidInput,
			)
		}
	}
	return nil
}
