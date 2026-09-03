-- Wave 4, remediation contract §7.18 (lead's ruling under Aaron's 2026-09-03
-- mandate; docs/INVARIANTS.md I-65, and the "Not closed: signing the
-- settlement record" paragraph I-49 left behind).
--
-- ####  What this closes  ####
--
-- I-49 established that a gated Reserve may not read its hold from anything
-- the application's own credential can write. It then found that every
-- discharge signal fails that test:
--
--   reservations.status / .settled_amount   ledger_app holds UPDATE, and
--                                          ledger_reservations_guard permits
--                                          exactly the transitions that zero
--                                          a hold (they are the legitimate
--                                          ones)
--   reservation_operation_receipts          append-only since 006, but
--   reservation_settlement_legs             ledger_app must keep INSERT --
--                                          the application writes them -- and
--                                          `INSERT ... ('release', 0)`
--                                          discharges a hold at the same
--                                          one-statement cost as the UPDATE
--                                          (measured as ledger_app over a
--                                          real socket, 2026-09-03)
--
-- So I-49 credited NO discharge at all and trusted only expires_at, at the
-- documented cost of a settled or released reservation holding its full
-- amount until it expires.
--
-- In this threat model the application's credential IS the attacker, which
-- means no trigger, ACL or SECURITY DEFINER function can separate the two:
-- same role, same statement. Exactly two signals escape that, and I-49 names
-- both -- the passage of time (expires_at, already used) and a signature
-- (a key the database credential does not hold). This migration adds the
-- storage for the second one.
--
-- ####  Shape  ####
--
-- Three columns per discharge table, deliberately identical in name, type
-- and default to journals.auth_digest / auth_signature / auth_key_id
-- (001_baseline section 5), so a reader who knows how a signed journal is
-- stored already knows how a signed discharge claim is stored.
--
-- NOT NULL DEFAULT '' rather than nullable, per this schema's no-NULL rule
-- (CLAUDE.md): "empty" means "this claim carries no signature", which
-- postgres.ReserverStore's gate treats as untrusted and therefore as no
-- discharge at all -- the I-49 fallback, reached by every row that predates
-- this migration without any backfill.
--
-- No auth_status column, and that asymmetry with `journals` is deliberate.
-- journals.auth_status exists (001_baseline section 5, migration 051 in the
-- pre-flattening history) because three unsigned cases were
-- indistinguishable there and one of them -- a journal legitimately posted
-- inside a caller's transaction, where there is no safe point to call an
-- Attestor -- must NOT be reported as tamper evidence by the asynchronous
-- verifier that reads the table. Nothing here has that problem: the only
-- consumer of these columns is a synchronous gate whose answer to "no
-- signature" is not "raise an alarm" but "keep holding the funds", which is
-- the correct and safe answer for every unsigned case (no Attestor
-- configured, a claim written before signing was switched on, or a claim
-- written from inside a caller's transaction). Adding a status column would
-- create a fourth writable claim for an attacker to set, buying nothing.
--
-- ####  Privileges  ####
--
-- No GRANT is issued or needed. Both tables carry TABLE-level privileges
-- (reservation_settlement_legs from 001_baseline's grant loop,
-- reservation_operation_receipts from 005, with UPDATE revoked again by
-- 006), and a table-level INSERT/SELECT privilege in PostgreSQL covers every
-- column the table ever grows -- there are no column-level grants on either
-- table to extend (verified against information_schema.column_privileges).
-- The append-only triggers from 006 are likewise column-agnostic: they
-- refuse UPDATE and DELETE outright, so the new columns are write-once along
-- with the rest of the row, which is what makes a stored signature worth
-- checking.
--
-- ALTER TABLE requires table ownership, which the migration runner has for
-- the duration of the run (001_baseline's owner bootstrap + migrate.go's
-- connection-scoped SET ROLE). Both tables are owned by ledger_owner, so no
-- explicit ALTER ... OWNER TO is needed here.

ALTER TABLE reservation_operation_receipts
    ADD COLUMN auth_digest    BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN auth_signature BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN auth_key_id    TEXT  NOT NULL DEFAULT '';

ALTER TABLE reservation_settlement_legs
    ADD COLUMN auth_digest    BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN auth_signature BYTEA NOT NULL DEFAULT ''::bytea,
    ADD COLUMN auth_key_id    TEXT  NOT NULL DEFAULT '';
