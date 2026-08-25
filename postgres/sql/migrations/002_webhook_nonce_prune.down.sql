-- Revoke the replay cache's DELETE.
--
-- After this, webhook_nonces grows without bound under ledger_app: the prune
-- is best-effort (postgres.WebhookSubscriberStore.TryRecordNonce) so inbound
-- webhooks keep working, but nothing removes expired rows. That is the
-- deliberate shape of this rollback -- it restores 001_baseline's blanket
-- "no DELETE anywhere" and accepts the growth, rather than restoring the
-- breakage that made this migration necessary.
REVOKE DELETE ON public.webhook_nonces FROM ledger_app;
