-- name: CreateUserInvite :one
-- R-21：冗余被邀请人 openid（注销后行保留，作为被邀请奖励终身一次的判定依据）。
INSERT INTO user_invites (id, user_id, inviter_id, entry_count, user_open_id, created_at)
VALUES ($1, $2, $3, 0, $4, now())
RETURNING *;

-- name: GetUserInviteByUserID :one
SELECT * FROM user_invites WHERE user_id = $1;

-- name: ListUserInvitesByInviter :many
SELECT ui.user_id, u.nickname, u.avatar,
       (u.current_family_id IS NOT NULL
        AND u.current_family_id != u.personal_family_id
        AND u.current_family_id = inv.current_family_id) AS joined
FROM user_invites ui
JOIN users u ON u.id = ui.user_id
JOIN users inv ON inv.id = ui.inviter_id
WHERE ui.inviter_id = $1
ORDER BY ui.created_at DESC
LIMIT 100;

-- name: MarkInviteeRewarded :execrows
UPDATE user_invites
SET reward_invitee_at = now()
WHERE id = $1 AND reward_invitee_at IS NULL;

-- name: MarkInviterRewarded :execrows
UPDATE user_invites
SET reward_inviter_at = now()
WHERE id = $1 AND reward_inviter_at IS NULL;

-- name: CountInviterMonthlyRewardDays :one
SELECT COALESCE(COUNT(*) * 7, 0)::int AS total_days
FROM user_invites
WHERE inviter_id = $1
  AND reward_inviter_at >= $2
  AND reward_inviter_at < $3;

-- name: LockInviterReward :exec
SELECT pg_advisory_xact_lock(hashtext('inviter_reward:' || $1));

-- name: CreateUserInviteCode :one
INSERT INTO user_invite_codes (user_id, short_code, created_at)
VALUES ($1, $2, now())
RETURNING *;
-- name: GetUserInviteCode :one
SELECT short_code FROM user_invite_codes WHERE user_id = $1;

-- name: ResolveInviterFromCode :one
-- 邀请码是邀请人的稳定分享码，可被多个被邀请人多次解析（不限制一次性），
-- 过期语义由 user_invite_codes.expires_at（baseline 000001）决定：过期后解析失败。
-- R-24：解析为纯读（used_at 死遥测写副作用移除；列保留，将来做过期策略再启用）。
SELECT user_id
FROM user_invite_codes
WHERE short_code = $1
  AND (expires_at IS NULL OR expires_at > now());

-- name: DeleteUserInviteCodeByUserID :exec
DELETE FROM user_invite_codes WHERE user_id = $1;