# Phase 1: Core Domain + Schema + Postgres Adapter

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the foundational core package (zero-dependency domain types + interfaces) and PostgreSQL schema + adapter. This is the base everything else depends on.

**Architecture:** Hexagonal — `core/` defines types and interfaces with zero imports, `postgres/` implements them with pgx+sqlc. All amounts use `shopspring/decimal.Decimal`.

**Tech Stack:** Go 1.25+, pgx v5, sqlc, shopspring/decimal, golang-migrate

**Depends on:** Nothing (this is the foundation)

**Blocks:** Phase 2, 3, 4, 5, 6

---

### Task 1: Go Module + Dependencies

**Files:**
- Modify: `go.mod`

**Step 1: Add dependencies**

```bash
cd /Users/aaron/azex/ledger
go get github.com/shopspring/decimal
go get github.com/jackc/pgx/v5
go get github.com/jackc/pgx/v5/pgxpool
go get github.com/golang-migrate/migrate/v4
go get github.com/stretchr/testify
go mod tidy
```

**Step 2: Commit**

```bash
git add go.mod go.sum
git commit -m "feat(core): init go module with dependencies"
```

---

### Task 2: Core Types — Account, Classification, Currency

**Files:**
- Create: `core/types.go`
- Test: `core/types_test.go`

**Step 1: Write the failing test**

```go
// core/types_test.go
package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEntryType_IsValid(t *testing.T) {
	assert.True(t, EntryTypeDebit.IsValid())
	assert.True(t, EntryTypeCredit.IsValid())
	assert.False(t, EntryType("invalid").IsValid())
}

func TestNormalSide_IsValid(t *testing.T) {
	assert.True(t, NormalSideDebit.IsValid())
	assert.True(t, NormalSideCredit.IsValid())
	assert.False(t, NormalSide("invalid").IsValid())
}

func TestSystemAccountHolder(t *testing.T) {
	assert.Equal(t, int64(-42), SystemAccountHolder(42))
	assert.True(t, IsSystemAccount(-42))
	assert.False(t, IsSystemAccount(42))
}
```

**Step 2: Run test to verify it fails**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v -run TestEntryType
```
Expected: FAIL — types not defined

**Step 3: Write minimal implementation**

```go
// core/types.go
package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// EntryType represents debit or credit.
type EntryType string

const (
	EntryTypeDebit  EntryType = "debit"
	EntryTypeCredit EntryType = "credit"
)

func (e EntryType) IsValid() bool {
	return e == EntryTypeDebit || e == EntryTypeCredit
}

// NormalSide indicates default balance direction.
type NormalSide string

const (
	NormalSideDebit  NormalSide = "debit"
	NormalSideCredit NormalSide = "credit"
)

func (n NormalSide) IsValid() bool {
	return n == NormalSideDebit || n == NormalSideCredit
}

// SystemAccountHolder returns the system counterpart for a user.
// Positive = user, negative = system.
func SystemAccountHolder(userID int64) int64 {
	return -userID
}

func IsSystemAccount(holder int64) bool {
	return holder < 0
}

// Currency represents a tradeable currency.
type Currency struct {
	ID   int64
	Code string
	Name string
}

// Classification represents a dynamic account classification.
type Classification struct {
	ID         int64
	Code       string
	Name       string
	NormalSide NormalSide
	IsSystem   bool
	IsActive   bool
	CreatedAt  time.Time
}

// JournalType represents a dynamic journal category.
type JournalType struct {
	ID        int64
	Code      string
	Name      string
	IsActive  bool
	CreatedAt time.Time
}

// Balance represents a computed balance for an account dimension.
type Balance struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Balance          decimal.Decimal
}
```

**Step 4: Run test to verify it passes**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v
```
Expected: PASS

**Step 5: Commit**

```bash
cd /Users/aaron/azex/ledger
git add core/
git commit -m "feat(core): add base types — EntryType, Classification, Currency, Balance"
```

---

### Task 3: Core Types — Journal, Entry, Checkpoint

**Files:**
- Create: `core/journal.go`
- Create: `core/checkpoint.go`
- Test: `core/journal_test.go`

**Step 1: Write the failing test**

