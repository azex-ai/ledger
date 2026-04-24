package presets

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDepositLifecycle_Validate(t *testing.T) {
	assert.NoError(t, DepositLifecycle.Validate())
}

func TestDepositLifecycle_Transitions(t *testing.T) {
	lc := DepositLifecycle

	tests := []struct {
		name string
		from core.Status
		to   core.Status
		want bool
	}{
		{"pending -> confirming", "pending", "confirming", true},
		{"pending -> confirmed (must go through confirming)", "pending", "confirmed", false},
		{"pending -> failed", "pending", "failed", true},
		{"pending -> expired", "pending", "expired", true},
		{"confirming -> confirmed", "confirming", "confirmed", true},
		{"confirming -> failed", "confirming", "failed", true},
		{"confirmed -> anything (terminal)", "confirmed", "pending", false},
		{"failed -> anything (terminal)", "failed", "pending", false},
		{"expired -> anything (terminal)", "expired", "pending", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.CanTransition(tt.from, tt.to))
		})
	}
}

func TestWithdrawalLifecycle_Validate(t *testing.T) {
	assert.NoError(t, WithdrawalLifecycle.Validate())
}

func TestWithdrawalLifecycle_Transitions(t *testing.T) {
	lc := WithdrawalLifecycle

	tests := []struct {
		name string
		from core.Status
		to   core.Status
		want bool
	}{
		{"locked -> reserved", "locked", "reserved", true},
		{"reserved -> reviewing", "reserved", "reviewing", true},
		{"reserved -> processing", "reserved", "processing", true},
		{"reviewing -> processing", "reviewing", "processing", true},
		{"reviewing -> failed", "reviewing", "failed", true},
		{"processing -> confirmed", "processing", "confirmed", true},
		{"processing -> failed", "processing", "failed", true},
		{"processing -> expired", "processing", "expired", true},
		{"failed -> reserved (retry)", "failed", "reserved", true},
		{"expired -> anything (terminal)", "expired", "reserved", false},
		{"confirmed -> anything (terminal)", "confirmed", "locked", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.CanTransition(tt.from, tt.to))
		})
	}
}

type fakeClassificationStore struct {
	nextID          int64
	classifications map[string]*core.Classification
}

func newFakeClassificationStore() *fakeClassificationStore {
	return &fakeClassificationStore{
		nextID:          1,
		classifications: make(map[string]*core.Classification),
	}
}

func (s *fakeClassificationStore) CreateClassification(_ context.Context, input core.ClassificationInput) (*core.Classification, error) {
	if existing, ok := s.classifications[input.Code]; ok {
		return existing, nil
	}
	classification := &core.Classification{
		ID:         s.nextID,
		Code:       input.Code,
		Name:       input.Name,
		NormalSide: input.NormalSide,
		IsSystem:   input.IsSystem,
		IsActive:   true,
		Lifecycle:  input.Lifecycle,
		CreatedAt:  time.Now(),
	}
	s.nextID++
	s.classifications[input.Code] = classification
	return classification, nil
}

func (s *fakeClassificationStore) GetByCode(_ context.Context, code string) (*core.Classification, error) {
	classification, ok := s.classifications[code]
	if !ok {
		return nil, core.ErrNotFound
	}
	return classification, nil
}

func (s *fakeClassificationStore) DeactivateClassification(_ context.Context, id int64) error {
	for _, classification := range s.classifications {
		if classification.ID == id {
			classification.IsActive = false
			return nil
		}
	}
	return core.ErrNotFound
}

func (s *fakeClassificationStore) ListClassifications(_ context.Context, activeOnly bool) ([]core.Classification, error) {
	result := make([]core.Classification, 0, len(s.classifications))
	for _, classification := range s.classifications {
		if activeOnly && !classification.IsActive {
			continue
		}
		result = append(result, *classification)
	}
	return result, nil
}

type fakeJournalTypeStore struct {
	nextID       int64
	journalTypes map[string]*core.JournalType
}

func newFakeJournalTypeStore() *fakeJournalTypeStore {
	return &fakeJournalTypeStore{
		nextID:       1,
		journalTypes: make(map[string]*core.JournalType),
	}
}

