-- name: CreateUserInvite :one
-- 冗余被邀请人 openid（注销后行保留，作为被邀请奖励终身一次的判定依据）。
INSERT INTO user_invites (id, user_id, inviter_id, entry_count, user_open_id, created_at)
VALUES ($1, $2, $3, 0, $4, now())
RETURNING *;

-- name: GetUserInviteByUserID :one
SELECT * FROM user_invites WHERE user_id = $1;

-- name: ExistsInviteeRewardByOpenID :one
-- openid 曾领过被邀请奖励（墓碑行，注销后保留，见迁移 000008/000011）则本次邀请
-- 关系整体不发奖：本人 +3 与邀请人 +7 均不发，邀请行照常创建且对邀请人可见，
-- 注册/绑定/加入主流程照常成功。
SELECT EXISTS(
    SELECT 1 FROM user_invites
    WHERE user_open_id = $1
      AND reward_invitee_at IS NOT NULL
);

-- name: ListUserInvitesByInviter :many
-- LEFT JOIN：受邀人注销后行保留（user_id 置 NULL，000011），关系记录仍应在
-- 邀请人"我的邀请"列表可见；昵称/头像为 NULL，由 handler 映射为"已注销"占位。
SELECT ui.user_id, u.nickname, u.avatar,
       (u.current_family_id IS NOT NULL
        AND u.current_family_id != u.personal_family_id
        AND u.current_family_id = inv.current_family_id) AS joined
FROM user_invites ui
LEFT JOIN users u ON u.id = ui.user_id
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
-- 邀请码是邀请人的稳定分享码，可被多个被邀请人多次解析（不限制一次性）。
-- 解析为纯读；短码永久有效（expires_at 恒 NULL，为重启过期策略预留）。
SELECT user_id
FROM user_invite_codes
WHERE short_code = $1;

-- name: DeleteUserInviteCodeByUserID :exec
DELETE FROM user_invite_codes WHERE user_id = $1;