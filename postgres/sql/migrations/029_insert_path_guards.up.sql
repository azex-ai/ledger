-- Independent review 2026-09-03 (docs/audits/2026-09-03-independent-review/):
-- money-out C-1 / C-2 / M-1 / M-4, onchain-ops C-1, install-roles M3 / M4.
-- Remediation contract docs/plans/2026-09-03-wave5-contract.md §0 R3-1.
--
-- ####  One sentence  ####
--
-- Every guard and every forensic trigger in this schema fires on UPDATE or
-- DELETE. Nothing fires on INSERT. So for every configuration and state
-- table, `ledger_app` -- the credential the whole threat model assumes is
-- leaked -- can APPEND a row of legal shape, and the append is neither
-- refused nor recorded anywhere.
--
-- Counted on a clean install of 001-028, from pg_trigger:
--
--     BEFORE UPDATE (row)           24
--     BEFORE DELETE (row)           13
--     AFTER  UPDATE (audit)          6 trigger definitions, 11 tables
--     BEFORE/AFTER INSERT            1   trg_check_journal_currency_balance
--
-- That one INSERT trigger is the per-currency debit=credit check. It is the
-- only thing in this schema that has ever looked at a row being inserted.
--
-- Migration 003's header describes the attack class exactly -- "it does not
-- forge anything, it makes the application sign a correct journal about the
-- wrong facts" -- and then closes only the UPDATE half, because the example
-- it was written from was an UPDATE. 006 extended the same half to seven
-- more tables. 020 derived the audit population from "carries a BEFORE
-- UPDATE row trigger", a predicate that cannot see an INSERT. Three rounds
-- of guard work, all of it about rewriting a row that already exists.
--
-- ####  What appending a row buys, measured  ####
--
-- Four independent paths, all as a real `ledger_app` over a socket:
--
--   1. entry_template_lines (money-out C-1). Append two lines to the
--      installed `deposit_confirm` template reusing the same amount_key.
--      EntryTemplate.Render re-reads the lines on every call, so the next
--      honest 100 deposit renders 200, and PostJournal signs it. The gated
--      withdrawal base (I-49's V) accepts it because the signature is real.
--      RunFullReconciliation: OverallPassed=true. verify: VERIFIED.
--      SolvencyCheck: solvent=true, margin unchanged (both sides grew).
--      config_table_changes: zero rows.
--
--   2. bookings (money-out C-2). Append one row at status='confirming'
--      with metadata.block_number=1. recheckOneDeposit computes
--      confirmations = latest - 1 + 1, which clears any threshold, so the
--      TxIncluded existence check (which only runs BELOW the threshold) is
--      never reached; advanceConfirmation posts a signed deposit_confirm
--      for an amount the attacker chose, for a transfer that never
--      happened.
--
--   3. chain_cursors (onchain-ops C-1). One UPDATE, or one INSERT for a
--      chain not yet scanned, moves last_scanned_block forward. The
--      forward scan never looks back (`from` = cursor+1), so every real
--      deposit in the skipped window is invisible to the ledger forever --
--      no booking, no event, no journal, no entry, and therefore nothing
--      for any of the 16 reconciliation checks to notice. The money still
--      arrives at the CREATE2 address and is still swept into treasury:
--      an asset with no matching liability, and solvency reads HEALTHIER
--      for it.
--
--   4. ledger_attestations (money-out M-4, install-roles M4). One row at
--      any seq. At a high seq it raises the chain head that migration
--      024's anchor-observation ceiling is measured against, re-opening
--      the weld 024 closed. At the true next seq it poisons the hash
--      chain, and both UPDATE and DELETE are refused to every role, so
--      verify reports TAMPERED forever.
--
-- ####  What this migration does, and what it deliberately does not  ####
--
-- Two layers, applied per table according to what is actually knowable at
-- INSERT time:
--
--   * PREVENTION where an invariant exists that the honest writer satisfies
--     structurally and an appended row cannot: a template's lines may only
--     be written by the transaction that created the template; a booking
--     may only be born at its lifecycle's initial status, unsettled and
--     unlinked; a reservation may only be born active and unsettled; an
--     attestation may only extend the chain by one, linked to the previous
--     root; a cursor may only move forward, and not by more than one
--     window.
--
--   * RECORDING everywhere else, because for a config row the honest INSERT
--     and the forged INSERT are the same statement. account_policies is the
--     canonical case: I-17 already ruled that the freeze/overdraft knobs are
--     business rules and not tamper-proof controls, so prevention is out --
--     but I-58's compensating promise ("a change a guard lets through is
--     recorded") was written in UPDATE semantics, and the INSERT variant of
--     the same attack left config_table_changes at zero rows.
--
-- What it does NOT do, stated so the next reader does not assume otherwise:
-- **this migration does not close money-out C-2.** Verified against the
-- source rather than the finding's summary: bookings.metadata legitimately
-- carries block_number at INSERT (service/onchain.go's CreateBooking call
-- persists it on purpose, because the recheck loop recomputes confirmations
-- from it), and recheckPendingDeposits scans 'pending' as well as
-- 'confirming'. So an attacker who appends a booking at the lifecycle's
-- initial status with a low block_number reaches the same auto-credit. The
-- guard below removes the shorter path (being born already confirming, or
-- already linked to a journal, or already part-settled) and the audit
-- trigger makes the append visible, which is worth having on its own. The
-- prevention C-2 needs is an application-layer fence -- requiring a second
-- data source before auto-credit, or calling TxIncluded unconditionally --
-- and lives in service/onchain.go, not here.
--
-- ####  Write amplification  ####
--
-- Same accounting 020 did. An audited INSERT costs one config_table_changes
-- row carrying a full jsonb copy of the inserted row. For the config tables
-- that is nothing. bookings, events and reservations are business-rate, and
-- 020 already accepted business-rate auditing for their UPDATEs on the
-- grounds that "that is the rate at which money moves"; one row per creation
-- is strictly less than what their transitions already write.
--
-- journals is the exception, and the only one. Its UPDATE audit is rare (the
-- event_id set-once backfill), but its INSERT is the ledger's highest-rate
-- write, and unlike every other table here a journal AUTHENTICATES ITSELF:
-- auth_digest/auth_signature/auth_key_id are covered by I-26, an appended
-- journal is exactly what VerifyJournalAuth refuses, and I-49's gated
-- withdrawal base is UNDEFINED for any dimension one touches. A second full
-- copy of every journal would double the money path's write volume to
-- re-detect what the signature already detects. Registered as a named
-- exception in postgres/audit_trail_guard_test.go rather than omitted.

-- Every function created below carries an explicit REVOKE ALL ... FROM
-- PUBLIC. 021 swept the functions that existed when it ran, but a newly
-- created function is EXECUTE-able by PUBLIC by default, so the sweep is not
-- inherited -- postgres/function_acl_test.go is the gate that says so, and it
-- went red on the first run of this migration.
--
-- Every function also carries `SET search_path = public, pg_temp`, that exact
-- value. Not decoration and not style: an unpinned search_path searches
-- pg_temp FIRST for relation names, and ledger_app holds the default
-- TEMPORARY privilege, so `CREATE TEMP TABLE journal_entries` shadows the
-- real one for any guard that reads it (install-roles C1). Naming pg_temp
-- explicitly at the END is what puts it after public. pg_catalog is left off
-- deliberately -- Postgres searches it first regardless when it is not
-- listed, so pg_has_role/current_setting/octet_length resolve either way, and
-- migration 030's catalogue gate requires this exact string of every trigger
-- and SECURITY DEFINER function rather than a set of near-equivalents nobody
-- can compare mechanically. Every body below also schema-qualifies the
-- relations it reads, which is belt to that brace.

------------------------------------------------------------
-- 1. The forensic trail learns about INSERT.
------------------------------------------------------------

-- COALESCE is what makes one function serve both events: in an INSERT
-- trigger PL/pgSQL leaves OLD unassigned and to_jsonb(OLD) evaluates to SQL
-- NULL, while config_table_changes.old_row is NOT NULL. 'null'::jsonb is the
-- JSON value "there was no row", which is what a reader of this trail needs
-- to distinguish a creation from a change -- and it is distinguishable from
-- '{}' (a row with no columns, which cannot occur).
CREATE OR REPLACE FUNCTION ledger_log_config_table_change() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (TG_TABLE_NAME, COALESCE(to_jsonb(OLD), 'null'::jsonb), to_jsonb(NEW), session_user);
    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_log_config_table_change() OWNER TO ledger_owner;

-- The reconcile-cursor trail has a shaped table rather than before/after
-- jsonb, so its INSERT branch has to name the "before" explicitly. The
-- honest answer is the column defaults 001 chose: a check that has never
-- scanned has after_holder/after_currency at BIGINT'-9223372036854775808'
-- ("before every possible dimension key") and lap_dirty false. Recording
-- those is not a placeholder, it is the state the row replaced.
CREATE OR REPLACE FUNCTION ledger_log_reconcile_scan_cursor_change() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO reconcile_scan_cursor_changes (
            check_name, old_after_holder, old_after_currency, old_lap_dirty,
            new_after_holder, new_after_currency, new_lap_dirty, changed_by
        ) VALUES (
            NEW.check_name, -9223372036854775808, -9223372036854775808, false,
            NEW.after_holder, NEW.after_currency, NEW.lap_dirty, session_user
        );
        RETURN NEW;
    END IF;

    INSERT INTO reconcile_scan_cursor_changes (
        check_name, old_after_holder, old_after_currency, old_lap_dirty,
        new_after_holder, new_after_currency, new_lap_dirty, changed_by
    ) VALUES (
        NEW.check_name, OLD.after_holder, OLD.after_currency, OLD.lap_dirty,
        NEW.after_holder, NEW.after_currency, NEW.lap_dirty, session_user
    );
    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_log_reconcile_scan_cursor_change() OWNER TO ledger_owner;

CREATE TRIGGER reconcile_scan_cursors_audit_insert
    AFTER INSERT ON public.reconcile_scan_cursors
    FOR EACH ROW EXECUTE FUNCTION ledger_log_reconcile_scan_cursor_change();

-- Derived, not listed -- 020's loop run for the other event. Every table
-- that already carries the config-change audit trigger gets its INSERT
-- counterpart, except journals (see the header). A WHEN clause is not
-- possible here and not needed: PostgreSQL refuses `WHEN` referencing OLD on
-- an INSERT trigger, and the churn carve-out 020 needed for events' delivery
-- bookkeeping only concerns UPDATEs.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT DISTINCT c.relname AS table_name
        FROM pg_trigger t
        JOIN pg_class c ON c.oid = t.tgrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        JOIN pg_proc p ON p.oid = t.tgfoid
        WHERE n.nspname = 'public'
          AND NOT t.tgisinternal
          AND p.proname = 'ledger_log_config_table_change'
          AND c.relname <> 'journals'
          AND NOT EXISTS (
              SELECT 1
              FROM pg_trigger t2
              JOIN pg_proc p2 ON p2.oid = t2.tgfoid
              WHERE t2.tgrelid = c.oid
                AND NOT t2.tgisinternal
                AND p2.proname = 'ledger_log_config_table_change'
                -- INSERT (bit 4).
                AND (t2.tgtype & 4) <> 0
          )
        ORDER BY 1
    LOOP
        EXECUTE format(
            'CREATE TRIGGER %I AFTER INSERT ON public.%I FOR EACH ROW EXECUTE FUNCTION ledger_log_config_table_change()',
            r.table_name || '_audit_insert', r.table_name);
    END LOOP;
END $$;

------------------------------------------------------------
-- 2. entry_template_lines: a template's lines are written once, by the
--    transaction that created it (money-out C-1).
------------------------------------------------------------

-- The property that separates the honest writer from an appended line is not
-- the content of the line -- an attacker can copy an existing one verbatim
-- -- it is WHEN it is written. postgres.TemplateStore.CreateTemplate inserts
-- the template row and every one of its lines in a single transaction, in
-- both of its modes (pool mode opens the transaction itself; tx mode writes
-- into the caller's). There is no other writer: entry_template_lines has no
-- upsert, no deactivation, no repair path, and 003 already took UPDATE away
-- because it has no legitimate mutation either.
--
-- So: a line may only be inserted while its template row is still being
-- created. `now()` is transaction_timestamp() -- identical for every
-- statement of a transaction including its subtransactions, and different
-- for any other -- and entry_templates.created_at takes it from the column
-- DEFAULT. The comparison is therefore exactly "was this template created by
-- the transaction I am in".
--
-- Considered and rejected: a UNIQUE (template_id, classification_id,
-- entry_type, holder_role, amount_key) index. It blocks the measured attack
-- (which duplicated two existing lines) and nothing else -- an attacker who
-- points the extra credit line at a different classification walks through
-- it. Also rejected: "amount_key must be unique within a template", which is
-- not true of the shipped presets (deposit_confirm's two lines both key on
-- 'amount').
--
-- The cost of being wrong here is loud, not silent: a writer this refuses
-- gets an exception at install time, not a quietly missing line.
-- The one sanctioned way past the rule, in 027's two-layer shape (a
-- transaction-local flag AND owner membership; neither alone is worth
-- anything). It exists because migration 016 is the precedent: a preset's
-- lines were shipped with the wrong polarity, and correcting an
-- already-installed deployment meant deleting a template's lines and writing
-- new ones from inside a migration. That is a real, recurring operator need,
-- and a guard with no door for it is a guard the next author will drop
-- instead of using.
--
-- 016 itself needs no flag on any real deployment: migrations are ordered, so
-- it always ran before this file existed. Only a test that re-applies it on
-- top of 029 sees the guard.
CREATE FUNCTION ledger_template_line_repair_is_authorized() RETURNS boolean
LANGUAGE sql STABLE
SET search_path = public, pg_temp
AS $$
    SELECT COALESCE(current_setting('ledger.repair_template_lines', true), 'off') = 'on'
       AND pg_has_role(current_user, 'ledger_owner', 'USAGE');
$$;

ALTER FUNCTION ledger_template_line_repair_is_authorized() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_template_line_repair_is_authorized() FROM PUBLIC;

CREATE FUNCTION ledger_entry_template_lines_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    same_tx BOOLEAN;
BEGIN
    SELECT t.created_at = now() INTO same_tx
      FROM public.entry_templates t
     WHERE t.id = NEW.template_id;

    IF same_tx IS NULL THEN
        RAISE EXCEPTION 'ledger: entry_template_lines references template_id % which does not exist', NEW.template_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF same_tx THEN
        RETURN NEW;
    END IF;

    -- pg_has_role is checked BEFORE the door predicate on purpose: EXECUTE on
    -- that predicate is revoked from PUBLIC, so consulting it first would
    -- hand ledger_app a bare 42501 instead of the sentence that explains what
    -- it just tried to do.
    IF NOT pg_has_role(current_user, 'ledger_owner', 'USAGE') THEN
        RAISE EXCEPTION 'ledger: template % already exists, and a template''s lines may only be written by the transaction that created it -- appending a line to an installed template silently multiplies every journal that template renders (see migration 029)', NEW.template_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF NOT ledger_template_line_repair_is_authorized() THEN
        RAISE EXCEPTION 'ledger: template % already exists, and a template''s lines may only be written by the transaction that created it; an owner-run repair (migration 016''s shape) must say so explicitly with set_config(''ledger.repair_template_lines'', ''on'', true) in the same transaction', NEW.template_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_entry_template_lines_insert_guard() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_entry_template_lines_insert_guard() FROM PUBLIC;

CREATE TRIGGER entry_template_lines_insert_guard
    BEFORE INSERT ON public.entry_template_lines
    FOR EACH ROW EXECUTE FUNCTION ledger_entry_template_lines_insert_guard();

------------------------------------------------------------
-- 3. bookings and reservations are born at the start of their life.
------------------------------------------------------------

-- What CreateBooking actually writes (postgres/booking_store.go): status =
-- the classification lifecycle's `initial`, and nothing else -- journal_id,
-- reservation_id and settled_amount are left to their defaults, because a
-- booking that has already settled or is already linked to a journal is not
-- a booking being created, it is a booking being finished. The UPDATE guard
-- (006/027) already enforces the second half of that story: settled_amount
-- must not decrease and journal_id is set-once. This is the missing first
-- half.
--
-- A classification with no lifecycle is passed through rather than refused.
-- CreateBooking refuses it in the application ("classification %q has no
-- lifecycle"), so such a row is inert: it cannot be transitioned, and the
-- deposit recheck loop filters by the deposit classification, which does
-- have one. Refusing it here instead would only break fixtures that use a
-- label-only classification as a convenient dimension.
CREATE FUNCTION ledger_bookings_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    initial_status TEXT;
BEGIN
    SELECT c.lifecycle->>'initial' INTO initial_status
      FROM public.classifications c
     WHERE c.id = NEW.classification_id;

    IF initial_status IS NOT NULL AND NEW.status IS DISTINCT FROM initial_status THEN
        RAISE EXCEPTION 'ledger: a booking on classification % must be created at its lifecycle initial status %, not %',
            NEW.classification_id, initial_status, NEW.status
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.journal_id IS NOT NULL THEN
        RAISE EXCEPTION 'ledger: bookings.journal_id is set by the transition that posts the accounting, never at creation'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.settled_amount <> 0 THEN
        RAISE EXCEPTION 'ledger: a booking is created unsettled; settled_amount was %', NEW.settled_amount
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_bookings_insert_guard() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_bookings_insert_guard() FROM PUBLIC;

CREATE TRIGGER bookings_insert_guard
    BEFORE INSERT ON public.bookings
    FOR EACH ROW EXECUTE FUNCTION ledger_bookings_insert_guard();

-- InsertReservation names six columns and leaves status and settled_amount
-- to their defaults ('active', 0). A reservation appended already 'settled'
-- or already part-settled is a receipt for a settlement that never ran: the
-- I-49 conservative hold reads reserved_amount and expires_at, but
-- SumActiveReservations (the ungated path) reads status and settled_amount,
-- so a born-settled row is a hold that never existed.
CREATE FUNCTION ledger_reservations_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.status <> 'active' THEN
        RAISE EXCEPTION 'ledger: a reservation is created active, not %', NEW.status
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.settled_amount <> 0 THEN
        RAISE EXCEPTION 'ledger: a reservation is created unsettled; settled_amount was %', NEW.settled_amount
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.journal_id IS NOT NULL THEN
        RAISE EXCEPTION 'ledger: reservations.journal_id is set by the settlement that posts the accounting, never at creation'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_reservations_insert_guard() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_reservations_insert_guard() FROM PUBLIC;

CREATE TRIGGER reservations_insert_guard
    BEFORE INSERT ON public.reservations
    FOR EACH ROW EXECUTE FUNCTION ledger_reservations_insert_guard();

------------------------------------------------------------
-- 4. chain_cursors: the one value that decides which on-chain money the
--    ledger can ever see (onchain-ops C-1).
------------------------------------------------------------

-- Until now chain_cursors carried zero triggers and ledger_app held a plain
-- table UPDATE, while postgres/audit_trail_guard_test.go excluded it from
-- the audit requirement on three grounds, each of which is answered here:
--
--   "monotonic-protected on write" -- in SetChainCursor's WHERE clause,
--   i.e. in the application, i.e. in the layer this threat model assumes is
--   bypassed. The exclusion list is about DB-layer protection, so this was
--   circular. Now the monotonicity is a trigger.
--
--   "a gap that idempotency keys absorb" -- idempotency keys absorb
--   REPEATS. A gap is not a repeat; scanChainOnce's own I-52 comment three
--   files away says the forward scan never looks back, so a skipped window
--   is permanent.
--
--   "it cannot move money" -- it decides which money the ledger is ever
--   told about. A deposit in a skipped window leaves no row anywhere, so no
--   reconciliation check can see it, while the funds are still swept into
--   treasury: an asset with no liability, and solvency looks better for it.
--
-- Three things, then: monotonicity in the database, a bound on how far one
-- write may jump, and a forensic row for every write including the first.
-- The bound is deliberately crude -- the database has no idea what the chain
-- head is -- but it converts "one statement makes every deposit from here to
-- eternity invisible" into "one statement skips at most one oversized
-- window, and leaves a config_table_changes row saying so".
--
-- Amended by 032: that conclusion held for the UPDATE branch only. The
-- INSERT branch -- a chain with no cursor row yet -- was unbounded, and
-- ledger_app could create a cursor at any block (88,888,888 measured).
-- 032 bounds it with the same cap, plus an owner-only seeding door for a
-- chain that really does start high (I-67 rule 2).

-- Same two-layer shape as 027: a transaction-local flag AND owner
-- membership. Either alone is worthless -- set_config is available to every
-- role, and a bare role check would widen the rule for every owner-issued
-- statement including a mistaken one.
--
-- COALESCE is not decoration. current_setting(..., true) returns NULL when
-- the GUC was never set, so the un-COALESCEd expression 027 uses evaluates
-- to NULL rather than false, and `IF NOT <null> THEN RAISE` does not raise:
-- for a caller that already satisfies the role half, the guard would fail
-- OPEN exactly when nobody opened the door. Measured here on the first run
-- of this migration's own pin (the owner's raw backwards UPDATE succeeded).
-- 027's two guards are shielded from the same shape by the `NEW.journal_id
-- IS NULL AND ...` conjunct in front of the call, which is false in the
-- attack case; reported separately rather than edited here.
CREATE FUNCTION ledger_chain_cursor_rewind_is_authorized() RETURNS boolean
LANGUAGE sql STABLE
SET search_path = public, pg_temp
AS $$
    SELECT COALESCE(current_setting('ledger.rewind_chain_cursor', true), 'off') = 'on'
       AND pg_has_role(current_user, 'ledger_owner', 'USAGE');
$$;

ALTER FUNCTION ledger_chain_cursor_rewind_is_authorized() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_chain_cursor_rewind_is_authorized() FROM PUBLIC;

CREATE FUNCTION ledger_chain_cursors_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['last_scanned_block', 'updated_at'];
    -- How far one write may move the cursor. service.Onchain's forward scan
    -- writes at most maxBlocksPerScan (default 2000) per SetCursor, so this
    -- is two orders of magnitude of headroom and still refuses the jump the
    -- review measured.
    --
    -- Configurable the only way that is not self-reported: a deployment that
    -- really does scan in larger windows replaces this function body, which
    -- requires ownership. Deliberately NOT a GUC (ledger_app can SET one in
    -- its own session -- the same shape as the application_name hole
    -- install-roles M2 found in assertSoleSessionOnCredential), and
    -- deliberately not a column on chain_cursors (writable by whoever writes
    -- the row this bounds), and deliberately not a helper function (this
    -- guard runs with invoker rights, so ledger_app would need EXECUTE on it
    -- and would get a bare 42501 instead of the rule -- measured).
    cap     CONSTANT bigint := 100000;
BEGIN
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on chain_cursors may only change %, and this statement changed something else',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;

    -- The rewind door below is the only way backwards, and it is owner-only.
    -- pg_has_role is checked first for the same reason as the template guard:
    -- EXECUTE on the door predicate is revoked from PUBLIC, so asking it
    -- first would answer ledger_app with 42501 instead of the rule.
    IF NEW.last_scanned_block < OLD.last_scanned_block THEN
        -- Two nested IFs, not one OR: PostgreSQL does not promise to
        -- short-circuit boolean operators, and the second call is to a
        -- function whose EXECUTE ledger_app does not hold.
        IF NOT pg_has_role(current_user, 'ledger_owner', 'USAGE') THEN
            RAISE EXCEPTION 'ledger: chain_cursors.last_scanned_block only moves forward (% -> %); a deliberate rewind goes through ledger_rewind_chain_cursor(), which is owner-only and leaves a forensic row',
                OLD.last_scanned_block, NEW.last_scanned_block
                USING ERRCODE = 'check_violation';
        END IF;
        IF NOT ledger_chain_cursor_rewind_is_authorized() THEN
            RAISE EXCEPTION 'ledger: chain_cursors.last_scanned_block only moves forward (% -> %); an owner-run rewind goes through ledger_rewind_chain_cursor(), which sets the transaction-local flag this guard requires and writes the forensic row',
                OLD.last_scanned_block, NEW.last_scanned_block
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.last_scanned_block - OLD.last_scanned_block > cap THEN
        RAISE EXCEPTION 'ledger: chain_cursors.last_scanned_block advanced % blocks in one write, more than the % this deployment scans in a window -- every real deposit between % and % would never be seen by any code path (I-52, I-67)',
            NEW.last_scanned_block - OLD.last_scanned_block, cap, OLD.last_scanned_block + 1, NEW.last_scanned_block
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_chain_cursors_guard() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_chain_cursors_guard() FROM PUBLIC;

CREATE TRIGGER chain_cursors_mutation_guard
    BEFORE UPDATE ON public.chain_cursors
    FOR EACH ROW EXECUTE FUNCTION ledger_chain_cursors_guard();

CREATE TRIGGER chain_cursors_audit
    AFTER UPDATE ON public.chain_cursors
    FOR EACH ROW
    WHEN (OLD.last_scanned_block IS DISTINCT FROM NEW.last_scanned_block)
    EXECUTE FUNCTION ledger_log_config_table_change();

CREATE TRIGGER chain_cursors_audit_insert
    AFTER INSERT ON public.chain_cursors
    FOR EACH ROW EXECUTE FUNCTION ledger_log_config_table_change();

-- The recovery path onchain-ops C-1 found missing. SetChainCursor is
-- monotonic by design (a lagging replica must not drag the cursor back), so
-- after a forward jump -- whether an attack or a bad manual UPDATE -- the
-- application has no API that can re-scan the skipped window. The operator's
-- only option was the untriggered, unaudited raw UPDATE that caused the
-- problem. This is that action, named, owner-only, and recorded with the
-- reason the operator gives.
CREATE FUNCTION ledger_rewind_chain_cursor(p_chain_id bigint, p_to_block bigint, p_reason text) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_from BIGINT;
BEGIN
    IF p_reason IS NULL OR btrim(p_reason) = '' THEN
        RAISE EXCEPTION 'ledger: rewinding a chain cursor requires a reason -- it is the only thing that distinguishes this from the attack it repairs'
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT last_scanned_block INTO v_from FROM chain_cursors WHERE chain_id = p_chain_id FOR UPDATE;

    IF v_from IS NULL THEN
        RAISE EXCEPTION 'ledger: no chain cursor for chain %', p_chain_id
            USING ERRCODE = 'no_data_found';
    END IF;

    IF p_to_block >= v_from THEN
        -- Never silent (working-agreements.md §3): a repair that reports
        -- success having done nothing is the failure mode this repo keeps
        -- finding.
        RAISE EXCEPTION 'ledger: chain % cursor is already at %, which is not ahead of the requested %; use SetChainCursor to move forward',
            p_chain_id, v_from, p_to_block
            USING ERRCODE = 'check_violation';
    END IF;

    IF p_to_block < 0 THEN
        RAISE EXCEPTION 'ledger: chain cursor target block must not be negative, got %', p_to_block
            USING ERRCODE = 'check_violation';
    END IF;

    PERFORM set_config('ledger.rewind_chain_cursor', 'on', true);
    UPDATE chain_cursors SET last_scanned_block = p_to_block, updated_at = now() WHERE chain_id = p_chain_id;
    PERFORM set_config('ledger.rewind_chain_cursor', 'off', true);

    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (
        'ledger_rewind_chain_cursor',
        jsonb_build_object('chain_id', p_chain_id, 'last_scanned_block', v_from),
        jsonb_build_object('chain_id', p_chain_id, 'last_scanned_block', p_to_block, 'reason', p_reason),
        session_user
    );
END;
$$;

ALTER FUNCTION ledger_rewind_chain_cursor(bigint, bigint, text) OWNER TO ledger_owner;

-- No GRANT to ledger_app, for the reason 027 gives: a repair capability in
-- the hands of the credential the threat model assumes is leaked is not a
-- repair capability, it is the attack with a nicer name.
REVOKE ALL ON FUNCTION ledger_rewind_chain_cursor(bigint, bigint, text) FROM PUBLIC;

------------------------------------------------------------
-- 5. ledger_attestations: an INSERT may only extend the chain by one
--    (money-out M-4, install-roles M4).
------------------------------------------------------------

-- I-27 already states the property -- "seq is unique and contiguous starting
-- at 1; each row's prev_root equals the previous row's root_hash; seq 1's
-- prev_root is core.GenesisRoot, 32 zero bytes" -- and lists
-- AttestationService.RunAttestBatch and VerifyLedger as its enforcers. Both
-- are application code. In this threat model the application's credential is
-- the attacker, and it held a plain INSERT on this table with nothing but a
-- UNIQUE(seq) in the way.
--
-- Moving the structural half of I-27 into the database costs one indexed
-- lookup per attestation (one row per batch, not per entry) and closes the
-- half of the finding that has a local answer:
--
--   * install-roles M4 -- an appended seq=888888 raised the chain head that
--     024's anchor-observation ceiling measures against, so the weld 024
--     closed re-opened one INSERT later. MAX(seq) is a trustworthy ceiling
--     again only if seq can move by exactly one.
--
--   * money-out M-4 -- an appended row at seq 1 of an empty chain welded
--     verify to TAMPERED permanently. prev_root linkage does not stop a
--     determined attacker (GenesisRoot is a public constant and root_hash
--     can be any 32 bytes), so this is NOT a full closure; what closes it is
--     the recovery path below, which the finding asked for by name.
--
-- The length checks are shape, not cryptography: every one of these values
-- is a SHA-256 output in core/attestation.go, and the measured attacks all
-- used empty byteas. merkle_root is deliberately NOT length-checked -- it is
-- '' for the pre-P7 attestation shape, which service/attest_verify_merkle
-- still exercises.
CREATE FUNCTION ledger_attestations_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    head_seq  BIGINT;
    head_root BYTEA;
BEGIN
    SELECT seq, root_hash INTO head_seq, head_root
      FROM public.ledger_attestations ORDER BY seq DESC LIMIT 1;

    head_seq := COALESCE(head_seq, 0);
    -- core.GenesisRoot: GenesisRootHashLen zero bytes.
    head_root := COALESCE(head_root, decode(repeat('00', 32), 'hex'));

    IF NEW.seq <> head_seq + 1 THEN
        RAISE EXCEPTION 'ledger: attestation seq must extend the chain by one (chain head is %, got %) -- a chain whose head can be chosen is not a ceiling anything else can be measured against (I-27, I-66)',
            head_seq, NEW.seq
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.prev_root IS DISTINCT FROM head_root THEN
        RAISE EXCEPTION 'ledger: attestation prev_root must equal the previous attestation''s root_hash (seq 1 links to core.GenesisRoot)'
            USING ERRCODE = 'check_violation';
    END IF;

    IF octet_length(NEW.prev_root) <> 32 OR octet_length(NEW.root_hash) <> 32 OR octet_length(NEW.batch_digest) <> 32 THEN
        RAISE EXCEPTION 'ledger: attestation prev_root, root_hash and batch_digest are SHA-256 outputs and must be 32 bytes'
            USING ERRCODE = 'check_violation';
    END IF;

    IF octet_length(NEW.signature) = 0 OR NEW.key_id = '' THEN
        RAISE EXCEPTION 'ledger: an attestation with no signature or no key_id testifies to nothing'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.entry_count < 0 THEN
        RAISE EXCEPTION 'ledger: attestation entry_count must not be negative, got %', NEW.entry_count
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_attestations_insert_guard() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_attestations_insert_guard() FROM PUBLIC;

CREATE TRIGGER ledger_attestations_insert_guard
    BEFORE INSERT ON public.ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_attestations_insert_guard();

-- ####  The way back  ####
--
-- money-out M-4's third remedy, verbatim: "provide an owner-only, audited
-- 'quarantine the poison row' procedure, otherwise this is irreversible".
--
-- Both no_update and no_delete on this table run ledger_block_mutation(),
-- which refuses every role including ledger_owner, so before this there was
-- no path back short of dropping the trigger -- DDL, under incident
-- pressure, on the table whose immutability is the point. The DELETE guard
-- for this one table is replaced with a narrower one carrying a single named
-- exception, the same shape 027 gave the two set-once guards. The UPDATE
-- guard is untouched and still blanket: a poisoned attestation may be
-- discarded, never edited, so the chain that remains is always a prefix of
-- one that was actually written.
--
-- Only a SUFFIX may go, and only in one call, so this cannot be used to
-- open a hole in the middle of the chain -- which I-27's gaplessness makes
-- detectable anyway, but a repair tool should not be able to create the
-- thing it repairs.
CREATE FUNCTION ledger_attestation_discard_is_authorized() RETURNS boolean
LANGUAGE sql STABLE
SET search_path = public, pg_temp
AS $$
    SELECT COALESCE(current_setting('ledger.discard_attestations', true), 'off') = 'on'
       AND pg_has_role(current_user, 'ledger_owner', 'USAGE');
$$;

ALTER FUNCTION ledger_attestation_discard_is_authorized() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_attestation_discard_is_authorized() FROM PUBLIC;

CREATE FUNCTION ledger_attestation_chain_block_delete() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
    IF ledger_attestation_discard_is_authorized() THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'ledger: DELETE on % is not allowed', TG_TABLE_NAME
        USING ERRCODE = 'check_violation';
END;
$$;

ALTER FUNCTION ledger_attestation_chain_block_delete() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_attestation_chain_block_delete() FROM PUBLIC;

DROP TRIGGER ledger_attestations_no_delete ON public.ledger_attestations;
CREATE TRIGGER ledger_attestations_no_delete
    BEFORE DELETE ON public.ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_attestation_chain_block_delete();

DROP TRIGGER entry_attestations_no_delete ON public.entry_attestations;
CREATE TRIGGER entry_attestations_no_delete
    BEFORE DELETE ON public.entry_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_attestation_chain_block_delete();

CREATE FUNCTION ledger_discard_attestations_from(p_seq bigint, p_reason text) RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_head    BIGINT;
    v_removed BIGINT;
BEGIN
    IF p_reason IS NULL OR btrim(p_reason) = '' THEN
        RAISE EXCEPTION 'ledger: discarding attestations requires a reason -- this is the one operation that shortens the chain, and the reason is the whole forensic record of why'
            USING ERRCODE = 'check_violation';
    END IF;

    IF p_seq < 1 THEN
        RAISE EXCEPTION 'ledger: attestation seq starts at 1, got %', p_seq
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT COALESCE(MAX(seq), 0) INTO v_head FROM ledger_attestations;

    IF v_head < p_seq THEN
        RAISE EXCEPTION 'ledger: the attestation chain only reaches seq %, so there is nothing at or after % to discard', v_head, p_seq
            USING ERRCODE = 'no_data_found';
    END IF;

    PERFORM set_config('ledger.discard_attestations', 'on', true);
    -- Coverage first: entry_attestations.seq is a FK onto the rows being
    -- removed, and the entries it covered must go back to being uncovered so
    -- the next batch re-attests them (that is what makes the repair
    -- complete rather than a hole VerifyLedger step 3b would report).
    DELETE FROM entry_attestations WHERE seq >= p_seq;
    DELETE FROM ledger_attestations WHERE seq >= p_seq;
    GET DIAGNOSTICS v_removed = ROW_COUNT;
    PERFORM set_config('ledger.discard_attestations', 'off', true);

    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (
        'ledger_discard_attestations_from',
        jsonb_build_object('head_seq', v_head, 'from_seq', p_seq),
        jsonb_build_object('head_seq', p_seq - 1, 'discarded', v_removed, 'reason', p_reason),
        session_user
    );

    RETURN v_removed;
END;
$$;

ALTER FUNCTION ledger_discard_attestations_from(bigint, text) OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_discard_attestations_from(bigint, text) FROM PUBLIC;

------------------------------------------------------------
-- 6. install-roles M3: the partition rebalance builds only months that
--    exist.
------------------------------------------------------------

-- 021 capped the caller-supplied range at 120 months, then widened it to
-- cover whatever is actually in the default partition -- "must stay
-- uncapped", correctly, or an expired horizon would be unrecoverable. But
-- the widening built EVERY month in the widened span, and
-- journal_entries.created_at carries no upper bound and is one of the eight
-- columns ledger_app may write. Measured: one pair of balanced entries dated
-- 2050 turned a compliant call into 286 partitions / 1716 relations, each
-- taking ACCESS EXCLUSIVE on journal_entries, none of them droppable by the
-- credential that caused it. 4000 would be ~24,000.
--
-- The fix keeps the widening uncapped and removes the amplification: the
-- caller's own [p_first, p_last] stays dense (pre-creating empty future
-- months is the point of calling it), and the widening becomes sparse --
-- a partition for a month outside that range only if a row in the default
-- partition actually falls in it. The row-move that follows is unaffected,
-- because every month it needs a partition for is by definition a month
-- present in journal_entries_default. DDL volume becomes a function of the
-- data's month cardinality instead of a span the writer chose: one forged
-- far-future row now costs one partition, not 286.
--
-- Everything else about 021's body is carried over verbatim (argument
-- validation, DETACH/ATTACH, the row move, the `created` return shape).
CREATE OR REPLACE FUNCTION ledger_rebalance_default_partition(
    p_first date, p_last date
) RETURNS text[]
LANGUAGE plpgsql
SECURITY DEFINER
-- Restated for the same reason 021 restated it: CREATE OR REPLACE rewrites
-- proconfig wholesale, so omitting this would silently undo migration 013.
SET search_path = public, pg_temp
AS $$
DECLARE
    created    text[] := '{}';
    v_has_rows boolean;
    v_month    date;
    v_name     text;
    max_months CONSTANT integer := 120;
BEGIN
    IF date_trunc('month', p_first)::date <> p_first OR date_trunc('month', p_last)::date <> p_last THEN
        RAISE EXCEPTION 'ledger: partition rebalance range must be month-aligned, got % .. %', p_first, p_last
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_last < p_first THEN
        RAISE EXCEPTION 'ledger: partition rebalance range ends before it starts: % .. %', p_first, p_last
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF (EXTRACT(YEAR FROM age(p_last, p_first)) * 12 + EXTRACT(MONTH FROM age(p_last, p_first))) > max_months THEN
        RAISE EXCEPTION 'ledger: partition rebalance range % .. % spans more than % months', p_first, p_last, max_months
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    ALTER TABLE journal_entries DETACH PARTITION journal_entries_default;

    SELECT EXISTS (SELECT 1 FROM journal_entries_default) INTO v_has_rows;

    -- 1. The requested range, dense.
    v_month := p_first;
    WHILE v_month <= p_last LOOP
        v_name := format('journal_entries_y%sm%s', to_char(v_month, 'YYYY'), to_char(v_month, 'MM'));
        IF to_regclass('public.' || v_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
                v_name, v_month, (v_month + INTERVAL '1 month')::date
            );
        END IF;
        created := array_append(created, v_name);
        v_month := (v_month + INTERVAL '1 month')::date;
    END LOOP;

    -- 2. The months the data is actually in, sparse (install-roles M3).
    FOR v_month IN
        SELECT DISTINCT date_trunc('month', created_at)::date AS m
        FROM journal_entries_default
        WHERE date_trunc('month', created_at)::date < p_first
           OR date_trunc('month', created_at)::date > p_last
        ORDER BY 1
    LOOP
        v_name := format('journal_entries_y%sm%s', to_char(v_month, 'YYYY'), to_char(v_month, 'MM'));
        IF to_regclass('public.' || v_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
                v_name, v_month, (v_month + INTERVAL '1 month')::date
            );
        END IF;
        created := array_append(created, v_name);
    END LOOP;

    IF v_has_rows THEN
        INSERT INTO journal_entries SELECT * FROM journal_entries_default;
        TRUNCATE journal_entries_default;
    END IF;

    ALTER TABLE journal_entries ATTACH PARTITION journal_entries_default DEFAULT;

    RETURN created;
END;
$$;

ALTER FUNCTION ledger_rebalance_default_partition(date, date) OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_rebalance_default_partition(date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ledger_rebalance_default_partition(date, date) TO ledger_app;