func (s *fakeJournalTypeStore) CreateJournalType(_ context.Context, input core.JournalTypeInput) (*core.JournalType, error) {
	if existing, ok := s.journalTypes[input.Code]; ok {
		return existing, nil
	}
	journalType := &core.JournalType{
		ID:        s.nextID,
		Code:      input.Code,
		Name:      input.Name,
		IsActive:  true,
		CreatedAt: time.Now(),
	}
	s.nextID++
	s.journalTypes[input.Code] = journalType
	return journalType, nil
}

func (s *fakeJournalTypeStore) GetJournalTypeByCode(_ context.Context, code string) (*core.JournalType, error) {
	journalType, ok := s.journalTypes[code]
	if !ok {
		return nil, core.ErrNotFound
	}
	return journalType, nil
}

func (s *fakeJournalTypeStore) DeactivateJournalType(_ context.Context, id int64) error {
	for _, journalType := range s.journalTypes {
		if journalType.ID == id {
			journalType.IsActive = false
			return nil
		}
	}
	return core.ErrNotFound
}

func (s *fakeJournalTypeStore) ListJournalTypes(_ context.Context, activeOnly bool) ([]core.JournalType, error) {
	result := make([]core.JournalType, 0, len(s.journalTypes))
	for _, journalType := range s.journalTypes {
		if activeOnly && !journalType.IsActive {
			continue
		}
		result = append(result, *journalType)
	}
	return result, nil
}

type fakeTemplateStore struct {
	nextID    int64
	templates map[string]*core.EntryTemplate
}

func newFakeTemplateStore() *fakeTemplateStore {
	return &fakeTemplateStore{
		nextID:    1,
		templates: make(map[string]*core.EntryTemplate),
	}
}

func (s *fakeTemplateStore) CreateTemplate(_ context.Context, input core.TemplateInput) (*core.EntryTemplate, error) {
	if existing, ok := s.templates[input.Code]; ok {
		return existing, nil
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}
	lines := make([]core.EntryTemplateLine, len(input.Lines))
	for i, line := range input.Lines {
		lines[i] = core.EntryTemplateLine{
			ID:               int64(i + 1),
			TemplateID:       s.nextID,
			ClassificationID: line.ClassificationID,
			EntryType:        line.EntryType,
			HolderRole:       line.HolderRole,
			AmountKey:        line.AmountKey,
			SortOrder:        line.SortOrder,
		}
	}
	template := &core.EntryTemplate{
		ID:            s.nextID,
		Code:          input.Code,
		Name:          input.Name,
		JournalTypeID: input.JournalTypeID,
		IsActive:      true,
		Lines:         lines,
		CreatedAt:     time.Now(),
	}
	s.nextID++
	s.templates[input.Code] = template
	return template, nil
}

func (s *fakeTemplateStore) DeactivateTemplate(_ context.Context, id int64) error {
	for _, template := range s.templates {
		if template.ID == id {
			template.IsActive = false
			return nil
		}
	}
	return core.ErrNotFound
}

func (s *fakeTemplateStore) GetTemplate(_ context.Context, code string) (*core.EntryTemplate, error) {
	template, ok := s.templates[code]
	if !ok {
		return nil, core.ErrNotFound
	}
	return template, nil
}

func (s *fakeTemplateStore) ListTemplates(_ context.Context, activeOnly bool) ([]core.EntryTemplate, error) {
	result := make([]core.EntryTemplate, 0, len(s.templates))
	for _, template := range s.templates {
		if activeOnly && !template.IsActive {
			continue
		}
		result = append(result, *template)
	}
	return result, nil
}

