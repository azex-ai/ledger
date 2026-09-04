package postgres_test

// Pins for migration 029 and I-66 / I-67 (docs/INVARIANTS.md): appending a
// row is a mutation, so it is guarded where an invariant exists and recorded
// where one does not.
//
// Every attack below was run as a real `ledger_app` over a socket against a
// clean install of 001-028 before migration 029 was written, and every one
// succeeded silently. Sources: the 2026-09-03 independent review's
// money-out.md (C-1, C-2, M-1, M-4), onchain-ops.md (C-1) and
// install-roles.md (M3, M4).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestTemplateLineCannotBeAppendedAfterInstall is money-out C-1, the single
// most valuable statement in the review: two appended template lines make
// every future honest deposit render at N times its real amount, and the
// journal that does it is genuinely signed, so the gated withdrawal base
// (I-49's V) accepts it, reconciliation passes, verify reports VERIFIED and
// solvency stays balanced because both sides grew together.
//
// The append goes through raw SQL as ledger_app on purpose: that IS the
// attacker's entry point in this threat model, and every application path
// into this table (TemplateStore.CreateTemplate) writes the lines inside the
// transaction that creates the template.
func TestTemplateLineCannotBeAppendedAfterInstall(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-template-not-a-real-secret") //nolint:gosec

	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	curUID := postgrestest.SeedCurrency(t, pool, "TPL", "Template Guard")
	jtUID := postgrestest.SeedJournalType(t, pool, "tpl_deposit_confirm", "Deposit Confirm")
	walletUID := postgrestest.SeedClassification(t, pool, "tpl_main_wallet", "Main Wallet", "debit", false)
	custodialUID := postgrestest.SeedClassification(t, pool, "tpl_custodial", "Custodial", "credit", true)

	tmpl, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:           "tpl_deposit_confirm",
		Name:           "Deposit Confirm",
		JournalTypeUID: jtUID,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: walletUID, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationUID: custodialUID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err, "sanity: the honest installer writes a template and its lines together")
	require.Len(t, tmpl.Lines, 2)

	templateID := postgrestest.InternalID(t, pool, "entry_templates", tmpl.UID)
	walletID := postgrestest.InternalID(t, pool, "classifications", walletUID)
	custodialID := postgrestest.InternalID(t, pool, "classifications", custodialUID)

	t.Run("ledger_app cannot append a line to an installed template", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO entry_template_lines (template_id, classification_id, entry_type, holder_role, amount_key, sort_order)
			VALUES ($1, $2, 'debit',  'user',   'amount', 98),
			       ($1, $3, 'credit', 'system', 'amount', 99)
		`, templateID, walletID, custodialID)
		require.Error(t, err, "appending a line reusing the same amount_key doubles every journal this template renders, with a real signature on it")
		assert.Contains(t, err.Error(), "may only be written by the transaction that created it")
	})

	t.Run("the owner credential cannot either -- this is a trigger, not an ACL", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO entry_template_lines (template_id, classification_id, entry_type, holder_role, amount_key, sort_order)
			VALUES ($1, $2, 'debit', 'user', 'amount', 97)
		`, templateID, walletID)
		require.Error(t, err)
	})

	t.Run("an honest deposit still renders exactly what the template says", func(t *testing.T) {
		j, err := ledgerStore.ExecuteTemplate(ctx, "tpl_deposit_confirm", core.TemplateParams{
			HolderID:       910601,
			CurrencyUID:    curUID,
			IdempotencyKey: postgrestest.UniqueKey("tpl-guard-exec"),
			Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
			Source:         "insert-guard-test",
		})
		require.NoError(t, err)
		assert.True(t, j.TotalDebit.Equal(decimal.NewFromInt(100)),
			"before 029 this was 200 -- the same call, the same code, twice the money: got %s", j.TotalDebit)

		bal, err := ledgerStore.GetBalance(ctx, 910601, curUID, walletUID)
		require.NoError(t, err)
		assert.True(t, bal.Equal(decimal.NewFromInt(100)), "got %s", bal)
	})

	t.Run("installing a second template still works", func(t *testing.T) {
		// The guard's whole risk is over-reach: if it refused the legitimate
		// writer this would be a broken install, not a hardened one.
		second, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
			Code:           "tpl_withdraw_confirm",
			Name:           "Withdraw Confirm",
			JournalTypeUID: jtUID,
			Lines: []core.TemplateLineInput{
				{ClassificationUID: custodialUID, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
				{ClassificationUID: walletUID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
			},
		})
		require.NoError(t, err)
		assert.Len(t, second.Lines, 2)
	})
}

