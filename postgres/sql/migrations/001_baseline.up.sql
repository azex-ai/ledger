-- 001_baseline.up.sql -- the whole ledger schema, in one file.
--
-- ============================================================================
-- WHY THERE IS ONLY ONE MIGRATION
-- ============================================================================
--
-- This schema was built incrementally over 53 migrations. Every one of them
-- existed to move a database that already held data from one shape to
-- another, and several exist only to repair an earlier one. None of that is
-- useful to someone installing the ledger for the first time: they have no
-- data to migrate, no live connection to protect, and no reason to replay a
-- history of decisions that were already superseded before they arrived.
--
-- So the increments were squashed. The migration history itself is not lost --
-- it is in git, and `git log -- postgres/sql/migrations` still shows every
-- step and its reasoning. What is deliberately given up is the ability to
-- prove "migration N applies safely to a database at N-1". That ability has
-- value only when databases at N-1 exist; this library has no external
-- consumers (see `ledger-no-compat-constraint`), so it had none.
--
-- What is NOT given up is the reasoning. Roughly half the value of this
-- schema is in knowing *why* each guard exists and which specific attack or
-- bug it answers. A `pg_dump --schema-only` would have produced the same
-- objects and thrown all of that away. This file is hand-written, and the
-- prose from the migrations it replaces has been carried over and reorganised
-- into one continuous account rather than 53 appended fragments.
--
-- New migrations start at 002.
--
-- ============================================================================
-- INSTALL PREREQUISITE
-- ============================================================================
--
-- The connection that runs this file must be able to CREATE ROLE (a
-- superuser, or any role with CREATEROLE -- every managed-Postgres master
-- user qualifies). This is a one-time requirement of installation, not of
-- running the ledger.
--
-- The reason is section 14: the ledger's default state is the locked-down
-- one. Three roles are created, PUBLIC loses its schema access, every object
-- is owned by `ledger_owner`, and the application role `ledger_app` gets the
-- narrowest grants that still let it work. The alternative -- ship open and
-- document a hardening step -- was rejected: a rule that has to be
-- remembered is a rule that gets skipped, and the whole integrity-hardening
-- wave that produced sections 12 and 14 is a record of exactly that
-- happening.
--
-- The incremental chain could not do this in one step. It needed an
-- expand -> migrate -> contract sequence (old migrations 042 and 049) purely
-- because a live connection role was already seated and could not be
-- stranded mid-rollout -- and getting that sequence wrong was the single
-- worst defect of the wave. A fresh install has no seated role, so the
-- sequence has no purpose here and is not reproduced.
--
-- ============================================================================
-- CONVENTIONS
-- ============================================================================
--
-- No NULL: columns are NOT NULL with a meaningful zero value (0, '', 'epoch',
-- '{}'). The one class of exception is FK-target columns where "absent" is a
-- real state: those must be nullable, because Postgres cannot enforce
-- referential integrity against a 0 sentinel. Those columns are called out
-- individually below.
--
-- Money is NUMERIC(30,18) everywhere, never a float type.
--
-- `uid` (UUID) is the only externally visible identifier -- internal
-- BIGSERIAL ids appear in no public contract, including the library-mode Go
-- API. uid is generated Go-side as UUIDv7 on insert and deliberately has no
-- database DEFAULT, so a write path that forgets to supply one fails loudly
-- instead of quietly minting a second source of ids.


------------------------------------------------------------------------------
-- 1. ROLES
--
-- Created first so section 14 can hand them ownership and grants. No
-- passwords are set here -- secrets never enter a migration file or git. An
-- operator sets them out of band.
--
--   ledger_owner  owns every object. The only role with DDL rights
--                 (ALTER/DROP/TRUNCATE/trigger management), because Postgres
--                 does not let GRANT confer those -- only ownership does.
--                 This is what makes the triggers in section 12 more than
--                 advisory: dropping one requires ownership, and the
--                 application does not have it.
--   ledger_app    the running application. SELECT/INSERT/UPDATE on ordinary
--                 tables, SELECT/INSERT only on append-only ones, no DELETE
--                 anywhere, no DDL.
--   ledger_ro     SELECT everywhere, for reporting and BI. This is the role a
--                 dashboard should hold instead of the superuser session that
--                 the 2026-05 credential leak actually used.
--
-- `createrole_self_grant = 'set'` is scoped to this transaction and matters
-- only for a non-superuser runner: since PostgreSQL 16, CREATEROLE alone no
-- longer implies any relationship to a role you create, so without it a
-- managed-Postgres master user would create ledger_owner and then be unable
-- to run `ALTER TABLE ... OWNER TO ledger_owner` in section 14. It grants
-- exactly "member WITH SET OPTION" and nothing more -- deliberately not
-- INHERIT, so the connecting role never silently carries ledger_owner's
-- privileges.
------------------------------------------------------------------------------
SET LOCAL createrole_self_grant = 'set';
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_owner') THEN
        CREATE ROLE ledger_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END $$;
SET LOCAL createrole_self_grant = DEFAULT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_app') THEN
        CREATE ROLE ledger_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_ro') THEN
        CREATE ROLE ledger_ro LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END $$;


------------------------------------------------------------------------------
-- 2. CHART OF ACCOUNTS
--
-- classifications is the primary entity of this ledger: deposit, withdrawal
-- and friends are configured classifications with a lifecycle, not hardcoded
-- types.
--
-- currencies.exponent declares the maximum decimal places an amount may
-- carry for that currency (JPY=0, USD=2, USDT=6, wei=18). NUMERIC(30,18) is
-- storage precision, not business precision -- without this column a 0.001
-- JPY entry would be perfectly legal. The write path rejects an amount whose
-- scale exceeds the exponent (core.ErrPrecisionExceeded); it never silently
-- rounds. 18 is the loosest setting and the default.
--
-- Nothing here is ever hard-deleted -- historical journal entries reference
-- these rows forever -- so is_active is a soft delete that hides a row from
-- active listings while keeping referential integrity intact.
--
-- classifications.balance_role is a semantic liquidity tag that lets the
-- library expose a holder-facing available/pending/locked breakdown without
-- core hardcoding preset classification codes:
--   ''          not part of the holder's spendable-money view (fee_expense,
--               suspense, custodial, revenue/expense classifications)
--   'available' immediately spendable (e.g. main_wallet)
--   'pending'   inbound funds awaiting confirmation
--   'locked'    journal-locked funds (e.g. a withdrawal in flight)
-- Reservation holds are deliberately NOT a role: they live in `reservations`
-- and are layered onto the role sums at read time (available -= held,
-- locked += held).
--
-- display_label is the user-readable name for the holder transaction view.
-- Empty means "not configured" -- the projection falls back to `name`.
------------------------------------------------------------------------------
CREATE TABLE currencies (
    id        BIGSERIAL PRIMARY KEY,
    code      TEXT UNIQUE NOT NULL,
    name      TEXT NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT true,
    exponent  SMALLINT NOT NULL DEFAULT 18 CHECK (exponent >= 0 AND exponent <= 18),
    uid       UUID NOT NULL
);

CREATE TABLE classifications (
    id            BIGSERIAL PRIMARY KEY,
    code          TEXT UNIQUE NOT NULL,
    name          TEXT NOT NULL,
    normal_side   TEXT NOT NULL CHECK (normal_side IN ('debit', 'credit')),
    is_system     BOOLEAN NOT NULL DEFAULT false,
    is_active     BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Optional state machine. '{}' means label-only: the classification can
    -- be referenced by journal entries but no booking can be opened against
    -- it.
    lifecycle     JSONB NOT NULL DEFAULT '{}',
    uid           UUID NOT NULL,
    balance_role  TEXT NOT NULL DEFAULT ''
                  CHECK (balance_role IN ('', 'available', 'pending', 'locked')),
    display_label TEXT NOT NULL DEFAULT ''
);

