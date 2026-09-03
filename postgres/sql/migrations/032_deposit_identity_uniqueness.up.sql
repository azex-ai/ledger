-- One deposit booking per on-chain transfer log (2026-09-03 independent
-- review, re-check round: money-out N-1; docs/INVARIANTS.md I-71).
--
-- What N-1 showed. A real deposit of 50 is ingested and credited. An attacker
-- holding `ledger_app` then appends three more bookings describing THE SAME
-- on-chain log -- same chain_id, tx_hash, txlog_seq, token, block_number,
-- same amount, same holder -- differing only in `channel_name` (their own
-- choice) and with `channel_ref` left empty. The honest recheck job confirms
-- all three: I-69's corroboration re-reads the chain and finds a log carrying
-- exactly that tx hash, log position, token, amount and a recipient
-- registered to that holder, because the row is a faithful COPY of a real
-- one. Verified balance goes 50 -> 200. Solvency and full reconciliation stay
-- green throughout (both sides of the equation grow together). Configuring a
-- second confirmation source does not help either: it is asked "is this
-- transfer 50?", answers "yes", and is right.
--
-- Why nothing existing caught it:
--
--   * uq_bookings_idempotency is UNIQUE on a column the INSERT chooses.
--   * uq_bookings_channel_ref is UNIQUE (channel_name, channel_ref) WHERE
--     channel_ref <> '' -- its FIRST key column is also the INSERT's to
--     choose, and '' opts out of the index entirely. It exists because one
--     transaction can carry several Transfer logs (I-20), not to stop
--     duplicates; it blocked one variant of this attack by accident.
--   * migration 029's INSERT guard constrains `status`, `journal_id` and
--     `settled_amount` -- the shape of the row, which here is impeccable.
--   * `deposit-{chain}-{tx}-{seq}` is derived by IngestDeposit in Go. An
--     attacker who does not call IngestDeposit never meets it.
--
-- The fix is to key the constraint on the deposit's REAL identity, which is
-- already carried on the row: the (chain, transaction, log position) triple
-- the ledger itself uses to decide the booking is the same deposit. This is
-- I-66's argument applied to a different table -- a property of the only
-- honest writer there is: IngestDeposit writes at most one booking per log,
-- ever, so the index cannot reject anything the ledger legitimately does.
--
-- Scope. The predicate names our own metadata vocabulary rather than the
-- deposit classification, because an index expression cannot join to
-- classifications. All three keys must be present, so:
--
--   * deposit bookings (chain_id + tx_hash + txlog_seq) are covered;
--   * sweep bookings are NOT (their metadata is chain_id/token/nonce, no
--     tx_hash) -- deliberate, and recorded as a follow-up: a sweep booking
--     never posts a journal, so a duplicate cannot mint;
--   * a consumer whose own classification happens to carry all three of
--     these keys, with duplicates, would fail this migration. That
--     combination is this library's deposit vocabulary; if it fires, the
--     rows are almost certainly the very duplicates this index exists to
--     prevent. Find them with:
--
--       SELECT metadata->>'chain_id', metadata->>'tx_hash',
--              metadata->>'txlog_seq', count(*), array_agg(uid)
--       FROM bookings
--       WHERE metadata ? 'tx_hash' AND metadata ? 'txlog_seq'
--         AND metadata ? 'chain_id'
--       GROUP BY 1, 2, 3 HAVING count(*) > 1;
--
-- Plain CREATE UNIQUE INDEX, not CONCURRENTLY: golang-migrate runs each
-- migration in a transaction and CONCURRENTLY cannot run inside one (the
-- same note 015, 023 and 025 carry). Build it out-of-band first on a large
-- deployment if the lock window matters.
CREATE UNIQUE INDEX uq_bookings_deposit_identity
    ON bookings ((metadata->>'chain_id'), (metadata->>'tx_hash'), (metadata->>'txlog_seq'))
    WHERE metadata ? 'tx_hash' AND metadata ? 'txlog_seq' AND metadata ? 'chain_id';