// TestConfigTableInsertsLeaveAForensicRow is money-out M-1. The finding's own
// example: an application legitimately freezes an account with a
// (holder, currency, 0) policy; the attacker appends a MORE SPECIFIC tier,
// which GetEffectiveAccountPolicy prefers, and both the freeze and the
// overdraft floor are gone. I-17 already ruled that prevention is out of
// scope for these three knobs (they are business rules, not tamper-proof
// controls), so I-58's compensating promise -- "a change a guard lets
// through is recorded" -- is the whole defence. Before 029 that promise was
// written in UPDATE semantics and config_table_changes stayed at zero rows.
func TestConfigTableInsertsLeaveAForensicRow(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-audit-not-a-real-secret") //nolint:gosec

	curUID := postgrestest.SeedCurrency(t, pool, "AUD2", "Audit Insert")
	curID := postgrestest.InternalID(t, pool, "currencies", curUID)
	clsUID := postgrestest.SeedClassification(t, pool, "aud_main_wallet", "Main Wallet", "debit", false)
	clsID := postgrestest.InternalID(t, pool, "classifications", clsUID)

	_, err := pool.Exec(ctx, `
		INSERT INTO account_policies (account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance, note, uid)
		VALUES (910701, $1, 0, 'frozen', 0, true, 'legitimate freeze', gen_random_uuid())
	`, curID)
	require.NoError(t, err)

	// The attack: a more specific tier, appended. It still succeeds -- I-17
	// says prevention is out. What must change is that it is now visible.
	_, err = appPool.Exec(ctx, `
		INSERT INTO account_policies (uid, account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance, note)
		VALUES (gen_random_uuid(), 910701, $1, $2, 'active', -1000000, true, '')
	`, curID, clsID)
	require.NoError(t, err, "the guard's whitelist has to permit this -- UpsertAccountPolicy writes the same columns")

	var changedBy, oldRow, newStatus, newMin string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT changed_by, old_row::text, new_row->>'status', new_row->>'min_balance'
		FROM config_table_changes
		WHERE table_name = 'account_policies' AND new_row->>'account_holder' = '910701'
		  AND new_row->>'classification_id' = $1
		ORDER BY id DESC LIMIT 1
	`, fmt.Sprint(clsID)).Scan(&changedBy, &oldRow, &newStatus, &newMin),
		"appending a policy tier that unfreezes an account and moves its overdraft floor must leave a forensic row")

	assert.Equal(t, "ledger_app", changedBy, "the row must name the role that authenticated, not the trigger function's owner")
	assert.Equal(t, "null", oldRow, "an INSERT's 'before' is the JSON null, which is how a reader tells a creation from a change")
	assert.Equal(t, "active", newStatus)
	assert.Equal(t, "-1000000.000000000000000000", newMin)
}

// TestDepositAddressRegistrationIsAudited covers the same rule on the table
// migration 003 was written for. Its UPDATE is guarded to the point of
// permitting nothing, so the only way to point money at a different holder
// is to append -- which the unique indexes stop for an ADDRESS that already
// exists, but not for a new one. The append is legitimate (that is what
// registration is), which is exactly why the record matters.
func TestDepositAddressRegistrationIsAudited(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-depaddr-not-a-real-secret") //nolint:gosec

	_, err := appPool.Exec(ctx, `
		INSERT INTO deposit_addresses (uid, account_holder, address, factory, init_hash)
		VALUES (gen_random_uuid(), 910801, '0xabc0000000000000000000000000000000000001', '0xfactory', '0xinit')
	`)
	require.NoError(t, err)

	var changedBy, holder string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT changed_by, new_row->>'account_holder'
		FROM config_table_changes
		WHERE table_name = 'deposit_addresses'
		ORDER BY id DESC LIMIT 1
	`).Scan(&changedBy, &holder))
	assert.Equal(t, "ledger_app", changedBy)
	assert.Equal(t, "910801", holder)
}