CREATE TABLE journal_types (
    id            BIGSERIAL PRIMARY KEY,
    code          TEXT UNIQUE NOT NULL,
    name          TEXT NOT NULL,
    is_active     BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid           UUID NOT NULL,
    display_label TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX uq_currencies_uid      ON currencies (uid);
CREATE UNIQUE INDEX uq_classifications_uid ON classifications (uid);
CREATE UNIQUE INDEX uq_journal_types_uid   ON journal_types (uid);


------------------------------------------------------------------------------
-- 3. ENTRY TEMPLATES
--
-- A template renders a balanced set of entries from a named-amount map, so
-- callers describe "a deposit of X" rather than assembling debits and credits
-- by hand. holder_role picks the user side or the system counterpart side of
-- each leg.
------------------------------------------------------------------------------
CREATE TABLE entry_templates (
    id              BIGSERIAL PRIMARY KEY,
    code            TEXT UNIQUE NOT NULL,
    name            TEXT NOT NULL,
    journal_type_id BIGINT NOT NULL REFERENCES journal_types(id),
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid             UUID NOT NULL
);

CREATE TABLE entry_template_lines (
    id                BIGSERIAL PRIMARY KEY,
    template_id       BIGINT NOT NULL REFERENCES entry_templates(id),
    classification_id BIGINT NOT NULL REFERENCES classifications(id),
    entry_type        TEXT NOT NULL CHECK (entry_type IN ('debit', 'credit')),
    holder_role       TEXT NOT NULL CHECK (holder_role IN ('user', 'system')),
    amount_key        TEXT NOT NULL,
    sort_order        INT NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX uq_entry_templates_uid ON entry_templates (uid);


------------------------------------------------------------------------------
-- 4. THE LEDGER: journals AND journal_entries
--
-- Double entry, append-only. A journal is one balanced accounting event; its
-- entries are the legs. Corrections are made by posting a reversal journal,
-- never by editing history -- which is why section 12 blocks UPDATE and
-- DELETE on both tables at the database level rather than trusting callers.
--
-- Column notes:
--
--   effective_at  the business date the journal is attributed to, separate
--                 from created_at (when it was written). This is what makes
--                 retroactive posting possible -- a late invoice, a delayed
--                 on-chain confirmation -- and it is the foundation of period
--                 close and trial-balance reporting. Denormalised onto entries
--                 so as-of aggregation never needs to join journals.
--
--   reversal_of   the journal this one reverses. Deliberately NOT unique: a
--                 journal may be reversed repeatedly in fractions (a 1/3
--                 refund followed by another 1/3 refund both point here).
--                 Cumulative conservation -- the sum of reversed amounts per
--                 entry never exceeding the original -- is enforced in
--                 application code under SELECT ... FOR UPDATE, not by a
--                 unique index.
--
--   event_id      the booking transition that caused this journal, nullable
--                 because most journals have none. Set once: NULL -> non-NULL
--                 is the only legal transition (enforced in section 12).
--
--   auth_digest / auth_signature / auth_key_id / auth_status
--                 per-journal authorization signature: the canonical
--                 uid-space digest that was signed, the signature over it,
--                 the signing key's id (so key rotation is traceable per
--                 journal), and why the other three are empty when they are.
--                 auth_status exists because "empty" used to conflate three
--                 very different situations:
--                   'unsigned_no_attestor' -- no signer is configured for
--                       this deployment at all;
--                   'unsigned_tx_mode' -- the journal was posted through a
--                       write path with no safe point to call a signer,
--                       because calling an external service inside a database
--                       transaction is forbidden (reversals,
--                       ExecuteTemplateBatch, anything composed via RunInTx);
--                   'signed'.
--                 A row forged by direct SQL is a fourth case that this
--                 column cannot distinguish -- an attacker writes whatever
--                 status they like. That is not what it is for: the defence
--                 against SQL-level forgery is the signature itself, verified
--                 out of band. This column is an audit aid for the legitimate
--                 paths.
--
-- journal_entries is partitioned monthly by created_at. Its primary key is
-- (id, created_at) rather than id alone because a partitioned table's primary
-- key must include the partition key; it backs logical replication (REPLICA
-- IDENTITY needs one) and is a uniqueness backstop beyond trusting the
-- sequence.
--
-- The two `account_holder <> 0` CHECKs on several tables below are a
-- redundant pair: the auto-named one came from the original inline CHECK, the
-- `chk_*_nonzero` one from a later sweep that re-asserted the rule across
-- every table without noticing some already had it. They are kept as a pair
-- because collapsing them is a real schema change with its own review, not
-- something a squash should do silently. 0 is the codebase's "absent"
-- sentinel and must never appear on a real ledger row.
------------------------------------------------------------------------------
CREATE TABLE journals (
    id              BIGSERIAL PRIMARY KEY,
    journal_type_id BIGINT NOT NULL REFERENCES journal_types(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    total_debit     NUMERIC(30,18) NOT NULL,
    total_credit    NUMERIC(30,18) NOT NULL,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    actor_id        BIGINT NOT NULL DEFAULT 0,
    source          TEXT NOT NULL DEFAULT '',
    reversal_of     BIGINT REFERENCES journals(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- FK added at the end of section 7: events does not exist yet, and
    -- events references bookings which references journals -- the cycle has to
    -- be broken somewhere.
    event_id        BIGINT,
    effective_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid             UUID NOT NULL,
    auth_digest     BYTEA NOT NULL DEFAULT ''::bytea,
    auth_signature  BYTEA NOT NULL DEFAULT ''::bytea,
    auth_key_id     TEXT NOT NULL DEFAULT '',
    auth_status     TEXT NOT NULL DEFAULT 'unsigned_no_attestor'
                    CHECK (auth_status IN ('signed', 'unsigned_no_attestor', 'unsigned_tx_mode')),
    CONSTRAINT chk_journal_balance CHECK (total_debit = total_credit),
    CONSTRAINT chk_journal_nonzero CHECK (total_debit > 0)
);

CREATE INDEX idx_journals_created     ON journals (created_at);
CREATE INDEX idx_journals_reversal_of ON journals (reversal_of) WHERE reversal_of IS NOT NULL;
CREATE INDEX idx_journals_event       ON journals (event_id) WHERE event_id IS NOT NULL;
CREATE UNIQUE INDEX uq_journals_uid   ON journals (uid);

CREATE TABLE journal_entries (
    id                BIGSERIAL,
    journal_id        BIGINT NOT NULL REFERENCES journals(id),
    account_holder    BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id       BIGINT NOT NULL REFERENCES currencies(id),
    classification_id BIGINT NOT NULL REFERENCES classifications(id),
    entry_type        TEXT NOT NULL CHECK (entry_type IN ('debit', 'credit')),
    amount            NUMERIC(30,18) NOT NULL CHECK (amount > 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_journal_entries_account_holder_nonzero CHECK (account_holder <> 0),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- Monthly partitions plus a rolling four-month horizon, and an empty DEFAULT
-- partition as the safety net so a write outside the horizon never fails
-- outright. From here the worker's partition job keeps the horizon ahead of
-- now(); this block only has to cover the gap until it first runs.
--
-- The horizon is computed from now() rather than hardcoded, so installing
-- this file next year produces next year's partitions.
DO $$
DECLARE
    horizon date := (date_trunc('month', now()) + interval '4 months')::date;
    m       date := date_trunc('month', now())::date;
BEGIN
    WHILE m <= horizon LOOP
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF journal_entries FOR VALUES FROM (%L) TO (%L)',
            format('journal_entries_y%sm%s', to_char(m, 'YYYY'), to_char(m, 'MM')),
            m, (m + interval '1 month')::date);
        m := (m + interval '1 month')::date;
    END LOOP;
END $$;

CREATE TABLE journal_entries_default PARTITION OF journal_entries DEFAULT;

-- Created on the partitioned parent so they propagate to every existing and
-- future partition.
--
-- idx_entries_account_id serves the balance read path: checkpoint balance
-- plus the sum of entries after the checkpoint's last_entry_id, for one
-- (holder, currency, classification) dimension.
-- idx_entries_currency_effective serves as-of and trial-balance queries
-- (WHERE currency_id = $1 AND effective_at <= $2).
CREATE INDEX idx_entries_account_id        ON journal_entries (account_holder, currency_id, classification_id, id);
CREATE INDEX idx_entries_journal           ON journal_entries (journal_id);
CREATE INDEX idx_entries_currency_effective ON journal_entries (currency_id, effective_at);


------------------------------------------------------------------------------
-- 5. BALANCE MATERIALISATION
--
-- A balance is never stored as a single mutable number. It is
-- `checkpoint.balance + SUM(entries WHERE id > checkpoint.last_entry_id)`, so
-- the authoritative value is always derivable from append-only entries and a
-- corrupted checkpoint can be recomputed rather than trusted.
--
-- rollup_queue is the work list of dimensions whose checkpoint has fallen
-- behind. failed_attempts exists because a permanently unprocessable item
-- (say a malformed normal_side) would otherwise retry forever; the worker
-- increments it and skips items past a threshold.
--
-- balance_snapshots and system_rollups are reporting aggregates, not sources
-- of truth.
--
-- Both bookkeeping tables carry the `account_holder <> 0` CHECK for the same
-- reason the ledger tables do -- a holder=0 row here would silently pollute
-- aggregates that split on the sign of the holder (positive = user,
-- negative = system counterpart).
------------------------------------------------------------------------------
CREATE TABLE balance_checkpoints (
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    classification_id BIGINT NOT NULL,
    balance           NUMERIC(30,18) NOT NULL DEFAULT 0,
    last_entry_id     BIGINT NOT NULL DEFAULT 0,
    last_entry_at     TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_holder, currency_id, classification_id),
    CONSTRAINT chk_checkpoints_holder_nonzero CHECK (account_holder <> 0)
);

CREATE TABLE rollup_queue (
    id                BIGSERIAL PRIMARY KEY,
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    classification_id BIGINT NOT NULL,
    claimed_until     TIMESTAMPTZ,
    processed_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    failed_attempts   INTEGER NOT NULL DEFAULT 0,
    CONSTRAINT chk_rollup_queue_holder_nonzero CHECK (account_holder <> 0)
);

-- At most one unprocessed queue item per dimension, so a burst of writes to
-- one account does not queue a thousand identical rollups.
CREATE UNIQUE INDEX uq_rollup_queue_pending_dimension
    ON rollup_queue (account_holder, currency_id, classification_id)
    WHERE processed_at IS NULL;
CREATE INDEX idx_rollup_queue_pending ON rollup_queue (created_at, id) WHERE processed_at IS NULL;

CREATE TABLE balance_snapshots (
    account_holder    BIGINT NOT NULL,
    currency_id       BIGINT NOT NULL,
    classification_id BIGINT NOT NULL,
    snapshot_date     DATE NOT NULL,
    balance           NUMERIC(30,18) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_holder, currency_id, classification_id, snapshot_date)
);

CREATE INDEX idx_snapshots_date ON balance_snapshots (snapshot_date);

CREATE TABLE system_rollups (
    currency_id       BIGINT NOT NULL REFERENCES currencies(id),
    classification_id BIGINT NOT NULL REFERENCES classifications(id),
    total_balance     NUMERIC(30,18) NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (currency_id, classification_id)
);


------------------------------------------------------------------------------
-- 6. RESERVATIONS
--
-- Reserve-then-settle, never check-then-debit: an estimated amount is locked
-- atomically up front, then settled for the real amount. This is what makes
-- concurrent spending safe without holding a row lock across an external
-- call.
--
--   journal_id  nullable FK, not a 0 sentinel -- "no journal linked yet" is a
--               real state and Postgres cannot enforce a foreign key against
--               a sentinel. Set once.
--   status      active -> {settling, settled, released}, settling ->
--               {settled, released}; settled and released are terminal. The
--               same state machine is enforced by trigger in section 12.
--
-- reservation_settlement_legs makes partial settlement idempotent. Settling
-- is an accumulator (settled_amount += x), so a client retrying a lost
-- response used to double-apply. Each partial settlement now inserts a leg
-- keyed by a caller-supplied idempotency key: a replay with the same amount
-- succeeds without re-applying, a replay with a different amount is a
-- conflict. Legs are internal -- they appear in no public contract, so they
-- carry no uid.
------------------------------------------------------------------------------
CREATE TABLE reservations (
    id              BIGSERIAL PRIMARY KEY,
    account_holder  BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id     BIGINT NOT NULL REFERENCES currencies(id),
    reserved_amount NUMERIC(30,18) NOT NULL CHECK (reserved_amount > 0),
    settled_amount  NUMERIC(30,18) NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'settling', 'settled', 'released')),
    journal_id      BIGINT REFERENCES journals(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '15 minutes',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid             UUID NOT NULL,
    CONSTRAINT chk_settled_non_negative CHECK (settled_amount >= 0),
    CONSTRAINT chk_settled_lte_reserved CHECK (settled_amount <= reserved_amount),
    CONSTRAINT chk_reservations_account_holder_nonzero CHECK (account_holder <> 0)
);

CREATE INDEX idx_reservations_account_status ON reservations (account_holder, currency_id, status) WHERE status = 'active';
CREATE INDEX idx_reservations_expired        ON reservations (expires_at) WHERE status = 'active';
-- Listing a holder's reservations filters on account_holder with an optional
-- status and orders by created_at DESC. The two partial indexes above cannot
-- serve that (they only cover active rows), so this covering, non-partial
-- index exists to keep it off a sequential scan + sort. currency_id is
-- deliberately excluded: the query does not filter on it, and putting it
-- between account_holder and created_at would break the ORDER BY.
CREATE INDEX idx_reservations_account_created ON reservations (account_holder, created_at DESC);
CREATE UNIQUE INDEX uq_reservations_uid       ON reservations (uid);

CREATE TABLE reservation_settlement_legs (
    id              BIGSERIAL PRIMARY KEY,
    reservation_id  BIGINT NOT NULL REFERENCES reservations(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    amount          NUMERIC(30,18) NOT NULL CHECK (amount > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_settlement_legs_reservation ON reservation_settlement_legs (reservation_id);


------------------------------------------------------------------------------
-- 7. BOOKINGS AND EVENTS
--
-- A booking is one instance of a classification's lifecycle -- "book a
-- deposit", "book a withdrawal", in the ordinary banking sense. It replaced
-- the separate deposits/withdrawals tables of section 8, which exist now only
-- as history.
--
-- An event is the atomic record of a lifecycle transition, written in the
-- same transaction as the booking update. Events are also the outbound
-- delivery queue: delivery_status/attempts/next_attempt_at drive the webhook
-- worker.
--
-- Nullable-FK columns (journal_id, reservation_id) follow the same rule as
-- reservations.journal_id: "absent" must be NULL, because a 0 sentinel
-- defeats referential integrity. bookings.journal_id is additionally
-- set-once, which means a booking's lifecycle may have at most one
-- journal-bearing transition -- lifecycles where every transition posts
-- accounting model that per-event instead.
--
-- events.booking_id keeps its 0 default from before bookings became mandatory
-- for events; the FK makes 0 unwritable in practice, so the default is inert
-- history rather than a live code path.
------------------------------------------------------------------------------
CREATE TABLE bookings (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_id BIGINT NOT NULL REFERENCES classifications(id) ON DELETE RESTRICT,
    account_holder    BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id       BIGINT NOT NULL REFERENCES currencies(id) ON DELETE RESTRICT,
    amount            NUMERIC(30,18) NOT NULL,
    settled_amount    NUMERIC(30,18) NOT NULL DEFAULT 0,
    status            TEXT NOT NULL,
    channel_name      TEXT NOT NULL DEFAULT '',
    channel_ref       TEXT NOT NULL DEFAULT '',
    reservation_id    BIGINT REFERENCES reservations(id) ON DELETE RESTRICT,
    journal_id        BIGINT REFERENCES journals(id) ON DELETE RESTRICT,
    idempotency_key   TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}',
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid               UUID NOT NULL,

    CONSTRAINT uq_bookings_idempotency UNIQUE (idempotency_key),
    CONSTRAINT chk_bookings_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_bookings_settled_non_negative CHECK (settled_amount >= 0),
    CONSTRAINT chk_bookings_account_holder_nonzero CHECK (account_holder <> 0)
);

-- One booking per (channel, external reference), so a channel redelivering
-- the same sighting cannot open a second booking. '' means "no external
-- reference", which is why the index is partial.
CREATE UNIQUE INDEX uq_bookings_channel_ref
    ON bookings (channel_name, channel_ref)
    WHERE channel_ref != '';

CREATE INDEX idx_bookings_holder_class ON bookings (account_holder, classification_id, status);
CREATE INDEX idx_bookings_expires      ON bookings (expires_at) WHERE expires_at != 'epoch';
CREATE UNIQUE INDEX uq_bookings_uid    ON bookings (uid);

CREATE TABLE events (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    classification_code TEXT NOT NULL,
    booking_id          BIGINT NOT NULL DEFAULT 0 REFERENCES bookings(id) ON DELETE RESTRICT,
    account_holder      BIGINT NOT NULL DEFAULT 0,
    currency_id         BIGINT NOT NULL DEFAULT 0,
    from_status         TEXT NOT NULL DEFAULT '',
    to_status           TEXT NOT NULL,
    amount              NUMERIC(30,18) NOT NULL DEFAULT 0,
    settled_amount      NUMERIC(30,18) NOT NULL DEFAULT 0,
    journal_id          BIGINT REFERENCES journals(id) ON DELETE RESTRICT,
    metadata            JSONB NOT NULL DEFAULT '{}',
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    delivery_status     TEXT NOT NULL DEFAULT 'pending',
    attempts            INT NOT NULL DEFAULT 0,
    max_attempts        INT NOT NULL DEFAULT 10,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_id            BIGINT NOT NULL DEFAULT 0,
    source              TEXT NOT NULL DEFAULT '',
    uid                 UUID NOT NULL
);

CREATE INDEX idx_events_delivery_pending ON events (next_attempt_at) WHERE delivery_status = 'pending';
CREATE INDEX idx_events_booking          ON events (booking_id) WHERE booking_id != 0;
CREATE UNIQUE INDEX uq_events_uid        ON events (uid);

-- The deferred half of the journals <-> events cycle. See journals.event_id.
ALTER TABLE journals
    ADD CONSTRAINT journals_event_id_fkey FOREIGN KEY (event_id) REFERENCES events(id);


------------------------------------------------------------------------------
-- 8. deposits AND withdrawals -- HISTORY, NOT LIVE
--
-- These are the pre-booking tables. No application code reads or writes them
-- any more; `bookings` replaced both. They are kept because dropping a table
-- that might hold real rows in someone's database is a decision to make
-- deliberately and separately, not a side effect of a schema squash -- and
-- because a schema tidy-up is exactly the change that quietly destroys data
-- nobody remembered was there.
--
-- They picked up the holder<>0 CHECK when that rule was swept across every
-- table, but their nullable columns -- deposits.actual_amount,
-- {deposits,withdrawals}.channel_ref and .expires_at -- were left as they
-- were. The No-NULL convention arrived after these tables were already dead,
-- and rewriting columns nothing reads would have been churn. So they are
-- deliberately NOT held to the convention the live tables follow, which is
-- worth knowing before anyone reads them as an example of it.
------------------------------------------------------------------------------
CREATE TABLE deposits (
    id              BIGSERIAL PRIMARY KEY,
    account_holder  BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id     BIGINT NOT NULL REFERENCES currencies(id),
    expected_amount NUMERIC(30,18) NOT NULL CHECK (expected_amount > 0),
    actual_amount   NUMERIC(30,18),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'confirming', 'confirmed', 'failed', 'expired')),
    channel_name    TEXT NOT NULL,
    channel_ref     TEXT UNIQUE,
    journal_id      BIGINT REFERENCES journals(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_deposits_account_holder_nonzero CHECK (account_holder <> 0)
);

CREATE INDEX idx_deposits_account ON deposits (account_holder, currency_id, status);

CREATE TABLE withdrawals (
    id              BIGSERIAL PRIMARY KEY,
    account_holder  BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id     BIGINT NOT NULL REFERENCES currencies(id),
    amount          NUMERIC(30,18) NOT NULL CHECK (amount > 0),
    status          TEXT NOT NULL DEFAULT 'locked'
                    CHECK (status IN ('locked', 'reserved', 'reviewing', 'processing', 'confirmed', 'failed', 'expired')),
    channel_name    TEXT NOT NULL,
    channel_ref     TEXT UNIQUE,
    reservation_id  BIGINT REFERENCES reservations(id),
    journal_id      BIGINT REFERENCES journals(id),
    idempotency_key TEXT UNIQUE NOT NULL,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    review_required BOOLEAN NOT NULL DEFAULT false,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_withdrawals_account_holder_nonzero CHECK (account_holder <> 0)
);

CREATE INDEX idx_withdrawals_account ON withdrawals (account_holder, currency_id, status);


------------------------------------------------------------------------------
-- 9. ACCOUNT POLICY AND PERIOD CLOSE
--
-- account_policies are optional per-dimension overrides on the otherwise
-- implicit (account_holder, currency_id, classification_id) account. No row
-- means "active and unconstrained", so this is additive by construction --
-- installing it changes nothing about existing behaviour.
--
-- currency_id = 0 means "all currencies for this holder", classification_id =
-- 0 likewise. These two are the deliberate exception to the FK rule: they are
-- wildcards, not absent references, so there is no row for them to point at.
--
-- min_balance is a floor, not a limit: 0 = no overdraft, negative = an
-- overdraft/credit line of |min_balance|, positive = a dust floor (the
-- anti-dust-attack minimum belongs here). Only enforced when
-- enforce_min_balance is true.
--
-- The policy row itself is UPDATEd in place -- it is operational config, not
-- funds -- but every change is appended to account_policy_changes. That keeps
-- an audit trail without forcing ledger-grade immutability onto config rows.
--
-- period_closes is an append-only close log. The active close line at any
-- moment is the row with the latest created_at; reopening a period means
-- appending a row with an earlier close_before. Nothing is ever updated or
-- deleted, so the full history of closes and reopenings stays auditable --
-- enforced by trigger in section 12, because "documented as append-only" was
-- all this table had for a long time.
------------------------------------------------------------------------------
CREATE TABLE account_policies (
    id                  BIGSERIAL PRIMARY KEY,
    account_holder      BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id         BIGINT NOT NULL DEFAULT 0,
    classification_id   BIGINT NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'frozen', 'closed')),
    min_balance         NUMERIC(30,18) NOT NULL DEFAULT 0,
    enforce_min_balance BOOLEAN NOT NULL DEFAULT false,
    note                TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid                 UUID NOT NULL,
    UNIQUE (account_holder, currency_id, classification_id)
);

