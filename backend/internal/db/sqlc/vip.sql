-- name: ListActivePaidVIPs :many
SELECT * FROM vips
WHERE is_active = true AND type IN ('month', 'year')
ORDER BY sort ASC
LIMIT 100;

-- name: ListActiveFreeVIPs :many
SELECT * FROM vips
WHERE is_active = true AND type = 'free'
ORDER BY sort ASC
LIMIT 100;

-- name: GetVIPByID :one
SELECT * FROM vips WHERE id = $1;

-- name: UpsertUserVIP :one
INSERT INTO user_vips (id, user_id, begin_time, expire_time, created_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id) DO UPDATE SET
    begin_time = EXCLUDED.begin_time,
    -- GREATEST 防止并发/重复下发时缩短已购 VIP 时长（B2-11）
    expire_time = GREATEST(user_vips.expire_time, EXCLUDED.expire_time)
RETURNING *;

-- name: GetUserVIP :one
SELECT * FROM user_vips WHERE user_id = $1;

-- name: GetUserVIPForUpdate :one
SELECT * FROM user_vips WHERE user_id = $1 FOR UPDATE;

-- name: ClaimUserVIPRow :exec
-- 并发首次激活竞态防护：GetUserVIPForUpdate 对不存在的行无法加锁，两个并发首次激活
-- 都走 ErrNoRows 创建路径会各自按 now 计算 expire、GREATEST 只保留较大者导致较小档时长丢失。
-- 先 ON CONFLICT DO NOTHING 占位（expire=now+1s，满足 CHECK (expire_time > begin_time)），
-- 再 FOR UPDATE 重读串行化首次创建。
INSERT INTO user_vips (id, user_id, begin_time, expire_time, created_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id) DO NOTHING;

-- name: DeleteUserVIP :exec
DELETE FROM user_vips WHERE user_id = $1;

-- name: UpsertVIPClaim :execrows
-- R-21：open_id 冗余发放主体；不带目标的 ON CONFLICT DO NOTHING 同时覆盖
-- (user_id, vip_id) 与 (open_id, vip_id) 两级唯一，rowsAffected=0 → 409 已领取。
INSERT INTO user_vip_claims (id, user_id, vip_id, open_id, created_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT DO NOTHING;

-- name: HasVIPClaim :one
SELECT EXISTS(SELECT 1 FROM user_vip_claims WHERE user_id = $1 AND vip_id = $2) AS exists;

-- name: HasVIPClaimByOpenID :one
-- R-21：按微信主体判重（注销重注册后仍能识别已领取），openid 为空时调用方回退 HasVIPClaim。
SELECT EXISTS(SELECT 1 FROM user_vip_claims WHERE open_id = $1 AND vip_id = $2) AS exists;