```go
// core/journal_test.go
package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJournalInput_Validate_Balanced(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-001",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	require.NoError(t, input.Validate())
}

func TestJournalInput_Validate_Unbalanced(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-002",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unbalanced")
}

func TestJournalInput_Validate_EmptyEntries(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-003",
		Entries:        []EntryInput{},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entries")
}

func TestJournalInput_Validate_ZeroAmount(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-004",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.Zero},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive")
}

func TestJournalInput_Validate_NoIdempotencyKey(t *testing.T) {
	input := JournalInput{
		JournalTypeID: 1,
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency")
}

func TestJournalInput_Totals(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-005",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 3, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(150)},
		},
	}
	debit, credit := input.Totals()
	assert.True(t, debit.Equal(decimal.NewFromInt(150)))
	assert.True(t, credit.Equal(decimal.NewFromInt(150)))
}
```

**Step 2: Run test to verify it fails**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v -run TestJournalInput
```
Expected: FAIL

**Step 3: Write implementation**

```go
// core/journal.go
package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Journal is a persisted balanced journal.
type Journal struct {
	ID             int64
	JournalTypeID  int64
	IdempotencyKey string
	TotalDebit     decimal.Decimal
	TotalCredit    decimal.Decimal
	Metadata       map[string]string
	ActorID        *int64
	Source         string
	ReversalOf     *int64
	CreatedAt      time.Time
}

// Entry is a single debit or credit line in a journal.
type Entry struct {
	ID               int64
	JournalID        int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	EntryType        EntryType
	Amount           decimal.Decimal
	CreatedAt        time.Time
}

// EntryInput is the input for a single entry line.
type EntryInput struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	EntryType        EntryType
	Amount           decimal.Decimal
}

// JournalInput is the input to post a journal.
type JournalInput struct {
	JournalTypeID  int64
	IdempotencyKey string
	Entries        []EntryInput
	Metadata       map[string]string
	ActorID        *int64
	Source         string
	ReversalOf     *int64
}

func (j *JournalInput) Totals() (debit, credit decimal.Decimal) {
	debit = decimal.Zero
	credit = decimal.Zero
	for _, e := range j.Entries {
		switch e.EntryType {
		case EntryTypeDebit:
			debit = debit.Add(e.Amount)
		case EntryTypeCredit:
			credit = credit.Add(e.Amount)
		}
	}
	return debit, credit
}

func (j *JournalInput) Validate() error {
	if j.IdempotencyKey == "" {
		return fmt.Errorf("core: journal: idempotency key required")
	}
	if len(j.Entries) == 0 {
		return fmt.Errorf("core: journal: entries must not be empty")
	}
	for i, e := range j.Entries {
		if !e.EntryType.IsValid() {
			return fmt.Errorf("core: journal: entry[%d]: invalid entry type %q", i, e.EntryType)
		}
		if !e.Amount.IsPositive() {
			return fmt.Errorf("core: journal: entry[%d]: amount must be positive", i)
		}
	}
	debit, credit := j.Totals()
	if !debit.Equal(credit) {
		return fmt.Errorf("core: journal: unbalanced — debit=%s credit=%s", debit, credit)
	}
	return nil
}
```

```go
// core/checkpoint.go
package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// BalanceCheckpoint stores the materialized balance at a point in time.
type BalanceCheckpoint struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Balance          decimal.Decimal
	LastEntryID      int64
	LastEntryAt      time.Time
	UpdatedAt        time.Time
}

// RollupQueueItem represents a pending rollup work item.
type RollupQueueItem struct {
	ID               int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	CreatedAt        time.Time
}

// BalanceSnapshot stores a historical daily balance.
type BalanceSnapshot struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	SnapshotDate     time.Time
	Balance          decimal.Decimal
}

// SystemRollup stores aggregated system-wide balances.
type SystemRollup struct {
	CurrencyID       int64
	ClassificationID int64
	TotalBalance     decimal.Decimal
	UpdatedAt        time.Time
}
```

**Step 4: Run tests**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v
```
Expected: ALL PASS

**Step 5: Commit**

```bash
cd /Users/aaron/azex/ledger
git add core/
git commit -m "feat(core): add Journal, Entry, Checkpoint types with balance validation"
```

---

### Task 4: Core Types — Reserve, Deposit, Withdraw

**Files:**
- Create: `core/reserve.go`
- Create: `core/deposit.go`
- Create: `core/withdraw.go`
- Test: `core/reserve_test.go`
- Test: `core/deposit_test.go`
- Test: `core/withdraw_test.go`

