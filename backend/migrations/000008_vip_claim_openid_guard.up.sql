-- R-21 注销循环权益收口：trial/free 领取与被邀请奖励改按微信主体（open_id）终身一次。
-- 背景：注销为物理删除，同一微信重注册得新 user_id，原 (user_id) 维度防重被绕过。
-- 方案：发放记录冗余 open_id（历史回填），DB 层部分唯一索引兜底（I1/I11）；
--       user_id FK 改 SET NULL，注销后保留仅含 openid+vip_id 的"领取墓碑"行（无业务数据）。

-- 1) user_vip_claims：补 open_id 列 + 回填 + 同一 openid 同一 vip 终身一次
ALTER TABLE public.user_vip_claims ADD COLUMN open_id text;

UPDATE public.user_vip_claims c
SET open_id = u.open_id
FROM public.users u
WHERE c.user_id = u.id
  AND c.open_id IS NULL;

CREATE UNIQUE INDEX uq_user_vip_claims_open_id_vip_id
    ON public.user_vip_claims (open_id, vip_id)
    WHERE open_id IS NOT NULL;

ALTER TABLE public.user_vip_claims DROP CONSTRAINT user_vip_claims_user_id_fkey;
ALTER TABLE public.user_vip_claims
    ADD CONSTRAINT user_vip_claims_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;

-- 2) user_invites：记录被邀请人 openid，被邀请奖励（reward_invitee_at）终身一次
ALTER TABLE public.user_invites ADD COLUMN user_open_id text;

UPDATE public.user_invites i
SET user_open_id = u.open_id
FROM public.users u
WHERE i.user_id = u.id
  AND i.user_open_id IS NULL;

-- 防御：历史数据中同一 openid 已有多条被邀请奖励时，保留最早一条标记，其余清空（仅影响唯一性记账，不追溯权益）。
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY user_open_id ORDER BY reward_invitee_at ASC, created_at ASC) AS rn
    FROM public.user_invites
    WHERE user_open_id IS NOT NULL
      AND reward_invitee_at IS NOT NULL
)
UPDATE public.user_invites i
SET reward_invitee_at = NULL
FROM ranked r
WHERE i.id = r.id
  AND r.rn > 1;

CREATE UNIQUE INDEX uq_user_invites_user_open_id
    ON public.user_invites (user_open_id)
    WHERE user_open_id IS NOT NULL
      AND reward_invitee_at IS NOT NULL;

-- 被邀请人行在注销后保留（终身一次的判定依据）：user_id 允许 NULL、FK 改 SET NULL。
ALTER TABLE public.user_invites ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE public.user_invites DROP CONSTRAINT user_invites_user_id_fkey;
ALTER TABLE public.user_invites
    ADD CONSTRAINT user_invites_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;