func TestInstallDefaultTemplatePresets(t *testing.T) {
	ctx := context.Background()
	classifications := newFakeClassificationStore()
	journalTypes := newFakeJournalTypeStore()
	templates := newFakeTemplateStore()

	err := InstallDefaultTemplatePresets(ctx, classifications, journalTypes, templates)
	require.NoError(t, err)

	for _, preset := range DefaultTemplateClassifications {
		_, err := classifications.GetByCode(ctx, preset.Code)
		require.NoError(t, err, preset.Code)
	}
	for _, preset := range DefaultTemplateJournalTypes {
		_, err := journalTypes.GetJournalTypeByCode(ctx, preset.Code)
		require.NoError(t, err, preset.Code)
	}
	for _, preset := range DefaultTemplatePresets {
		template, err := templates.GetTemplate(ctx, preset.Code)
		require.NoError(t, err, preset.Code)
		assert.Equal(t, preset.Name, template.Name)
		assert.Len(t, template.Lines, len(preset.Lines))
	}
}

func TestInstallDefaultTemplatePresets_Idempotent(t *testing.T) {
	ctx := context.Background()
	classifications := newFakeClassificationStore()
	journalTypes := newFakeJournalTypeStore()
	templates := newFakeTemplateStore()

	require.NoError(t, InstallDefaultTemplatePresets(ctx, classifications, journalTypes, templates))
	require.NoError(t, InstallDefaultTemplatePresets(ctx, classifications, journalTypes, templates))

	assert.Len(t, classifications.classifications, len(DefaultTemplateClassifications))
	assert.Len(t, journalTypes.journalTypes, len(DefaultTemplateJournalTypes))
	assert.Len(t, templates.templates, len(DefaultTemplatePresets))
}

func TestInstallTemplatePresets_MissingDependency(t *testing.T) {
	ctx := context.Background()
	classifications := newFakeClassificationStore()
	journalTypes := newFakeJournalTypeStore()
	templates := newFakeTemplateStore()

	err := InstallTemplatePresets(
		ctx,
		classifications,
		journalTypes,
		templates,
		nil,
		nil,
		[]TemplatePreset{{
			Code:            "broken",
			Name:            "Broken",
			JournalTypeCode: "missing",
			Lines: []TemplateLinePreset{
				{ClassificationCode: "missing", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			},
		}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("%q", "missing"))
}

func TestInstallTemplatePresets_RejectsConflictingClassification(t *testing.T) {
	ctx := context.Background()
	classifications := newFakeClassificationStore()
	journalTypes := newFakeJournalTypeStore()
	templates := newFakeTemplateStore()

	classifications.classifications["main_wallet"] = &core.Classification{
		ID:         1,
		Code:       "main_wallet",
		Name:       "Main Wallet",
		NormalSide: core.NormalSideCredit,
		IsActive:   true,
	}

	err := InstallDefaultTemplatePresets(ctx, classifications, journalTypes, templates)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "main_wallet")
}

func TestInstallTemplatePresets_RejectsConflictingTemplate(t *testing.T) {
	ctx := context.Background()
	classifications := newFakeClassificationStore()
	journalTypes := newFakeJournalTypeStore()
	templates := newFakeTemplateStore()

	require.NoError(t, InstallDefaultTemplatePresets(ctx, classifications, journalTypes, templates))

	withdrawJournalType, err := journalTypes.GetJournalTypeByCode(ctx, "withdraw_confirm")
	require.NoError(t, err)
	mainWallet, err := classifications.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	custodial, err := classifications.GetByCode(ctx, "custodial")
	require.NoError(t, err)

	templates.templates["withdraw_confirm"] = &core.EntryTemplate{
		ID:            999,
		Code:          "withdraw_confirm",
		Name:          "Withdraw Confirm",
		JournalTypeID: withdrawJournalType.ID,
		IsActive:      true,
		Lines: []core.EntryTemplateLine{
			{
				ID:               1,
				TemplateID:       999,
				ClassificationID: mainWallet.ID,
				EntryType:        core.EntryTypeDebit,
				HolderRole:       core.HolderRoleUser,
				AmountKey:        "amount",
				SortOrder:        1,
			},
			{
				ID:               2,
				TemplateID:       999,
				ClassificationID: custodial.ID,
				EntryType:        core.EntryTypeCredit,
				HolderRole:       core.HolderRoleSystem,
				AmountKey:        "amount",
				SortOrder:        2,
			},
		},
	}

	err = InstallDefaultTemplatePresets(ctx, classifications, journalTypes, templates)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "withdraw_confirm")
}