**Step 1: Write failing tests**

```go
// core/reserve_test.go
package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReservationStatus_IsValid(t *testing.T) {
	assert.True(t, ReservationStatusActive.IsValid())
	assert.True(t, ReservationStatusSettling.IsValid())
	assert.True(t, ReservationStatusSettled.IsValid())
	assert.True(t, ReservationStatusReleased.IsValid())
	assert.False(t, ReservationStatus("bogus").IsValid())
}

func TestReservationStatus_CanTransition(t *testing.T) {
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusSettling))
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusReleased))
	assert.True(t, ReservationStatusSettling.CanTransitionTo(ReservationStatusSettled))
	assert.True(t, ReservationStatusSettling.CanTransitionTo(ReservationStatusReleased))
	assert.False(t, ReservationStatusSettled.CanTransitionTo(ReservationStatusActive))
	assert.False(t, ReservationStatusReleased.CanTransitionTo(ReservationStatusActive))
}
```

```go
// core/deposit_test.go
package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDepositStatus_Transitions(t *testing.T) {
	assert.True(t, DepositStatusPending.CanTransitionTo(DepositStatusConfirming))
	assert.True(t, DepositStatusPending.CanTransitionTo(DepositStatusFailed))
	assert.True(t, DepositStatusPending.CanTransitionTo(DepositStatusExpired))
	assert.True(t, DepositStatusConfirming.CanTransitionTo(DepositStatusConfirmed))
	assert.True(t, DepositStatusConfirming.CanTransitionTo(DepositStatusFailed))
	assert.False(t, DepositStatusConfirmed.CanTransitionTo(DepositStatusPending))
	assert.False(t, DepositStatusFailed.CanTransitionTo(DepositStatusConfirmed))
}
```

```go
// core/withdraw_test.go
package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithdrawStatus_Transitions(t *testing.T) {
	// Happy path
	assert.True(t, WithdrawStatusLocked.CanTransitionTo(WithdrawStatusReserved))
	assert.True(t, WithdrawStatusReserved.CanTransitionTo(WithdrawStatusReviewing))
	assert.True(t, WithdrawStatusReserved.CanTransitionTo(WithdrawStatusProcessing)) // skip review
	assert.True(t, WithdrawStatusReviewing.CanTransitionTo(WithdrawStatusProcessing))
	assert.True(t, WithdrawStatusProcessing.CanTransitionTo(WithdrawStatusConfirmed))
	// Failure + retry
	assert.True(t, WithdrawStatusProcessing.CanTransitionTo(WithdrawStatusFailed))
	assert.True(t, WithdrawStatusFailed.CanTransitionTo(WithdrawStatusReserved)) // retry
	// Expired is terminal
	assert.False(t, WithdrawStatusExpired.CanTransitionTo(WithdrawStatusReserved))
}
```

**Step 2: Run tests — expect FAIL**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v -run "TestReservation|TestDeposit|TestWithdraw"
```

**Step 3: Implement**

```go
// core/reserve.go
package core

import (
	"time"

	"github.com/shopspring/decimal"
)

type ReservationStatus string

const (
	ReservationStatusActive   ReservationStatus = "active"
	ReservationStatusSettling ReservationStatus = "settling"
	ReservationStatusSettled  ReservationStatus = "settled"
	ReservationStatusReleased ReservationStatus = "released"
)

var reservationTransitions = map[ReservationStatus][]ReservationStatus{
	ReservationStatusActive:   {ReservationStatusSettling, ReservationStatusReleased},
	ReservationStatusSettling: {ReservationStatusSettled, ReservationStatusReleased},
}

func (s ReservationStatus) IsValid() bool {
	switch s {
	case ReservationStatusActive, ReservationStatusSettling, ReservationStatusSettled, ReservationStatusReleased:
		return true
	}
	return false
}