// TestBookingIsBornAtTheStartOfItsLifecycle is the DB half of money-out C-2.
//
// Read the migration header before reading this test: it does NOT close
// C-2. bookings.metadata legitimately carries block_number at INSERT, and
// the recheck loop scans 'pending' as well as 'confirming', so an attacker
// who appends at the initial status with a low block_number still reaches
// auto-credit; that prevention is an application-layer fence in
// service/onchain.go. What this closes is the shorter path -- being born
// already confirming, already linked to a journal, or already part-settled
// -- and it pins the invariant CreateBooking has always held.
func TestBookingIsBornAtTheStartOfItsLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-booking-not-a-real-secret") //nolint:gosec

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:        "insguard_deposit",
		Name:        "Deposit",
		NormalSide:  core.NormalSideDebit,
		IsSystem:    false,
		BalanceRole: core.BalanceRoleAvailable,
		Lifecycle: &core.Lifecycle{
			Initial:  "pending",
			Terminal: []core.Status{"confirmed", "failed"},
			Transitions: map[core.Status][]core.Status{
				"pending":    {"confirming", "failed"},
				"confirming": {"confirmed", "failed"},
			},
		},
	})
	require.NoError(t, err)
	classID := postgrestest.InternalID(t, pool, "classifications", cls.UID)

	curUID := postgrestest.SeedCurrency(t, pool, "BKG", "Booking Guard")
	curID := postgrestest.InternalID(t, pool, "currencies", curUID)

	t.Run("cannot be appended already confirming", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
			                      channel_name, channel_ref, idempotency_key, metadata, uid)
			VALUES ($1, 910901, $2, 999, 'confirming', 'onchain', '0xforged#0', 'deposit-7-0xforged-0',
			        '{"chain_id":"7","tx_hash":"0xforged","txlog_seq":"0","token":"0xtoken","block_number":"1"}'::jsonb,
			        gen_random_uuid())
		`, classID, curID)
		require.Error(t, err, "the review appended exactly this row and the recheck job signed a deposit for it")
		assert.Contains(t, err.Error(), "lifecycle initial status")
	})

	t.Run("cannot be appended already linked to a journal", func(t *testing.T) {
		jtUID := postgrestest.SeedJournalType(t, pool, "insguard_jt", "Insert Guard JT")
		var journalID int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
			SELECT gen_random_uuid(), jt.id, 'insguard-j1', 10, 10 FROM journal_types jt WHERE jt.uid = $1::uuid
			RETURNING id
		`, jtUID).Scan(&journalID))

		_, err := appPool.Exec(ctx, `
			INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status, idempotency_key, journal_id, uid)
			VALUES ($1, 910902, $2, 10, 'pending', 'insguard-prelinked', $3, gen_random_uuid())
		`, classID, curID, journalID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "journal_id is set by the transition")
	})

	t.Run("cannot be appended already part-settled", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO bookings (classification_id, account_holder, currency_id, amount, settled_amount, status, idempotency_key, uid)
			VALUES ($1, 910903, $2, 10, 5, 'pending', 'insguard-presettled', gen_random_uuid())
		`, classID, curID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "created unsettled")
	})

	t.Run("the honest writer is unaffected and leaves a forensic row", func(t *testing.T) {
		b, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
			ClassificationCode: cls.Code,
			AccountHolder:      910904,
			CurrencyUID:        curUID,
			Amount:             decimal.NewFromInt(100),
			IdempotencyKey:     postgrestest.UniqueKey("insguard-honest"),
			ChannelName:        "test",
		})
		require.NoError(t, err)
		assert.Equal(t, core.Status("pending"), b.Status)

		var n int64
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM config_table_changes
			WHERE table_name = 'bookings' AND new_row->>'uid' = $1
		`, b.UID).Scan(&n))
		assert.Equal(t, int64(1), n, "a booking's creation is now recorded, so an appended one is not invisible")
	})
}