-- Not included, deliberately: a companion guard requiring
-- channel_name = 'onchain' on deposit-classification bookings. It was
-- written and then removed, because it is false that the ingestion path is
-- the only honest writer of a deposit booking -- README's Tier 2 example
-- creates one directly through Booker.CreateBooking with a channel name of
-- the consumer's choosing, and so may any consumer driving their own deposit
-- flow. The index above needs no such assumption: it constrains only rows
-- that carry an on-chain identity, and for those, uniqueness is what every
-- writer wants.

------------------------------------------------------------
-- chain_cursors: the FIRST write is a write too (onchain-ops re-check,
-- 2026-09-03; I-67).
------------------------------------------------------------

-- The authorization predicate, same two-layer shape (transaction-local flag
-- AND owner membership) and the same COALESCE that keeps it from failing
-- open when the GUC was never set, as 029's rewind predicate.
CREATE FUNCTION ledger_chain_cursor_seed_is_authorized() RETURNS boolean
LANGUAGE sql STABLE
SET search_path = public, pg_temp
AS $$
    SELECT COALESCE(current_setting('ledger.seed_chain_cursor', true), 'off') = 'on'
       AND pg_has_role(current_user, 'ledger_owner', 'USAGE');
$$;

ALTER FUNCTION ledger_chain_cursor_seed_is_authorized() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_chain_cursor_seed_is_authorized() FROM PUBLIC;