func (s ReservationStatus) CanTransitionTo(target ReservationStatus) bool {
	for _, allowed := range reservationTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Reservation struct {
	ID             int64
	AccountHolder  int64
	CurrencyID     int64
	ReservedAmount decimal.Decimal
	SettledAmount  *decimal.Decimal
	Status         ReservationStatus
	JournalID      *int64
	IdempotencyKey string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ReserveInput struct {
	AccountHolder  int64
	CurrencyID     int64
	Amount         decimal.Decimal
	IdempotencyKey string
	ExpiresIn      time.Duration
}
```

```go
// core/deposit.go
package core

import (
	"time"

	"github.com/shopspring/decimal"
)

type DepositStatus string

const (
	DepositStatusPending    DepositStatus = "pending"
	DepositStatusConfirming DepositStatus = "confirming"
	DepositStatusConfirmed  DepositStatus = "confirmed"
	DepositStatusFailed     DepositStatus = "failed"
	DepositStatusExpired    DepositStatus = "expired"
)

var depositTransitions = map[DepositStatus][]DepositStatus{
	DepositStatusPending:    {DepositStatusConfirming, DepositStatusFailed, DepositStatusExpired},
	DepositStatusConfirming: {DepositStatusConfirmed, DepositStatusFailed, DepositStatusExpired},
}

func (s DepositStatus) IsValid() bool {
	switch s {
	case DepositStatusPending, DepositStatusConfirming, DepositStatusConfirmed, DepositStatusFailed, DepositStatusExpired:
		return true
	}
	return false
}

func (s DepositStatus) CanTransitionTo(target DepositStatus) bool {
	for _, allowed := range depositTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Deposit struct {
	ID             int64
	AccountHolder  int64
	CurrencyID     int64
	ExpectedAmount decimal.Decimal
	ActualAmount   *decimal.Decimal
	Status         DepositStatus
	ChannelName    string
	ChannelRef     *string
	JournalID      *int64
	IdempotencyKey string
	Metadata       map[string]string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type DepositInput struct {
	AccountHolder  int64
	CurrencyID     int64
	ExpectedAmount decimal.Decimal
	ChannelName    string
	IdempotencyKey string
	Metadata       map[string]string
	ExpiresAt      *time.Time
}

type ConfirmDepositInput struct {
	DepositID    int64
	ActualAmount decimal.Decimal
	ChannelRef   string
}
```

```go
// core/withdraw.go
package core

import (
	"time"

	"github.com/shopspring/decimal"
)

type WithdrawStatus string

const (
	WithdrawStatusLocked     WithdrawStatus = "locked"
	WithdrawStatusReserved   WithdrawStatus = "reserved"
	WithdrawStatusReviewing  WithdrawStatus = "reviewing"
	WithdrawStatusProcessing WithdrawStatus = "processing"
	WithdrawStatusConfirmed  WithdrawStatus = "confirmed"
	WithdrawStatusFailed     WithdrawStatus = "failed"
	WithdrawStatusExpired    WithdrawStatus = "expired"
)

var withdrawTransitions = map[WithdrawStatus][]WithdrawStatus{
	WithdrawStatusLocked:     {WithdrawStatusReserved},
	WithdrawStatusReserved:   {WithdrawStatusReviewing, WithdrawStatusProcessing},
	WithdrawStatusReviewing:  {WithdrawStatusProcessing, WithdrawStatusFailed},
	WithdrawStatusProcessing: {WithdrawStatusConfirmed, WithdrawStatusFailed, WithdrawStatusExpired},
	WithdrawStatusFailed:     {WithdrawStatusReserved}, // retry
}

func (s WithdrawStatus) IsValid() bool {
	switch s {
	case WithdrawStatusLocked, WithdrawStatusReserved, WithdrawStatusReviewing,
		WithdrawStatusProcessing, WithdrawStatusConfirmed, WithdrawStatusFailed, WithdrawStatusExpired:
		return true
	}
	return false
}

func (s WithdrawStatus) CanTransitionTo(target WithdrawStatus) bool {
	for _, allowed := range withdrawTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Withdrawal struct {
	ID             int64
	AccountHolder  int64
	CurrencyID     int64
	Amount         decimal.Decimal
	Status         WithdrawStatus
	ChannelName    string
	ChannelRef     *string
	ReservationID  *int64
	JournalID      *int64
	IdempotencyKey string
	Metadata       map[string]string
	ReviewRequired bool
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type WithdrawInput struct {
	AccountHolder  int64
	CurrencyID     int64
	Amount         decimal.Decimal
	ChannelName    string
	IdempotencyKey string
	ReviewRequired bool
	Metadata       map[string]string
	ExpiresAt      *time.Time
}
```

**Step 4: Run all tests**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v
```
Expected: ALL PASS

**Step 5: Commit**

```bash
cd /Users/aaron/azex/ledger
git add core/
git commit -m "feat(core): add Reserve, Deposit, Withdraw types with state machines"
```

---

### Task 5: Core Types — Template

**Files:**
- Create: `core/template.go`
- Test: `core/template_test.go`

**Step 1: Write failing test**

```go
// core/template_test.go
package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntryTemplate_Render(t *testing.T) {
	tmpl := EntryTemplate{
		ID:            1,
		Code:          "deposit_confirm",
		JournalTypeID: 1,
		IsActive:      true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
			{ClassificationID: 20, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "amount"},
			{ClassificationID: 30, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "fee"},
			{ClassificationID: 40, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "fee"},
		},
	}

	params := TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "tx-100",
		Amounts: map[string]decimal.Decimal{
			"amount": decimal.NewFromInt(1000),
			"fee":    decimal.NewFromInt(5),
		},
	}

	input, err := tmpl.Render(params)
	require.NoError(t, err)
	assert.Equal(t, int64(1), input.JournalTypeID)
	assert.Equal(t, "tx-100", input.IdempotencyKey)
	assert.Len(t, input.Entries, 4)

	// Verify holder resolution
	assert.Equal(t, int64(42), input.Entries[0].AccountHolder)   // user
	assert.Equal(t, int64(-42), input.Entries[1].AccountHolder)  // system

	// Verify balance
	require.NoError(t, input.Validate())
}

func TestEntryTemplate_Render_MissingAmountKey(t *testing.T) {
	tmpl := EntryTemplate{
		ID:       1,
		Code:     "test",
		IsActive: true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
		},
	}
	params := TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "tx-101",
		Amounts:        map[string]decimal.Decimal{},
	}
	_, err := tmpl.Render(params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount key")
}

func TestEntryTemplate_Render_Inactive(t *testing.T) {
	tmpl := EntryTemplate{
		ID:       1,
		Code:     "test",
		IsActive: false,
		Lines:    []EntryTemplateLine{},
	}
	_, err := tmpl.Render(TemplateParams{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inactive")
}
```

**Step 2: Run test — expect FAIL**

**Step 3: Implement**

```go
// core/template.go
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
		return nil, fmt.Errorf("core: template: %q is inactive", t.Code)
	}

	entries := make([]EntryInput, 0, len(t.Lines))
	for i, line := range t.Lines {
		amount, ok := params.Amounts[line.AmountKey]
		if !ok {
			return nil, fmt.Errorf("core: template: line[%d]: missing amount key %q", i, line.AmountKey)
		}

		var holder int64
		switch line.HolderRole {
		case HolderRoleUser:
			holder = params.HolderID
		case HolderRoleSystem:
			holder = SystemAccountHolder(params.HolderID)
		default:
			return nil, fmt.Errorf("core: template: line[%d]: invalid holder role %q", i, line.HolderRole)
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
```

**Step 4: Run tests — expect PASS**

**Step 5: Commit**

```bash
cd /Users/aaron/azex/ledger
git add core/
git commit -m "feat(core): add EntryTemplate with Render and parameter resolution"
```

---

### Task 6: Core Interfaces

**Files:**
- Create: `core/interfaces.go`
- Create: `core/logger.go`
- Create: `core/metrics.go`

**Step 1: Write interfaces** (no test needed — interfaces are compile-time contracts)

```go
// core/interfaces.go
package core

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// JournalWriter handles journal posting.
type JournalWriter interface {
	PostJournal(ctx context.Context, input JournalInput) (*Journal, error)
	ExecuteTemplate(ctx context.Context, templateCode string, params TemplateParams) (*Journal, error)
	ReverseJournal(ctx context.Context, journalID int64, reason string) (*Journal, error)
}

// BalanceReader handles balance queries.
type BalanceReader interface {
	GetBalance(ctx context.Context, holder int64, currencyID, classificationID int64) (decimal.Decimal, error)
	GetBalances(ctx context.Context, holder int64, currencyID int64) ([]Balance, error)
	BatchGetBalances(ctx context.Context, holderIDs []int64, currencyID int64) (map[int64][]Balance, error)
}

// Reserver handles reserve/settle/lock flow.
type Reserver interface {
	Reserve(ctx context.Context, input ReserveInput) (*Reservation, error)
	Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error
	Release(ctx context.Context, reservationID int64) error
}

// Depositor handles deposit state machine.
type Depositor interface {
	InitDeposit(ctx context.Context, input DepositInput) (*Deposit, error)
	ConfirmingDeposit(ctx context.Context, depositID int64, channelRef string) error
	ConfirmDeposit(ctx context.Context, input ConfirmDepositInput) error
	FailDeposit(ctx context.Context, depositID int64, reason string) error
	ExpireDeposit(ctx context.Context, depositID int64) error
}

// Withdrawer handles withdrawal state machine.
type Withdrawer interface {
	InitWithdraw(ctx context.Context, input WithdrawInput) (*Withdrawal, error)
	ReserveWithdraw(ctx context.Context, withdrawalID int64) error
	ReviewWithdraw(ctx context.Context, withdrawalID int64, approved bool) error
	ProcessWithdraw(ctx context.Context, withdrawalID int64, channelRef string) error
	ConfirmWithdraw(ctx context.Context, withdrawalID int64) error
	FailWithdraw(ctx context.Context, withdrawalID int64, reason string) error
	RetryWithdraw(ctx context.Context, withdrawalID int64) error
}

// ChannelAdapter abstracts a deposit/withdraw channel.
type ChannelAdapter interface {
	Name() string
	SupportsDeposit() bool
	SupportsWithdraw() bool
}

// RollupWorker processes async checkpoint updates.
type RollupWorker interface {
	ProcessBatch(ctx context.Context, batchSize int) (int, error)
}

// Reconciler checks accounting equation integrity.
type Reconciler interface {
	CheckAccountingEquation(ctx context.Context) (*ReconcileResult, error)
	ReconcileAccount(ctx context.Context, holder int64, currencyID int64) (*ReconcileResult, error)
}

// ReconcileResult holds the outcome of a reconciliation check.
type ReconcileResult struct {
	Balanced bool
	Gap      decimal.Decimal
	Details  []ReconcileDetail
	CheckedAt time.Time
}

type ReconcileDetail struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Expected         decimal.Decimal
	Actual           decimal.Decimal
	Drift            decimal.Decimal
}

// Snapshotter handles daily balance snapshots.
type Snapshotter interface {
	CreateDailySnapshot(ctx context.Context, date time.Time) error
	GetSnapshotBalance(ctx context.Context, holder int64, currencyID int64, date time.Time) ([]Balance, error)
}

// ClassificationStore manages dynamic classifications.
type ClassificationStore interface {
	CreateClassification(ctx context.Context, input ClassificationInput) (*Classification, error)
	DeactivateClassification(ctx context.Context, id int64) error
	ListClassifications(ctx context.Context, activeOnly bool) ([]Classification, error)
}

type ClassificationInput struct {
	Code       string
	Name       string
	NormalSide NormalSide
	IsSystem   bool
}

// JournalTypeStore manages dynamic journal types.
type JournalTypeStore interface {
	CreateJournalType(ctx context.Context, input JournalTypeInput) (*JournalType, error)
	DeactivateJournalType(ctx context.Context, id int64) error
	ListJournalTypes(ctx context.Context, activeOnly bool) ([]JournalType, error)
}

type JournalTypeInput struct {
	Code string
	Name string
}

// TemplateStore manages entry templates.
type TemplateStore interface {
	CreateTemplate(ctx context.Context, input TemplateInput) (*EntryTemplate, error)
	DeactivateTemplate(ctx context.Context, id int64) error
	GetTemplate(ctx context.Context, code string) (*EntryTemplate, error)
	ListTemplates(ctx context.Context, activeOnly bool) ([]EntryTemplate, error)
}

type TemplateInput struct {
	Code          string
	Name          string
	JournalTypeID int64
	Lines         []TemplateLineInput
}

type TemplateLineInput struct {
	ClassificationID int64
	EntryType        EntryType
	HolderRole       HolderRole
	AmountKey        string
	SortOrder        int
}

// CurrencyStore manages currencies.
type CurrencyStore interface {
	CreateCurrency(ctx context.Context, input CurrencyInput) (*Currency, error)
	ListCurrencies(ctx context.Context) ([]Currency, error)
	GetCurrency(ctx context.Context, id int64) (*Currency, error)
}

type CurrencyInput struct {
	Code string
	Name string
}
```

```go
// core/logger.go
package core

// Logger is the observability interface for structured logging.
// Inject slog, zap, zerolog, or any implementation. Default: nopLogger (silent).
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// NopLogger returns a no-op logger.
func NopLogger() Logger { return nopLogger{} }
```

```go
// core/metrics.go
package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Metrics is the observability interface for counters, histograms, and gauges.
// Inject Prometheus, OpenTelemetry, or DataDog implementation. Default: nopMetrics (silent).
// NOTE: reason/code parameters must be constrained enums, not free-form strings (Prometheus cardinality).
type Metrics interface {
	// Counters
	JournalPosted(journalTypeCode string)
	JournalFailed(journalTypeCode string, reason string)
	ReserveCreated()
	ReserveSettled()
	ReserveReleased()
	RollupProcessed(count int)
	ReconcileCompleted(success bool)
	IdempotencyCollision(journalTypeCode string)
	TemplateFailed(templateCode string, reason string)
	DepositConfirmed(channelName string)
	WithdrawConfirmed(channelName string)

	// Histograms
	JournalLatency(d time.Duration)
	RollupLatency(d time.Duration)
	SnapshotLatency(d time.Duration)
	JournalEntryCount(journalTypeCode string, count int)

	// Gauges
	PendingRollups(count int64)
	ActiveReservations(count int64)
	CheckpointAge(classCode string, age time.Duration)

	// Financial
	BalanceDrift(classCode string, currencyID int64, delta decimal.Decimal)
	ReconcileGap(currencyID int64, gap decimal.Decimal)
	ReservedAmount(currencyID int64, amount decimal.Decimal)
}

type nopMetrics struct{}

func (nopMetrics) JournalPosted(string)                              {}
func (nopMetrics) JournalFailed(string, string)                      {}
func (nopMetrics) ReserveCreated()                                   {}
func (nopMetrics) ReserveSettled()                                   {}
func (nopMetrics) ReserveReleased()                                  {}
func (nopMetrics) RollupProcessed(int)                               {}
func (nopMetrics) ReconcileCompleted(bool)                           {}
func (nopMetrics) IdempotencyCollision(string)                       {}
func (nopMetrics) TemplateFailed(string, string)                     {}
func (nopMetrics) DepositConfirmed(string)                           {}
func (nopMetrics) WithdrawConfirmed(string)                          {}
func (nopMetrics) JournalLatency(time.Duration)                      {}
func (nopMetrics) RollupLatency(time.Duration)                       {}
func (nopMetrics) SnapshotLatency(time.Duration)                     {}
func (nopMetrics) JournalEntryCount(string, int)                     {}
func (nopMetrics) PendingRollups(int64)                              {}
func (nopMetrics) ActiveReservations(int64)                          {}
func (nopMetrics) CheckpointAge(string, time.Duration)               {}
func (nopMetrics) BalanceDrift(string, int64, decimal.Decimal)       {}
func (nopMetrics) ReconcileGap(int64, decimal.Decimal)               {}
func (nopMetrics) ReservedAmount(int64, decimal.Decimal)             {}

// NopMetrics returns a no-op metrics collector.
func NopMetrics() Metrics { return nopMetrics{} }
```

**Step 2: Verify compilation**

```bash
cd /Users/aaron/azex/ledger && go build ./core/
```
Expected: compiles without error

**Step 3: Commit**

```bash
cd /Users/aaron/azex/ledger
git add core/
git commit -m "feat(core): add interfaces, Logger, and Metrics with nop defaults"
```

---

### Task 7: Core Engine — Wiring with Functional Options

**Files:**
- Create: `core/engine.go`
- Test: `core/engine_test.go`

**Step 1: Write failing test**

```go
// core/engine_test.go
package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewEngine_Defaults(t *testing.T) {
	e := NewEngine()
	assert.NotNil(t, e.Logger())
	assert.NotNil(t, e.Metrics())
}

func TestNewEngine_WithOptions(t *testing.T) {
	logger := NopLogger()
	metrics := NopMetrics()
	e := NewEngine(
		WithLogger(logger),
		WithMetrics(metrics),
	)
	assert.Equal(t, logger, e.Logger())
	assert.Equal(t, metrics, e.Metrics())
}
```

**Step 2: Run — expect FAIL**

**Step 3: Implement**

```go
// core/engine.go
package core

// Engine is the central ledger engine holding all dependencies.
type Engine struct {
	logger  Logger
	metrics Metrics
}

// Option configures the Engine.
type Option func(*Engine)

func WithLogger(l Logger) Option {
	return func(e *Engine) { e.logger = l }
}

func WithMetrics(m Metrics) Option {
	return func(e *Engine) { e.metrics = m }
}

func NewEngine(opts ...Option) *Engine {
	e := &Engine{
		logger:  NopLogger(),
		metrics: NopMetrics(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func (e *Engine) Logger() Logger   { return e.logger }
func (e *Engine) Metrics() Metrics { return e.metrics }
```

**Step 4: Run tests — expect PASS**

```bash
cd /Users/aaron/azex/ledger && go test ./core/ -v
```

**Step 5: Commit**

```bash
cd /Users/aaron/azex/ledger
git add core/
git commit -m "feat(core): add Engine with functional options wiring"
```

---

### Task 8: PostgreSQL Schema Migrations

**Files:**
- Create: `postgres/sql/migrations/001_currencies.up.sql`
- Create: `postgres/sql/migrations/002_classifications.up.sql`
- Create: `postgres/sql/migrations/003_templates.up.sql`
- Create: `postgres/sql/migrations/004_ledger.up.sql`
- Create: `postgres/sql/migrations/005_checkpoints.up.sql`
- Create: `postgres/sql/migrations/006_reservations.up.sql`
- Create: `postgres/sql/migrations/007_deposits.up.sql`
- Create: `postgres/sql/migrations/008_withdrawals.up.sql`
- Create: `postgres/sql/migrations/009_snapshots.up.sql`
- Create: `postgres/migrate.go`

**Step 1: Write all migration files**

Copy each `CREATE TABLE` block from the design doc Section 6 into its corresponding file. Include all indexes, constraints, and FKs as specified.

Corresponding `.down.sql` files for each:
```sql
-- 001_currencies.down.sql
DROP TABLE IF EXISTS currencies CASCADE;
```
(Repeat pattern for each migration — DROP in reverse dependency order)

**Step 2: Write migrate.go**

```go
// postgres/migrate.go
package postgres

import (
	"context"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/migrations/*.sql
var migrations embed.FS

// Migrate runs all pending schema migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: migrate: acquire conn: %w", err)
	}
	defer conn.Release()

	source, err := iofs.New(migrations, "sql/migrations")
	if err != nil {
		return fmt.Errorf("postgres: migrate: init source: %w", err)
	}

	driver, err := pgx.WithConnection(ctx, conn.Conn(), &pgx.Config{})
	if err != nil {
		return fmt.Errorf("postgres: migrate: init driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("postgres: migrate: init migrate: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("postgres: migrate: up: %w", err)
	}
	return nil
}
```

**Step 3: Verify compilation**

```bash
cd /Users/aaron/azex/ledger && go build ./postgres/
```

**Step 4: Commit**

```bash
cd /Users/aaron/azex/ledger
git add postgres/
git commit -m "feat(postgres): add schema migrations and Migrate() function"
```

---

### Task 9: sqlc Configuration + Base Queries

**Files:**
- Create: `postgres/sqlc.yaml`
- Create: `postgres/sql/queries/journals.sql`
- Create: `postgres/sql/queries/classifications.sql`
- Create: `postgres/sql/queries/currencies.sql`

**Step 1: Write sqlc config**

```yaml
# postgres/sqlc.yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "sql/queries"
    schema: "sql/migrations"
    gen:
      go:
        package: "sqlcgen"
        out: "sqlcgen"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_empty_slices: true
        overrides:
          - db_type: "numeric"
            go_type:
              import: "github.com/shopspring/decimal"
              type: "Decimal"
          - db_type: "timestamptz"
            go_type: "time.Time"
```

**Step 2: Write initial queries** (journals, classifications, currencies — minimum for PostJournal flow)

See design doc for table structures. Write INSERT/SELECT queries following sqlc conventions.

**Step 3: Generate**

```bash
cd /Users/aaron/azex/ledger/postgres && sqlc generate
```

**Step 4: Verify compilation**

```bash
cd /Users/aaron/azex/ledger && go build ./postgres/...
```

**Step 5: Commit**

```bash
cd /Users/aaron/azex/ledger
git add postgres/
git commit -m "feat(postgres): add sqlc config and base queries"
```
