-- name: InsertWebhookSubscriber :one
INSERT INTO webhook_subscribers (name, url, secret, filter_class, filter_to_status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetWebhookSubscriber :one
SELECT * FROM webhook_subscribers WHERE id = $1;

-- name: ListActiveWebhookSubscribers :many
SELECT * FROM webhook_subscribers WHERE is_active = true;

-- name: DeleteWebhookSubscriber :exec
DELETE FROM webhook_subscribers WHERE id = $1;

-- name: UpdateWebhookSubscriberDeliveryStatus :exec
UPDATE webhook_subscribers
SET last_status_code = $2, last_error = $3, last_attempt_at = now()
WHERE id = $1;

-- name: TryRecordWebhookNonce :execrows
-- 0 rows affected = nonce already seen inside the retention window = replay.
INSERT INTO webhook_nonces (nonce) VALUES ($1)
ON CONFLICT (nonce) DO NOTHING;

-- name: DeleteExpiredWebhookNonces :exec
-- Retention is 2x the signature timestamp window; anything older can never
-- verify again, so keeping it only bloats the cache.
DELETE FROM webhook_nonces WHERE seen_at < now() - interval '15 minutes';
