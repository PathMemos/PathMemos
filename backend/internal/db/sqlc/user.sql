-- name: GetUserByOpenID :one
SELECT id, open_id, unionid, phone_number, avatar, avatar_file_id, nickname, user_type, phone_bind_time, auto_record_enabled, personal_family_id, current_family_id, invited_by, lang, created_at, updated_at, abnormal_subscribe_accepted, last_active_at FROM users WHERE open_id = $1;

-- name: GetUserByUnionID :one
SELECT id, open_id, unionid, phone_number, avatar, avatar_file_id, nickname, user_type, phone_bind_time, auto_record_enabled, personal_family_id, current_family_id, invited_by, lang, created_at, updated_at, abnormal_subscribe_accepted, last_active_at FROM users WHERE unionid = $1;

-- name: GetUserByID :one
SELECT id, open_id, unionid, phone_number, avatar, avatar_file_id, nickname, user_type, phone_bind_time, auto_record_enabled, personal_family_id, current_family_id, invited_by, lang, created_at, updated_at, abnormal_subscribe_accepted, last_active_at FROM users WHERE id = $1;

-- name: GetUserByIDForUpdate :one
SELECT id, open_id, unionid, phone_number, avatar, avatar_file_id, nickname, user_type, phone_bind_time, auto_record_enabled, personal_family_id, current_family_id, invited_by, lang, created_at, updated_at, abnormal_subscribe_accepted, last_active_at FROM users WHERE id = $1 FOR UPDATE;

-- name: GetUserSessionKeyByID :one
SELECT session_key FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (
    id, open_id, unionid, phone_number, avatar, avatar_file_id, nickname,
    user_type, phone_bind_time, auto_record_enabled, session_key,
    personal_family_id, current_family_id, invited_by, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now(), now())
RETURNING *;

-- name: UpdateUserSessionKey :exec
UPDATE users SET session_key = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserUnionID :exec
UPDATE users SET unionid = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserAvatar :exec
UPDATE users SET avatar = $2, avatar_file_id = $3, updated_at = now() WHERE id = $1;

-- name: UpdateUserAvatarIfEmpty :execrows
-- 仅当 avatar 仍为空时写入：注册后异步默认头像回写不覆盖登录链路已写入的微信头像（02a A-9）。
UPDATE users SET avatar = $2, avatar_file_id = $3, updated_at = now()
WHERE id = $1 AND (avatar IS NULL OR avatar = '');

-- name: UpdateUserNickname :exec
UPDATE users SET nickname = $2, updated_at = now() WHERE id = $1;

-- name: BindUserPhoneIfAllowed :execrows
-- A-FIX-03：绑定手机号的原子日限——仅当 phone_bind_time 为空或不在今天（上海时区）时写入，
-- 避免「检查-再更新」竞态下并发绑定绕过日限、反复消耗微信认证额度。
UPDATE users
SET phone_number = $2, phone_bind_time = $3, updated_at = now()
WHERE id = $1
  AND (phone_bind_time IS NULL OR (phone_bind_time AT TIME ZONE 'Asia/Shanghai')::date <> sqlc.arg(today)::date);

-- name: UpdateUserInvitedBy :execrows
-- 仅当尚无邀请人时写入（登录后补绑场景的幂等闸门）。
UPDATE users SET invited_by = $2, updated_at = now() WHERE id = $1 AND (invited_by IS NULL OR invited_by = '');

-- name: UpdateUserCurrentFamily :exec
UPDATE users SET current_family_id = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUsersCurrentFamily :exec
UPDATE users SET current_family_id = $2, updated_at = now() WHERE id = ANY($1::text[]);

-- name: BatchUpdateUsersCurrentFamilyToPersonal :exec
UPDATE users u SET current_family_id = p.personal_family_id, updated_at = now()
FROM (
    SELECT unnest(@user_ids::text[]) AS user_id, unnest(@personal_family_ids::text[]) AS personal_family_id
) AS p
WHERE u.id = p.user_id;

-- name: UpdateUserPersonalFamily :exec
UPDATE users SET personal_family_id = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserLang :exec
UPDATE users SET lang = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserAutoRecord :exec
UPDATE users SET auto_record_enabled = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserAlertSubscribe :exec
UPDATE users SET
    abnormal_subscribe_accepted = $2,
    updated_at = now()
WHERE id = $1;

-- name: UpdateUserLastActiveAt :exec
UPDATE users SET last_active_at = now(), updated_at = now() WHERE id = $1;

-- name: MarkAbnormalAlertSent :execrows
UPDATE users
SET abnormal_alert_sent_at = now(),
    updated_at = now()
WHERE id = $1
  AND (
      abnormal_alert_sent_at IS NULL
      OR (abnormal_alert_sent_at AT TIME ZONE 'Asia/Shanghai')::date < (now() AT TIME ZONE 'Asia/Shanghai')::date
  );


-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: GetUserImageStorageUsage :one
SELECT image_storage_bytes FROM users WHERE id = $1;

-- name: IncrementUserImageStorage :execrows
-- 条件原子扣减——超限时更新 0 行，由调用方识别拒绝，杜绝 check-then-act 竞态。
UPDATE users
SET image_storage_bytes = image_storage_bytes + $2,
    updated_at = now()
WHERE id = $1
  AND (sqlc.arg(storage_limit)::bigint <= 0 OR image_storage_bytes + $2 <= sqlc.arg(storage_limit));

-- name: DecrementUserImageStorage :exec
-- GREATEST 保底：并发/重复扣减时计量不得为负（负值会绕过存储限额检查）。
UPDATE users
SET image_storage_bytes = GREATEST(image_storage_bytes - $2, 0),
    updated_at = now()
WHERE id = $1;

-- name: UpsertUserAvatarMarker :exec
INSERT INTO user_avatar_markers (user_id, marker_path, storage_type, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id)
DO UPDATE SET
    marker_path = EXCLUDED.marker_path,
    storage_type = EXCLUDED.storage_type,
    updated_at = now();

-- name: GetUserAvatarMarker :one
SELECT marker_path, storage_type FROM user_avatar_markers WHERE user_id = $1;

-- name: GetUserAvatarMarkersByIDs :many
SELECT user_id, marker_path, storage_type FROM user_avatar_markers WHERE user_id = ANY($1::text[]);
