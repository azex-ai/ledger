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