// TestReservationIsBornActiveAndUnsettled is the sibling of the booking
// guard on the other table whose UPDATE side was already guarded. A
// reservation appended already 'settled' (or already part-settled) is a
// receipt for a settlement that never ran: SumActiveReservations -- the
// ungated Reserve path's hold -- reads status and settled_amount, so a
// born-settled row is a hold that never existed.
func TestReservationIsBornActiveAndUnsettled(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-reservation-not-a-real-secret") //nolint:gosec

	curUID := postgrestest.SeedCurrency(t, pool, "RSV", "Reservation Guard")
	curID := postgrestest.InternalID(t, pool, "currencies", curUID)

	t.Run("cannot be appended already settled", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO reservations (account_holder, currency_id, reserved_amount, settled_amount, status, idempotency_key, uid)
			VALUES (911001, $1, 50, 50, 'settled', 'insguard-res-settled', gen_random_uuid())
		`, curID)
		require.Error(t, err)
	})

	t.Run("cannot be appended already part-settled", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO reservations (account_holder, currency_id, reserved_amount, settled_amount, idempotency_key, uid)
			VALUES (911002, $1, 50, 25, 'insguard-res-partial', gen_random_uuid())
		`, curID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "created unsettled")
	})

	t.Run("the honest writer is unaffected", func(t *testing.T) {
		// Through the facade, not a hand-assembled store: the point of the
		// subtest is that the path a consumer actually calls still works.
		svc, err := ledger.New(pool)
		require.NoError(t, err)
		_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder:  911003,
			CurrencyUID:    curUID,
			Amount:         decimal.NewFromInt(1),
			IdempotencyKey: postgrestest.UniqueKey("insguard-res-honest"),
			ExpiresIn:      time.Hour,
		})
		// The holder has no balance, so Reserve refuses on funds -- and that
		// refusal is itself the proof the INSERT guard is not what stopped
		// it: a guard rejection surfaces as a check_violation, not
		// ErrInsufficientBalance.
		require.Error(t, err)
		assert.ErrorIs(t, err, core.ErrInsufficientBalance)
	})
}

// TestChainCursorCannotJumpAndEveryMoveIsRecorded is onchain-ops C-1.
//
// chain_cursors.last_scanned_block is the only state deciding which on-chain
// money the ledger can ever be told about, and until 029 it carried zero
// triggers while ledger_app held a plain table UPDATE. A forward jump makes
// every real deposit in the skipped window permanently invisible -- no
// booking, no event, no journal, no entry, so no reconciliation check can
// see it -- while the funds are still swept into treasury.
func TestChainCursorCannotJumpAndEveryMoveIsRecorded(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-cursor-not-a-real-secret") //nolint:gosec

	cursors := postgres.NewChainCursorStore(pool)
	const chainID = int64(911101)
	require.NoError(t, cursors.SetCursor(ctx, chainID, 1000))

	t.Run("the first write is recorded", func(t *testing.T) {
		var changedBy, block string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT changed_by, new_row->>'last_scanned_block'
			FROM config_table_changes
			WHERE table_name = 'chain_cursors' AND new_row->>'chain_id' = $1
			ORDER BY id ASC LIMIT 1
		`, fmt.Sprint(chainID)).Scan(&changedBy, &block))
		assert.Equal(t, "1000", block)
		assert.NotEmpty(t, changedBy)
	})

	t.Run("an ordinary advance still works and is recorded", func(t *testing.T) {
		require.NoError(t, cursors.SetCursor(ctx, chainID, 3000))
		got, err := cursors.GetCursor(ctx, chainID)
		require.NoError(t, err)
		assert.Equal(t, int64(3000), got.LastScannedBlock)

		var n int64
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM config_table_changes
			WHERE table_name = 'chain_cursors' AND new_row->>'chain_id' = $1
		`, fmt.Sprint(chainID)).Scan(&n))
		assert.Equal(t, int64(2), n)
	})

	t.Run("ledger_app cannot jump the cursor past the chain", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `UPDATE chain_cursors SET last_scanned_block = 99999999 WHERE chain_id = $1`, chainID)
		require.Error(t, err, "one statement used to make every deposit between here and eternity invisible")
		assert.Contains(t, err.Error(), "blocks in one write")
	})

	t.Run("ledger_app cannot drag the cursor backwards either", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `UPDATE chain_cursors SET last_scanned_block = 1 WHERE chain_id = $1`, chainID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only moves forward",
			"ledger_app must get the rule, not a bare 42501 from the door predicate whose EXECUTE it does not hold")
	})

	t.Run("nor can the owner, outside the rewind door", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE chain_cursors SET last_scanned_block = 1 WHERE chain_id = $1`, chainID)
		require.Error(t, err, "a raw backwards UPDATE is what the rewind door exists to replace")
		assert.Contains(t, err.Error(), "only moves forward")
	})

	t.Run("ledger_app cannot rewrite the chain a cursor belongs to", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `UPDATE chain_cursors SET chain_id = 999 WHERE chain_id = $1`, chainID)
		require.Error(t, err)
	})

	t.Run("ledger_app cannot call the rewind door", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `SELECT ledger_rewind_chain_cursor($1, 500, 'let me in')`, chainID)
		assertPermissionDenied(t, err)
	})

	t.Run("the owner can rewind, with a reason, and it is recorded", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SELECT ledger_rewind_chain_cursor($1, 500, 'operator re-scan after a bad manual UPDATE')`, chainID)
		require.NoError(t, err)

		got, err := cursors.GetCursor(ctx, chainID)
		require.NoError(t, err)
		assert.Equal(t, int64(500), got.LastScannedBlock)

		var reason, changedBy string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT new_row->>'reason', changed_by FROM config_table_changes
			WHERE table_name = 'ledger_rewind_chain_cursor'
			ORDER BY id DESC LIMIT 1
		`).Scan(&reason, &changedBy))
		assert.Equal(t, "operator re-scan after a bad manual UPDATE", reason)
		assert.NotEmpty(t, changedBy)
	})

	t.Run("a rewind that would do nothing fails loudly", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SELECT ledger_rewind_chain_cursor($1, 900, 'already behind')`, chainID)
		require.Error(t, err, "a repair that reports success having done nothing is the failure mode this repo keeps finding")
	})

	t.Run("a rewind without a reason is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SELECT ledger_rewind_chain_cursor($1, 100, '   ')`, chainID)
		require.Error(t, err)
	})
}

