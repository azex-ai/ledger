-- Inbound webhook replay cache. VerifySignature's timestamp window (±5 min)
-- only rejects STALE replays; a captured request replayed inside the window
-- verifies fine and previously relied entirely on downstream Transition
-- idempotency. Recording each seen signature closes that gap at the HTTP
-- boundary. This is a cache, not ledger data: rows older than the replay
-- window are deleted opportunistically (the one sanctioned DELETE — nothing
-- financial lives here).
CREATE TABLE IF NOT EXISTS webhook_nonces (
    nonce   TEXT PRIMARY KEY,
    seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_nonces_seen ON webhook_nonces (seen_at);
