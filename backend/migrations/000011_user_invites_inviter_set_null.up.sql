-- R-27（第三轮评审 N-07(3)）：被邀请人 openid 墓碑行与邀请人存续解耦。
-- 原 inviter_id 为 ON DELETE CASCADE：邀请人注销会连带删除 user_invites（含被邀请奖励
-- 墓碑行），被邀请人重注册后可绕过 uq_user_invites_user_open_id 再领 +3，「每微信主体
-- 终身一次」失效。与 R-21 对 user_vip_claims.user_id（000008）的处理对齐，改为 SET NULL。
-- inviter_id 置 NULL 的行在按 inviter 统计/查询中自然不命中（与 CASCADE 对统计行为等价）。
ALTER TABLE public.user_invites ALTER COLUMN inviter_id DROP NOT NULL;
ALTER TABLE public.user_invites DROP CONSTRAINT user_invites_inviter_id_fkey;
ALTER TABLE public.user_invites
    ADD CONSTRAINT user_invites_inviter_id_fkey FOREIGN KEY (inviter_id) REFERENCES public.users(id) ON DELETE SET NULL;