// TestAttestationInsertMustExtendTheChain is money-out M-4 and
// install-roles M4: two findings, one INSERT.
//
// I-27 has always claimed the chain is "gapless, linked, signed", and listed
// RunAttestBatch and VerifyLedger as its enforcers -- both application code,
// in a threat model whose premise is that the application's credential is
// the attacker. What the database enforced was UNIQUE(seq).
func TestAttestationInsertMustExtendTheChain(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-attest-not-a-real-secret") //nolint:gosec

	t.Run("install-roles M4: a high seq cannot be appended", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO ledger_attestations (uid, seq, entry_count, batch_digest, prev_root, root_hash, signature, key_id)
			VALUES (gen_random_uuid(), 888888, 0, ''::bytea, ''::bytea, ''::bytea, ''::bytea, 'x')
		`)
		require.Error(t, err, "seq 888888 raised the chain head that migration 024's anchor ceiling is measured against")
		assert.Contains(t, err.Error(), "extend the chain by one")
	})

	t.Run("MAX(seq) is a ceiling again: the anchor observation stays refused", func(t *testing.T) {
		// 024's whole argument is "the worst a leaked credential can record
		// is the true current chain height". That only holds if the height
		// cannot be chosen.
		_, err := appPool.Exec(ctx, `SELECT ledger_record_anchor_observation(gen_random_uuid(), 888888, ''::bytea)`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "attestation chain only reaches")
	})

	t.Run("seq 1 must link to the genesis root", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO ledger_attestations (uid, seq, entry_count, batch_digest, prev_root, root_hash, signature, key_id)
			VALUES (gen_random_uuid(), 1, 0, decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'),
			        decode(repeat('33', 32), 'hex'), decode(repeat('44', 32), 'hex'), 'k')
		`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "prev_root")
	})

	t.Run("an unsigned attestation testifies to nothing", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO ledger_attestations (uid, seq, entry_count, batch_digest, prev_root, root_hash, signature, key_id)
			VALUES (gen_random_uuid(), 1, 0, decode(repeat('11', 32), 'hex'), decode(repeat('00', 32), 'hex'),
			        decode(repeat('33', 32), 'hex'), ''::bytea, '')
		`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "testifies to nothing")
	})

	t.Run("the honest writer extends the chain, twice", func(t *testing.T) {
		store := postgres.NewAttestationStore(pool)

		first, err := store.InsertAttestation(ctx, core.Attestation{
			Seq: 1, EntryCount: 0,
			BatchDigest: bytesOf(0x11), PrevRoot: core.GenesisRoot, RootHash: bytesOf(0x33),
			Signature: []byte("sig-1"), KeyID: "k1",
		}, nil, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, int64(1), first.Seq)

		_, err = store.InsertAttestation(ctx, core.Attestation{
			Seq: 2, EntryCount: 0,
			BatchDigest: bytesOf(0x44), PrevRoot: bytesOf(0x33), RootHash: bytesOf(0x55),
			Signature: []byte("sig-2"), KeyID: "k1",
		}, nil, nil, nil)
		require.NoError(t, err)

		// ...and a third that does not link is refused, whoever asks.
		_, err = store.InsertAttestation(ctx, core.Attestation{
			Seq: 3, EntryCount: 0,
			BatchDigest: bytesOf(0x66), PrevRoot: bytesOf(0x99), RootHash: bytesOf(0x77),
			Signature: []byte("sig-3"), KeyID: "k1",
		}, nil, nil, nil)
		require.Error(t, err)
	})
}

// TestPoisonedAttestationTailHasAWayBack is money-out M-4's other half, the
// one the seq/prev_root rule cannot reach: GenesisRoot is a public constant
// and root_hash can be any 32 bytes, so an attacker CAN still append one
// shape-valid row at the true head with a signature that does not verify,
// and the honest job will then chain onto it. Before 029 that was permanent
// -- both DELETE guards refused every role including ledger_owner, so the
// only way back was dropping a trigger on the table whose immutability is
// the point.
//
// The finding asked for this by name: "an owner-only, audited procedure to
// quarantine the poison row, otherwise this is irreversible".
func TestPoisonedAttestationTailHasAWayBack(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-discard-not-a-real-secret") //nolint:gosec

	store := postgres.NewAttestationStore(pool)
	_, err := store.InsertAttestation(ctx, core.Attestation{
		Seq: 1, EntryCount: 0,
		BatchDigest: bytesOf(0x11), PrevRoot: core.GenesisRoot, RootHash: bytesOf(0x33),
		Signature: []byte("sig-1"), KeyID: "k1",
	}, nil, nil, nil)
	require.NoError(t, err)

	// The poison: shape-valid, correctly linked, signature nonsense.
	_, err = appPool.Exec(ctx, `
		INSERT INTO ledger_attestations (uid, seq, entry_count, batch_digest, prev_root, root_hash, signature, key_id)
		VALUES (gen_random_uuid(), 2, 0, decode(repeat('aa', 32), 'hex'), decode(repeat('33', 32), 'hex'),
		        decode(repeat('bb', 32), 'hex'), '\x00'::bytea, 'not-a-real-key')
	`)
	require.NoError(t, err, "shape checks are shape checks -- the database cannot verify a signature")

	t.Run("ledger_app still cannot delete anything", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `DELETE FROM ledger_attestations WHERE seq = 2`)
		require.Error(t, err)
	})

	t.Run("ledger_app cannot call the discard door", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `SELECT ledger_discard_attestations_from(2, 'let me in')`)
		assertPermissionDenied(t, err)
	})

	t.Run("a raw owner DELETE is still refused -- the door is the only way", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM ledger_attestations WHERE seq = 2`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})

	// entry_attestations carries the same guard, and this test's fixture
	// writes no coverage rows -- a DELETE here would match nothing, and a
	// BEFORE ROW trigger that is never reached is not a refusal. It is
	// pinned where a row can be made to exist:
	// TestAppendOnlyGuards_EveryTriggerRefusesItsMutation seeds one from the
	// catalogue and drives the DELETE as the owner (R-2, 2026-09-04).

	t.Run("the owner can discard the poisoned suffix, with a reason", func(t *testing.T) {
		var removed int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT ledger_discard_attestations_from(2, 'seq 2 signature does not verify -- incident 2026-09-03')`).Scan(&removed))
		assert.Equal(t, int64(1), removed)

		latest, err := store.LatestAttestation(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), latest.Seq, "the chain is a prefix of what was written, never an edit of it")

		var reason string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT new_row->>'reason' FROM config_table_changes
			WHERE table_name = 'ledger_discard_attestations_from' ORDER BY id DESC LIMIT 1
		`).Scan(&reason))
		assert.Contains(t, reason, "incident 2026-09-03")
	})

	t.Run("and the honest job can extend the chain again", func(t *testing.T) {
		_, err := store.InsertAttestation(ctx, core.Attestation{
			Seq: 2, EntryCount: 0,
			BatchDigest: bytesOf(0x44), PrevRoot: bytesOf(0x33), RootHash: bytesOf(0x55),
			Signature: []byte("sig-2"), KeyID: "k1",
		}, nil, nil, nil)
		require.NoError(t, err, "recovery means the chain grows again, not that it merely stopped being red")
	})

	t.Run("discarding nothing fails loudly", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SELECT ledger_discard_attestations_from(99, 'nothing there')`)
		require.Error(t, err)
	})

	t.Run("discarding without a reason is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SELECT ledger_discard_attestations_from(2, '')`)
		require.Error(t, err)
	})
}