CREATE INDEX idx_account_policies_holder    ON account_policies (account_holder);
CREATE UNIQUE INDEX uq_account_policies_uid ON account_policies (uid);

CREATE TABLE account_policy_changes (
    id         BIGSERIAL PRIMARY KEY,
    policy_id  BIGINT NOT NULL REFERENCES account_policies(id),
    old_state  JSONB NOT NULL,
    new_state  JSONB NOT NULL,
    actor_id   BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_account_policy_changes_policy ON account_policy_changes (policy_id, created_at DESC);

CREATE TABLE period_closes (
    id           BIGSERIAL PRIMARY KEY,
    close_before TIMESTAMPTZ NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    actor_id     BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    uid          UUID NOT NULL
);

CREATE INDEX idx_period_closes_created      ON period_closes (created_at DESC);
CREATE UNIQUE INDEX uq_period_closes_uid    ON period_closes (uid);


------------------------------------------------------------------------------
-- 10. CHANNELS AND INGESTION
--
-- webhook_subscribers is the outbound side: who receives events, filtered by
-- classification and target status. last_status_code/last_error/
-- last_attempt_at record the most recent attempt so an operator can see which
-- endpoints are failing without grepping application logs.
--
-- webhook_nonces is the inbound replay cache. Signature verification's
-- timestamp window rejects only STALE replays -- a captured request replayed
-- inside the window verifies fine, and used to rely entirely on downstream
-- transition idempotency to be harmless. Recording each seen signature closes
-- that at the HTTP boundary. This is a cache, not ledger data: rows older than
-- the replay window are deleted opportunistically. It is the one sanctioned
-- DELETE in this schema, and nothing financial lives here.
--
-- deposit_addresses is the address <-> holder registry behind CREATE2-derived
-- crypto deposit addresses. factory and init_hash are recorded per row rather
-- than only read from config, so a later factory redeploy or init-hash change
-- is auditable per address instead of being visible only in whatever config
-- happens to be current.
--
-- One address per holder: the derivation salts on the holder, which locks a
-- 1:1 mapping. Addresses are always stored in the canonical EIP-55 checksum
-- casing the derivation produces, so a plain case-sensitive unique index is
-- sufficient -- as long as every read and write path goes through that same
-- derivation. A store adapter that bypasses it and lowercases would split one
-- holder across two rows, which is why the normalisation lives in exactly one
-- place.
--
-- chain_cursors is the watcher's per-chain log-scan progress, so a restart
-- resumes instead of rescanning from genesis or skipping unseen blocks.
--
-- ingest_dead_letters holds sightings that could not be booked because the
-- watcher path and the webhook path derived different payloads for what
-- should be the same event -- a normalisation bug, not a transient failure.
-- These are never auto-retried; on-call reconciles them by hand. The unique
-- index on idempotency_key is load-bearing: it lets the writer use
-- ON CONFLICT DO NOTHING so a watcher retrying a sighting it can never
-- reconcile does not spam the table.
--
-- registration_rescans is the backfill queue for an address registered after
-- deposits to it already happened.
------------------------------------------------------------------------------
CREATE TABLE webhook_subscribers (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL,
    secret           TEXT NOT NULL DEFAULT '',
    filter_class     TEXT NOT NULL DEFAULT '',
    filter_to_status TEXT NOT NULL DEFAULT '',
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status_code INT NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    last_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT 'epoch'
);

CREATE TABLE webhook_nonces (
    nonce   TEXT PRIMARY KEY,
    seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_webhook_nonces_seen ON webhook_nonces (seen_at);

CREATE TABLE deposit_addresses (
    id             BIGSERIAL PRIMARY KEY,
    uid            UUID NOT NULL,
    account_holder BIGINT NOT NULL CHECK (account_holder > 0),
    address        TEXT NOT NULL,
    factory        TEXT NOT NULL,
    init_hash      TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_deposit_addresses_uid            ON deposit_addresses (uid);
CREATE UNIQUE INDEX uq_deposit_addresses_account_holder ON deposit_addresses (account_holder);
CREATE UNIQUE INDEX uq_deposit_addresses_address        ON deposit_addresses (address);

CREATE TABLE chain_cursors (
    chain_id           BIGINT PRIMARY KEY,
    last_scanned_block BIGINT NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ingest_dead_letters (
    id              BIGSERIAL PRIMARY KEY,
    uid             UUID NOT NULL,
    chain_id        BIGINT NOT NULL,
    tx_hash         TEXT NOT NULL,
    txlog_seq       INTEGER NOT NULL,
    idempotency_key TEXT NOT NULL,
    reason          TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_ingest_dead_letters_uid             ON ingest_dead_letters (uid);
CREATE UNIQUE INDEX uq_ingest_dead_letters_idempotency_key ON ingest_dead_letters (idempotency_key);
CREATE INDEX idx_ingest_dead_letters_chain_tx              ON ingest_dead_letters (chain_id, tx_hash);

CREATE TABLE registration_rescans (
    id            BIGSERIAL PRIMARY KEY,
    uid           UUID NOT NULL UNIQUE,
    chain_id      BIGINT NOT NULL CHECK (chain_id > 0),
    address       TEXT NOT NULL,
    next_block    BIGINT NOT NULL CHECK (next_block >= 0),
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'running', 'completed')),
    attempts      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until TIMESTAMPTZ,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chain_id, address)
);

CREATE INDEX idx_registration_rescans_claim
    ON registration_rescans (available_at, claimed_until)
    WHERE status <> 'completed';


------------------------------------------------------------------------------
-- 11. INTEGRITY, FORENSICS AND THE ATTESTATION CHAIN
--
-- reconcile_scan_cursors is a persisted resume cursor for fleet-wide
-- reconciliation scans. It exists because the cursor used to live only in
-- call-scoped memory: every scheduled run restarted from the beginning, so
-- with a scan limit of a few thousand dimensions, any fleet larger than that
-- limit had its tail permanently unscanned -- every run re-verified the same
-- prefix and never reached the rest.
--
-- Keyed by check name so a future fleet-wide check reuses the mechanism
-- without a schema change. The cursor columns start at INT64_MIN rather than 0
-- because system counterparts are negative holders: a (0, 0) start would skip
-- every one of them.
--
-- lap_dirty carries "did any segment of the CURRENT lap already find a
-- violation" across resumptions. Without it, a lap spanning several runs could
-- report a pass on the run that happens to finish the lap even though an
-- earlier run in the same lap found real drift -- looking green while it is
-- not. It resets only when a lap completes, at the same moment the cursor
-- resets.
--
-- checkpoint_rebuilds is the durable record of every checkpoint repair.
-- Rebuilding a poisoned checkpoint destroys the evidence that it was poisoned:
-- the drift disappears the moment it is overwritten, and a human choosing to
-- run the repair does not change that. Without this table all that survives is
-- a log line, which rotates. The drift IS the evidence, so it has to outlive
-- the log.
--
-- drift = previous_balance - new_balance, the same sign convention
-- reconciliation uses (what the checkpoint claimed, minus what the entries
-- say). A non-zero row is proof a poisoned checkpoint existed; a zero-drift
-- row means the rebuild ran as a no-op confirmation, not a repair -- which is
-- worth recording too, because "we checked and it was fine" is an answer.
--
-- ledger_attestations is a hash chain over batches of journal entries. It
-- exists because per-journal signatures prove "this row was authorized when
-- written" but say nothing about "this row still exists" or "the history has
-- not been renumbered" -- a DELETE is invisible to them. Here:
--   seq        gapless, so a missing seq IS a truncated batch, visible without
--              needing anything external to compare against.
--   prev_root  chains each batch to its predecessor (genesis = 32 zero bytes),
--              so rewriting any historical batch changes every root_hash after
--              it. An external anchor therefore only has to remember the
--              LATEST root_hash to make any rewrite detectable.
--   merkle_root  the RFC 6962 Merkle root over the batch's entries, so a
--              TAMPERED verdict can be narrowed from "batch N is bad" to
--              specific entry ids, and so a third party can verify one entry's
--              membership without database access.
--   auth_verdict_digest  binds every covered entry's authorization verdict
--              into this batch's signed content, so a withdrawal-time check
--              can trust an already-attested entry's cached verdict instead of
--              re-verifying every contributing journal on every read.
--
-- Three of these columns have an empty-means-something-specific sentinel, and
-- they all have the same reason. root_hash is what gets signed. Changing what
-- any of its inputs MEANS for an already-committed row would change that row's
-- root_hash and invalidate its signature -- and re-signing history is not
-- possible: it requires the original signing key for every historical key_id,
-- for a value whose entire purpose is that nothing, including a later
-- migration, can silently recompute it. So rows written before a given input
-- existed keep the older root-hash formula forever, and the verifier
-- discriminates on the sentinel:
--   merkle_root = ''         -> this row uses the pre-Merkle root formula
--   auth_verdict_digest = '' -> this row's root does not cover auth verdicts
--   entry_attestations.auth_verdict = '' -> no cached verdict for this entry
-- That last one matters most: '' means "no cached answer", NOT an answer.
-- Treating it as passing would silently widen what counts as authorized;
-- treating it as failing would make every pre-existing account permanently
-- suspect the moment the feature shipped. Callers fall back to a live check.
--
-- entry_attestations records which batch covered each entry. It is a side
-- table rather than a "covered" flag column on journal_entries, for two
-- reasons. First, journal_entries' no-UPDATE guard is one of the few hard
-- guarantees in this schema; opening it for a bookkeeping flag is exactly the
-- guard-with-an-exception pattern that decayed elsewhere in this file's
-- history. Second, coverage becomes a plain queryable fact (LEFT JOIN ...
-- WHERE seq IS NULL) instead of an id-range assumption -- entries can commit
-- out of id order across different (holder, currency) pairs, so an id-range
-- boundary would let a late-arriving small-id entry slip through a gap no
-- sequence-continuity check could ever notice.
--
-- leaf_hash is the exact RFC 6962 leaf hash that went into the batch's Merkle
-- root for this entry, stored at attestation time. It makes localisation
-- self-contained: rebuild a tree from the stored leaf hashes, rebuild another
-- from live journal_entries content, and diff. Without it, localising a
-- mismatch needed an operator-supplied trusted snapshot -- required precisely
-- at the moment tampering is discovered, which is when nobody has one.
--
-- The stored leaf hashes are themselves tamper-evident without any additional
-- signing: rebuilding a tree from them and comparing to the batch's signed
-- merkle_root detects any edit, so an attacker would have to alter the
-- leaf_hash AND the merkle_root AND re-sign root_hash consistently -- and the
-- signing key is exactly what bypassing a database trigger does not give them.
-- This is what Certificate Transparency logs do (persist the whole log, not
-- just the root), scoped here to one column per entry instead of a second copy
-- of journal_entries.
--
-- Coverage is independent of the verdict: an entry_attestations row exists for
-- every entry the worker processes, whatever its journal's authorization
-- verdict turned out to be. DELETE-detection and authorization are orthogonal
-- checks that merely share a table for storage convenience; filtering entries
-- out of coverage because their authorization is unknown would silently
-- regress DELETE-detection for every historically unsigned entry.
------------------------------------------------------------------------------
CREATE TABLE reconcile_scan_cursors (
    check_name     TEXT NOT NULL PRIMARY KEY,
    after_holder   BIGINT NOT NULL DEFAULT -9223372036854775808,
    after_currency BIGINT NOT NULL DEFAULT -9223372036854775808,
    lap_dirty      BOOLEAN NOT NULL DEFAULT false,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE checkpoint_rebuilds (
    id                     BIGSERIAL PRIMARY KEY,
    uid                    UUID NOT NULL,
    account_holder         BIGINT NOT NULL,
    currency_id            BIGINT NOT NULL REFERENCES currencies(id),
    classification_id      BIGINT NOT NULL REFERENCES classifications(id),
    previous_balance       NUMERIC(30,18) NOT NULL,
    previous_last_entry_id BIGINT NOT NULL,
    new_balance            NUMERIC(30,18) NOT NULL,
    new_last_entry_id      BIGINT NOT NULL,
    drift                  NUMERIC(30,18) NOT NULL,
    actor_id               BIGINT NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_checkpoint_rebuilds_uid ON checkpoint_rebuilds (uid);
CREATE INDEX idx_checkpoint_rebuilds_dimension
    ON checkpoint_rebuilds (account_holder, currency_id, classification_id);
-- The forensically interesting rows: "show me every real repair", fast.
CREATE INDEX idx_checkpoint_rebuilds_nonzero_drift
    ON checkpoint_rebuilds (created_at) WHERE drift <> 0;

CREATE TABLE ledger_attestations (
    id                  BIGSERIAL PRIMARY KEY,
    uid                 UUID   NOT NULL UNIQUE,
    seq                 BIGINT NOT NULL UNIQUE,
    entry_count         BIGINT NOT NULL,
    batch_digest        BYTEA  NOT NULL,
    prev_root           BYTEA  NOT NULL,
    root_hash           BYTEA  NOT NULL,
    signature           BYTEA  NOT NULL,
    key_id              TEXT   NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    merkle_root         BYTEA NOT NULL DEFAULT ''::bytea,
    auth_verdict_digest BYTEA NOT NULL DEFAULT ''::bytea
);

CREATE INDEX idx_ledger_attestations_seq ON ledger_attestations (seq);

CREATE TABLE entry_attestations (
    entry_id     BIGINT NOT NULL,
    seq          BIGINT NOT NULL REFERENCES ledger_attestations(seq),
    leaf_hash    BYTEA NOT NULL DEFAULT ''::bytea,
    auth_verdict TEXT NOT NULL DEFAULT ''
                 CHECK (auth_verdict IN ('', 'authorized', 'unauthorized')),
    PRIMARY KEY (entry_id)
);

CREATE INDEX idx_entry_attestations_seq ON entry_attestations (seq);


------------------------------------------------------------------------------
-- 12. MUTATION GUARDS
--
-- Everything above is structure. This section is the part that makes the
-- ledger's promises true against a writer who is not going through the
-- application -- someone holding a leaked application database credential, or
-- a script run by hand at 2am.
--
-- The threat model is specifically NOT "an attacker with ownership". Ownership
-- can DROP any of these triggers, which is why section 14 gives ownership to a
-- role the application does not hold. The two halves only work together: these
-- triggers without role separation are advisory, and role separation without
-- these triggers leaves the application credential able to rewrite balances.
--
-- ####  A note on how this section is written  ####
--
-- The journals guard is a generic comparison against an explicit
-- mutable-column whitelist, not a hardcoded list of protected columns. That
-- shape was arrived at the hard way. The original guard listed every protected
-- column by name, with a written rule that any migration adding a column to
-- journals must recreate the function including it. The rule was broken twice
-- before anyone noticed -- two columns were added without being added to the
-- guard, so `UPDATE journals SET effective_at = ...` silently bypassed it
-- entirely, which is enough to move a posted journal into a closed accounting
-- period. Then the repair itself turned out to reproduce the hazard: two
-- migrations each issuing their own CREATE OR REPLACE with their own hardcoded
-- list means the later one silently drops the earlier one's protection.
--
-- The root cause was never ordering. It was that the design turned "remember
-- to update this function" into a rule a human carries. Comparing
-- to_jsonb(OLD) against to_jsonb(NEW) minus a whitelist means any future
-- column is protected by default -- fail-closed by construction -- and the
-- only way to weaken it is to add a name to a three-word array, which is
-- visible in review.
------------------------------------------------------------------------------

-- The unconditional guard: any UPDATE or DELETE on the table this fires for is
-- refused, no exceptions to carve out. Used by every table where the whole row
-- is identity or audit data.
CREATE OR REPLACE FUNCTION ledger_block_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: % on % is not allowed; use a reversal journal instead',
        TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'check_violation';
END;
$$;

-- journals: append-only except the set-once event_id backfill.
--
-- event_id is the ONLY mutable column, and only NULL -> non-NULL. Both halves
-- are implemented in the function body rather than a trigger WHEN clause,
-- because for a long time the set-once promise existed only in a comment
-- describing a WHEN clause that had never been written -- the trigger was
-- unconditional and the guard permitted any event_id change. Enforce it where
-- it can be read.
CREATE OR REPLACE FUNCTION ledger_journals_block_arbitrary_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    -- The only columns allowed to change post-insert. Adding a column here
    -- is an explicit, reviewable decision; anything NOT in this array
    -- (including every column added by a future migration) is protected by
    -- default.
    mutable CONSTANT text[] := ARRAY['event_id'];
BEGIN
    -- event_id's set-once semantics: only a NULL -> non-NULL transition is
    -- legal. Implemented in the function body, not a trigger WHEN clause --
    -- 033's WHEN clause was only ever described in a comment, never
    -- written (018:137-140 is an unconditional BEFORE UPDATE FOR EACH ROW).
    IF OLD.event_id IS NOT NULL AND NEW.event_id IS DISTINCT FROM OLD.event_id THEN
        RAISE EXCEPTION 'ledger: journals.event_id is set-once and already set to %', OLD.event_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on journals is not allowed except the set-once event_id backfill; use a reversal journal instead'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

-- classifications: normal_side and balance_role.
--
-- These two columns are not money, but changing either rewrites the meaning of
-- money already recorded -- which is why they need a guard while the rest of
-- the table does not.
--
-- normal_side decides the sign of every historical rollup for the
-- classification. It has no legitimate post-insert mutation path anywhere in
-- the codebase; it was immutable by convention and enforced nowhere.
--
-- balance_role has exactly one legitimate transition, '' -> <role>, used once
-- when a deployment starts opting a classification into the holder-facing
-- balance breakdown. Switching between two non-empty roles, or reverting to
-- '', silently re-buckets a holder's available/pending/locked view with no
-- accounting event behind it. That is the transition this refuses.
--
-- Every other column here (code, name, is_system, is_active, created_at,
-- lifecycle, display_label, uid) has a legitimate mutation path and is
-- deliberately left alone.
CREATE OR REPLACE FUNCTION ledger_classifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.normal_side IS DISTINCT FROM OLD.normal_side THEN
        RAISE EXCEPTION 'ledger: classifications.normal_side is immutable; it determines the sign of every historical rollup for this classification'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.balance_role IS DISTINCT FROM OLD.balance_role AND OLD.balance_role <> '' THEN
        RAISE EXCEPTION 'ledger: classifications.balance_role is already set to %; only the '''' -> <role> upgrade is allowed', OLD.balance_role
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

-- reservations: a held amount is spendable-balance-adjacent, so the columns
-- that decide how much is held are as sensitive as a journal entry.
--
-- account_holder / currency_id / reserved_amount / idempotency_key /
-- expires_at / created_at / uid have no legitimate mutation path -- insertion
-- is the only writer of any of them.
--
-- settled_amount only ever accumulates. The settle path's own precondition
-- already guarantees that; this makes it a database-level fact rather than a
-- caller convention, and a decrease can then only be tampering.
--
-- journal_id is set-once, matching every other nullable-FK column here.
--
-- status follows the same whitelist the domain state machine uses. Encoding it
-- twice is deliberate: the domain check protects against a bug, this one
-- protects against a writer who never ran the domain code.
CREATE OR REPLACE FUNCTION ledger_reservations_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.account_holder  IS DISTINCT FROM OLD.account_holder  OR
       NEW.currency_id     IS DISTINCT FROM OLD.currency_id     OR
       NEW.reserved_amount IS DISTINCT FROM OLD.reserved_amount OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR
       NEW.expires_at      IS DISTINCT FROM OLD.expires_at      OR
       NEW.created_at      IS DISTINCT FROM OLD.created_at      OR
       NEW.uid             IS DISTINCT FROM OLD.uid THEN
        RAISE EXCEPTION 'ledger: UPDATE on reservations may not change account_holder/currency_id/reserved_amount/idempotency_key/expires_at/created_at/uid'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.settled_amount < OLD.settled_amount THEN
        RAISE EXCEPTION 'ledger: reservations.settled_amount must not decrease (% -> %)', OLD.settled_amount, NEW.settled_amount
            USING ERRCODE = 'check_violation';
    END IF;

    IF OLD.journal_id IS NOT NULL AND NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'ledger: reservations.journal_id is set-once and already set to %', OLD.journal_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT (
            (OLD.status = 'active'   AND NEW.status IN ('settling', 'settled', 'released')) OR
            (OLD.status = 'settling' AND NEW.status IN ('settled', 'released'))
        ) THEN
            RAISE EXCEPTION 'ledger: reservations status transition % -> % is not allowed', OLD.status, NEW.status
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

-- Per-journal, per-currency balance enforcement at the database layer.
--
-- The application already validates this: one aggregate query per posted
-- journal, inside the same transaction as the inserts, with a better error
-- message. This is the backstop for a writer who never ran that code -- a
-- direct INSERT into journal_entries with a leaked application credential can
-- otherwise post unbalanced entries with nothing in the database to stop it.
--
-- Getting the cost right took two attempts. The first version was a per-row
-- constraint trigger where each row's firing re-scanned every entry of the
-- affected journal: O(N) per row, O(N^2) per journal. It was dropped for that
-- reason and the check moved into the application only, which is what opened
-- the gap this closes.
--
-- Postgres constraint triggers must be FOR EACH ROW -- statement-level
-- constraint triggers do not exist -- so the per-row firing cannot be avoided.
-- Instead the function dedupes by journal_id within the current transaction
-- using a transaction-scoped temp table (ON COMMIT DELETE ROWS, so nothing
-- leaks across transactions on a pooled connection). The first row of a given
-- journal runs the aggregate check; every later row for the same journal is a
-- cheap no-op. That holds across statement boundaries too, because the check
-- reads the base table rather than the triggering row. One aggregate query per
-- journal touched, not one per row.
CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER AS $$
DECLARE
    target_journal_id BIGINT;
BEGIN
    target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
    IF target_journal_id IS NULL THEN
        RETURN NULL;
    END IF;

    -- Transaction-scoped dedup set, lives in pg_temp, cleared automatically
    -- at the end of every transaction. INSERT ... ON CONFLICT DO NOTHING
    -- sets FOUND=true only when this journal_id was NOT already present
    -- (i.e. this is the first row for it in the current transaction).
    CREATE TEMP TABLE IF NOT EXISTS ledger_balance_checked (
        journal_id BIGINT PRIMARY KEY
    ) ON COMMIT DELETE ROWS;

    INSERT INTO ledger_balance_checked (journal_id)
    VALUES (target_journal_id)
    ON CONFLICT DO NOTHING;

    IF NOT FOUND THEN
        -- Already validated this journal_id earlier in this transaction.
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM journal_entries
        WHERE journal_id = target_journal_id
        GROUP BY currency_id
        HAVING SUM(
            CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END
        ) <> 0
    ) THEN
        RAISE EXCEPTION 'journal % has unbalanced entries by currency', target_journal_id
            USING
                ERRCODE = '23514',
                CONSTRAINT = 'chk_journal_currency_balance';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

------------------------------------------------------------
-- Triggers. Each line below names the table whose history it protects; the
-- reasoning for each is in the function comments above.
------------------------------------------------------------

-- The ledger itself: entries are never edited or removed, journals are never
-- removed and never edited except the set-once event_id.
CREATE TRIGGER journal_entries_no_update
    BEFORE UPDATE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER journal_entries_no_delete
    BEFORE DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER journals_no_delete
    BEFORE DELETE ON journals
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER journals_no_arbitrary_update
    BEFORE UPDATE ON journals
    FOR EACH ROW EXECUTE FUNCTION ledger_journals_block_arbitrary_update();

CREATE CONSTRAINT TRIGGER trg_check_journal_currency_balance
    AFTER INSERT OR UPDATE OR DELETE ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION check_journal_currency_balance();

-- Tables that participate in balance computation without being the ledger.
CREATE TRIGGER classifications_mutation_guard
    BEFORE UPDATE ON classifications
    FOR EACH ROW EXECUTE FUNCTION ledger_classifications_guard();

CREATE TRIGGER reservations_mutation_guard
    BEFORE UPDATE ON reservations
    FOR EACH ROW EXECUTE FUNCTION ledger_reservations_guard();

-- Append-only audit logs. Editing any of these defeats the only purpose the
-- table has.
CREATE TRIGGER period_closes_no_update
    BEFORE UPDATE ON period_closes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER period_closes_no_delete
    BEFORE DELETE ON period_closes
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER checkpoint_rebuilds_no_update
    BEFORE UPDATE ON checkpoint_rebuilds
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER checkpoint_rebuilds_no_delete
    BEFORE DELETE ON checkpoint_rebuilds
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER ledger_attestations_no_update
    BEFORE UPDATE ON ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER ledger_attestations_no_delete
    BEFORE DELETE ON ledger_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER entry_attestations_no_update
    BEFORE UPDATE ON entry_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

CREATE TRIGGER entry_attestations_no_delete
    BEFORE DELETE ON entry_attestations
    FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();


------------------------------------------------------------------------------
-- 13. SEED
--
-- The only seeded rows in the whole schema. Everything else -- currencies,
-- journal types, templates, the rest of the chart of accounts -- is installed
-- by the consumer, either through the preset bundles or by hand.
--
-- These two exist because deposit and withdrawal are the classifications the
-- presets and the HTTP surface assume are present. Their lifecycle is '{}'
-- (label-only) until a preset installs one, and balance_role is '' until a
-- deployment opts them into the holder-facing breakdown.
--
-- These uids are the one exception to the rule stated at the top of this
-- file. Everywhere else a uid is a UUIDv7 minted Go-side, and the column
-- deliberately has no DEFAULT so a write path that forgets one fails loudly.
-- Here there is no Go-side writer -- the rows are installed by this file --
-- so gen_random_uuid() supplies a v4. The uid only has to be unique and
-- opaque, which a v4 is; what it loses is v7's time ordering, which means
-- nothing for two rows created at install. The column still has no DEFAULT,
-- so the guarantee that matters is intact.
------------------------------------------------------------------------------
INSERT INTO classifications (code, name, normal_side, is_system, uid) VALUES
    ('deposit',  'Deposit',  'debit',  true, gen_random_uuid()),
    ('withdraw', 'Withdraw', 'credit', true, gen_random_uuid())
ON CONFLICT (code) DO NOTHING;


------------------------------------------------------------------------------
-- 14. GRANTS, OWNERSHIP AND LOCKDOWN
--
-- Everything above was created by the connection running this file. This
-- section hands it all to `ledger_owner`, gives `ledger_app` and `ledger_ro`
-- the least privilege each needs, and takes PUBLIC's schema access away.
--
-- ####  Why the order below is load-bearing  ####
--
-- 1. GRANT to ledger_app / ledger_ro, then transfer ownership. Not the other
--    way round: after the transfer, the running connection no longer owns
--    anything and cannot grant on it. Granting first also produces the right
--    grantor -- ALTER ... OWNER TO rewrites the old owner's references in the
--    ACL, so the grants end up recorded as issued by ledger_owner, which is
--    what they will look like to anyone reading the schema later.
--
-- 2. REVOKE ALL ON SCHEMA public FROM PUBLIC is the LAST statement. Schema
--    ownership does not imply schema USAGE: `public` is owned by the dynamic
--    pseudo-role pg_database_owner, and while the connecting role is
--    recognised as its administrator for owner-gated actions, that does NOT
--    satisfy the ordinary ACL-gated USAGE check that every statement naming
--    `public.<anything>` has to pass. Revoke PUBLIC's USAGE any earlier and
--    the next statement in this very file fails with "permission denied for
--    schema public" -- the connection locks itself out mid-install.
--
-- ####  The two narrow re-grants  ####
--
-- After the transfer, `schema_migrations` belongs to ledger_owner too, and
-- golang-migrate writes to it with the SAME connection that ran this file --
-- twice per migration, once to mark the version dirty before the SQL and once
-- to mark it clean after. That second write happens after this file's
-- transaction has already committed. Without an explicit grant it fails with
-- "permission denied for table schema_migrations" and golang-migrate reports
-- the whole migration as failed and dirty, even though the DDL committed.
-- Retrying does not help: the same role runs the same file and hits the same
-- wall.
--
-- So the runner keeps exactly two things: USAGE on the schema, and
-- SELECT/INSERT/TRUNCATE on schema_migrations (TRUNCATE because that is how
-- golang-migrate's pgx driver replaces the version row). Every business table
-- stays fully locked out from it.
--
-- The two are granted at different moments, and the difference is not
-- cosmetic:
--
--   USAGE on the schema is granted BEFORE the transfer, while the runner is
--   still the schema's effective administrator.
--
--   The schema_migrations grant has to come AFTER the transfer, and cannot be
--   issued by the runner alone. Granting it beforehand looks like it works and
--   does nothing: the runner already owns the table, so the GRANT folds into
--   the owner's existing ACL entry -- and ALTER ... OWNER TO then rewrites
--   that entry to the new owner, taking the runner's access with it. The grant
--   has to be made by ledger_owner, after ledger_owner owns the table.
--
--   So the runner temporarily upgrades its own ledger_owner membership to
--   WITH INHERIT TRUE (using the ADMIN OPTION Postgres unconditionally gives
--   the creator of a role), issues the grant, and revokes the membership
--   again. `SET ROLE` would be the obvious alternative and is deliberately
--   avoided: it interacts badly with golang-migrate's two version writes
--   bracketing this file.
--
-- ⚠️ Residual capability this install cannot close: that ADMIN OPTION is
-- permanent. Postgres gives a non-superuser no way to strip its own automatic
-- admin option on a role it created, so the bootstrap credential can always
-- repeat the GRANT/REVOKE dance above to regain ledger_owner's privileges. The
-- install prerequisite is therefore also an install-time-only credential: it
-- should be rotated or retired once the ledger is running, and a real
-- superuser can strip the admin option outright.
--
-- ####  What ledger_app is allowed to do  ####
--
-- The rule is derived from each table's own trigger, not from a list someone
-- maintains: a table carrying a BEFORE UPDATE `ledger_block_mutation()`
-- trigger gets SELECT/INSERT, everything else gets SELECT/INSERT/UPDATE.
-- Nobody gets DELETE, anywhere.
--
-- Saying the same thing in the ACL that the trigger already says is
-- deliberate, not belt-and-braces for its own sake: dropping a trigger
-- requires ownership, whereas UPDATE only requires a grant. They are defences
-- against two different bypasses, and the incremental history of this schema
-- contains a real case of one layer silently failing while the tests only
-- exercised the other.
--
-- Deriving it from the trigger rather than a name list is the same lesson as
-- the journals guard in section 12: with a list, the next append-only table
-- gets the wrong ACL and nobody notices. The trigger IS the declaration of
-- intent, so read it.
--
-- schema_migrations is deliberately excluded -- ledger_app has no business
-- reading or writing the applied-migrations ledger.
--
-- ####  ledger_ro  ####
--
-- SELECT on everything. No reporting views exist yet to scope it down to, and
-- full-schema SELECT is still strictly less than the superuser session a BI
-- tool has no business holding. Narrowing this to purpose-built views is
-- tracked as a follow-up.
------------------------------------------------------------------------------

GRANT USAGE ON SCHEMA public TO ledger_owner, ledger_app, ledger_ro;
GRANT CREATE ON SCHEMA public TO ledger_owner;

DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname,
               EXISTS (
                   SELECT 1
                   FROM pg_trigger t
                   WHERE t.tgrelid = c.oid
                     AND t.tgfoid = 'public.ledger_block_mutation()'::regprocedure
                     AND (t.tgtype & 16) <> 0   -- fires on UPDATE
               ) AS append_only
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind IN ('r', 'p')
          AND c.relname <> 'schema_migrations'
    LOOP
        -- Partitions are included on purpose: a GRANT on a partitioned parent
        -- does not reach its partitions, and they carry the parent's cloned
        -- trigger, so the same rule classifies them correctly.
        IF r.append_only THEN
            EXECUTE format('GRANT SELECT, INSERT ON public.%I TO ledger_app', r.relname);
        ELSE
            EXECUTE format('GRANT SELECT, INSERT, UPDATE ON public.%I TO ledger_app', r.relname);
        END IF;
    END LOOP;

    FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('GRANT USAGE, SELECT ON public.%I TO ledger_app', r.sequencename);
    END LOOP;
END $$;

GRANT SELECT ON ALL TABLES IN SCHEMA public TO ledger_ro;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO ledger_ro;

-- Keepsake 1 of 2: schema USAGE, granted while the runner is still the
-- schema's effective administrator. See the header.
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', runner);
END $$;

------------------------------------------------------------
-- Ownership.
--
-- This sweep covers tables, partitions, sequences, views and routines. The
-- routines are the point of listing them out: the incremental version of this
-- step looped over tables and sequences only, so all five guard functions from
-- section 12 stayed owned by whoever happened to run the migration.
--
-- That is not a cosmetic gap. A function's owner can CREATE OR REPLACE its
-- body. Replacing `ledger_block_mutation` with `BEGIN RETURN NEW; END` turns
-- every append-only guarantee in this schema off, silently, and produces no
-- DDL that looks anything like DROP TRIGGER -- the triggers are all still
-- there, still firing, and now doing nothing. Compared with dropping a
-- trigger, it is both quieter and easier to miss in an audit. Function
-- ownership is therefore part of the tamper-evidence story, not an
-- afterthought to it.
--
-- The same omission hit tables too, in a subtler way: a table created by a
-- migration that ran AFTER the ownership sweep never got swept. Someone later
-- noticed those tables were missing their GRANTs and fixed exactly that,
-- without noticing they were also missing their ownership transfer -- the same
-- migration, the same tables, the second half of the same bug. Sweeping the
-- catalogue instead of a list of names is what makes both classes impossible
-- here: this runs after everything is created, and it asks the database what
-- exists rather than trusting anyone's memory.
--
-- The sequence loop skips sequences already owned by ledger_owner. Most are
-- owned by a BIGSERIAL column and were carried across by the table loop
-- already; re-altering one fails under anything short of superuser, because
-- the runner holds SET but deliberately not INHERIT on ledger_owner and so
-- does not satisfy Postgres's ownership-equivalence check once the sequence's
-- owner is already ledger_owner.
------------------------------------------------------------
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO ledger_owner', r.tablename);
    END LOOP;

    FOR r IN SELECT sequencename FROM pg_sequences
             WHERE schemaname = 'public' AND sequenceowner <> 'ledger_owner' LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO ledger_owner', r.sequencename);
    END LOOP;

    -- No views today. Kept so that adding the reporting views ledger_ro is
    -- meant to eventually read cannot reintroduce the gap above.
    FOR r IN SELECT c.relname, c.relkind
             FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relkind IN ('v', 'm') LOOP
        IF r.relkind = 'v' THEN
            EXECUTE format('ALTER VIEW public.%I OWNER TO ledger_owner', r.relname);
        ELSE
            EXECUTE format('ALTER MATERIALIZED VIEW public.%I OWNER TO ledger_owner', r.relname);
        END IF;
    END LOOP;

    FOR r IN SELECT p.oid::regprocedure AS sig
             FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
             WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p') LOOP
        EXECUTE format('ALTER ROUTINE %s OWNER TO ledger_owner', r.sig);
    END LOOP;
END $$;

------------------------------------------------------------
-- Keepsake 2 of 2: golang-migrate's own bookkeeping table. Issued after the
-- transfer, from ledger_owner's privileges, via a temporary membership
-- upgrade. See the header for why it cannot be done any earlier or any more
-- simply.
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('GRANT ledger_owner TO %I WITH INHERIT TRUE', runner);
    EXECUTE format('GRANT SELECT, INSERT, TRUNCATE ON public.schema_migrations TO %I', runner);
    EXECUTE format('REVOKE ledger_owner FROM %I', runner);
END $$;

------------------------------------------------------------
-- Anything the runner creates from here on also grants full rights to
-- ledger_owner automatically, so a future migration run under the bootstrap
-- credential does not silently create an object ledger_owner cannot touch.
--
-- Deliberately the only default-privilege entry: ledger_app and ledger_ro get
-- nothing automatically on new objects. A new table has to name them in an
-- explicit, reviewable GRANT -- or, better, be covered by re-running the
-- trigger-derived loop above.
------------------------------------------------------------
DO $$
DECLARE runner text := current_user;
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT ALL ON TABLES TO ledger_owner', runner);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT ALL ON SEQUENCES TO ledger_owner', runner);
END $$;

------------------------------------------------------------
-- Last statement in the file. See the header for why it has to be last.
------------------------------------------------------------
REVOKE ALL ON SCHEMA public FROM PUBLIC;
