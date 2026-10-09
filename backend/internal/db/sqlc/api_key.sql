-- name: CreateAPIKey :one
INSERT INTO api_keys (id, user_id, key_hash, api_key, created_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id) DO NOTHING
RETURNING id, user_id, key_hash, api_key, created_at;

-- name: GetAPIKeyByHash :one
SELECT id, user_id, key_hash, api_key, created_at FROM api_keys WHERE key_hash = $1;

-- name: GetAPIKeyByUser :one
SELECT id, user_id, key_hash, api_key, created_at FROM api_keys WHERE user_id = $1;

-- name: DeleteAPIKeyByUser :exec
DELETE FROM api_keys WHERE user_id = $1;