-- 029 bounded how far one write may move a cursor, and its header concluded
-- that a single statement can therefore "skip at most one oversized window".
-- That sentence is true of the UPDATE branch, which is the only branch it
-- guarded. A chain with no cursor row yet -- a newly configured chain, or one
-- whose row an owner deleted -- takes the INSERT branch, where `ledger_app`
-- could write any starting block it liked: 88,888,888 measured, leaving only
-- an audit row behind. Every deposit below that block is then invisible to
-- every code path, permanently, because the forward scan never looks back
-- (I-52).
--
-- I-67's rule 2 said the cursor "only moves forward", and "advance" is
-- exactly the word that let this through: the first write does not advance
-- anything, it decides where advancing starts from.
--
-- Same cap as the UPDATE branch (100,000) and the same door shape as the
-- rewind: a start above the cap is a deliberate act, so it goes through an
-- owner-only function that demands a reason and records it. A consumer whose
-- chain genuinely starts at a high block (any established chain, if they do
-- not want to scan from genesis) seeds it once, as the operator, before
-- pointing the watcher at it -- see docs/RUNBOOK.md.
CREATE FUNCTION ledger_chain_cursors_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    -- Deliberately the same constant as ledger_chain_cursors_guard's, and
    -- deliberately duplicated rather than shared through a helper: this
    -- guard runs with invoker rights, so a helper would need EXECUTE granted
    -- to ledger_app, which turns the rule into a bare 42501 (029's own note).
    cap CONSTANT bigint := 100000;
BEGIN
    -- SetChainCursor is an upsert, and PostgreSQL fires BEFORE INSERT for
    -- the PROPOSED row before it discovers the conflict -- so an ordinary
    -- advance of an already-seeded high cursor arrives here looking exactly
    -- like a fresh high INSERT. Measured: without this branch, a chain
    -- seeded at 4,000,000 could never be advanced by the watcher again.
    -- Where a row already exists the statement becomes an UPDATE, which
    -- ledger_chain_cursors_guard (029) bounds; this guard is only about the
    -- write that CREATES a cursor.
    IF EXISTS (SELECT 1 FROM public.chain_cursors WHERE chain_id = NEW.chain_id) THEN
        RETURN NEW;
    END IF;

    IF NEW.last_scanned_block <= cap THEN
        RETURN NEW;
    END IF;

    -- Two nested IFs and role-before-flag, same reasoning as 029's rewind
    -- branch: EXECUTE on the door predicate is revoked from PUBLIC, so
    -- asking it first answers ledger_app with 42501 instead of the rule.
    IF NOT pg_has_role(current_user, 'ledger_owner', 'USAGE') THEN
        RAISE EXCEPTION 'ledger: a chain cursor may not be created at block % (more than % ahead of genesis) -- every deposit below it would be invisible to every code path; seeding a chain that really starts high goes through ledger_seed_chain_cursor(), which is owner-only and leaves a forensic row',
            NEW.last_scanned_block, cap
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT ledger_chain_cursor_seed_is_authorized() THEN
        RAISE EXCEPTION 'ledger: a chain cursor may not be created at block % by a raw INSERT, even as owner -- use ledger_seed_chain_cursor(chain_id, block, reason), which sets the transaction-local flag this guard requires and writes the forensic row',
            NEW.last_scanned_block
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

ALTER FUNCTION ledger_chain_cursors_insert_guard() OWNER TO ledger_owner;
REVOKE ALL ON FUNCTION ledger_chain_cursors_insert_guard() FROM PUBLIC;

CREATE TRIGGER chain_cursors_insert_guard
    BEFORE INSERT ON public.chain_cursors
    FOR EACH ROW EXECUTE FUNCTION ledger_chain_cursors_insert_guard();

-- The door itself. Mirrors ledger_rewind_chain_cursor: owner-only, mandatory
-- reason, forensic row under its own table_name, and it refuses to be a
-- silent no-op.
CREATE FUNCTION ledger_seed_chain_cursor(p_chain_id bigint, p_block bigint, p_reason text) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_existing BIGINT;
BEGIN
    IF p_reason IS NULL OR btrim(p_reason) = '' THEN
        RAISE EXCEPTION 'ledger: seeding a chain cursor requires a reason -- it is the only thing that distinguishes this from the write it exists to bound'
            USING ERRCODE = 'check_violation';
    END IF;

    IF p_block < 0 THEN
        RAISE EXCEPTION 'ledger: chain cursor seed block must not be negative, got %', p_block
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT last_scanned_block INTO v_existing FROM chain_cursors WHERE chain_id = p_chain_id FOR UPDATE;
    IF v_existing IS NOT NULL THEN
        -- Never silent: seeding is for a chain with no cursor. Moving an
        -- existing one forward is SetChainCursor's job (bounded), and moving
        -- it back is ledger_rewind_chain_cursor's (audited).
        RAISE EXCEPTION 'ledger: chain % already has a cursor at %; seeding is only for a chain that has none -- use SetChainCursor to advance or ledger_rewind_chain_cursor() to go back',
            p_chain_id, v_existing
            USING ERRCODE = 'check_violation';
    END IF;

    PERFORM set_config('ledger.seed_chain_cursor', 'on', true);
    INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES (p_chain_id, p_block);
    PERFORM set_config('ledger.seed_chain_cursor', 'off', true);

    -- 'null'::jsonb, not NULL: config_table_changes.old_row is NOT NULL, and
    -- the JSON null is this schema's way of saying "there was no prior row"
    -- (029's INSERT-audit path settled on the same spelling).
    INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by)
    VALUES (
        'ledger_seed_chain_cursor',
        'null'::jsonb,
        jsonb_build_object('chain_id', p_chain_id, 'last_scanned_block', p_block, 'reason', p_reason),
        session_user
    );
END;
$$;

ALTER FUNCTION ledger_seed_chain_cursor(bigint, bigint, text) OWNER TO ledger_owner;

-- No GRANT to ledger_app, for the reason 027 and 029 both give: a capability
-- held by the credential the threat model assumes is leaked is not a
-- capability, it is the attack with a nicer name.
REVOKE ALL ON FUNCTION ledger_seed_chain_cursor(bigint, bigint, text) FROM PUBLIC;