// TestRebalanceDefaultPartitionIsBoundedByData is install-roles M3: 021's
// 120-month argument cap was measured against parameters the caller chose,
// then widened -- uncapped, deliberately -- to cover whatever is sitting in
// the default partition. journal_entries.created_at has no upper bound and
// is one of the columns ledger_app may write, so one balanced pair of
// entries dated 2050 turned a compliant call into 286 partitions / 1716
// relations, each taking ACCESS EXCLUSIVE on journal_entries, none of them
// droppable by the credential that caused it.
func TestRebalanceDefaultPartitionIsBoundedByData(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "insert-guard-app-partition-not-a-real-secret") //nolint:gosec

	curUID := postgrestest.SeedCurrency(t, pool, "PRT", "Partition Guard")
	curID := postgrestest.InternalID(t, pool, "currencies", curUID)
	clsUID := postgrestest.SeedClassification(t, pool, "prt_wallet", "Wallet", "debit", false)
	clsID := postgrestest.InternalID(t, pool, "classifications", clsUID)
	jtUID := postgrestest.SeedJournalType(t, pool, "prt_jt", "Partition JT")

	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
		SELECT gen_random_uuid(), jt.id, 'prt-j1', 5, 5 FROM journal_types jt WHERE jt.uid = $1::uuid
		RETURNING id
	`, jtUID).Scan(&journalID))

	// A balanced pair, dated far in the future, written by ledger_app -- the
	// exact statement the reviewer measured.
	_, err := appPool.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, 911201, $2, $3, 'debit',  5, now(), '2050-06-15'::timestamptz),
		       ($1, 911202, $2, $3, 'credit', 5, now(), '2050-06-15'::timestamptz)
	`, journalID, curID, clsID)
	require.NoError(t, err, "created_at carries no upper bound; that is the input, not the bug")

	before := countPartitions(t, pool)

	var created []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT ledger_rebalance_default_partition('2026-09-01', '2026-12-01')`).Scan(&created))

	after := countPartitions(t, pool)
	assert.LessOrEqual(t, after-before, 1,
		"001 already ships the four months the call asks for, so the only new partition is the one month the forged row is actually in; before 029 the same call built 286 (measured: 1716 relations) because it densely filled every month up to 2050")
	assert.Contains(t, created, "journal_entries_y2050m06", "the month the data IS in must still get its partition -- the row move depends on it")

	// And the row still landed where it belongs -- the fix must not lose data.
	var n int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE account_holder IN (911201, 911202)`).Scan(&n))
	assert.Equal(t, int64(2), n)
}

func countPartitions(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname ~ '^journal_entries_y[0-9]{4}m[0-9]{2}$'
	`).Scan(&n))
	return n
}

// bytesOf builds a 32-byte SHA-256-shaped value -- the width every root,
// digest and prev_root in ledger_attestations is (core.GenesisRootHashLen).
func bytesOf(b byte) []byte {
	out := make([]byte, core.GenesisRootHashLen)
	for i := range out {
		out[i] = b
	}
	return out
}
