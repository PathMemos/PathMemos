-- 可逆：恢复 NOT NULL + CASCADE。若邀请人注销已产生 inviter_id 为 NULL 的墓碑行，
-- 需先人工清理（删除墓碑意味着放弃被邀请奖励「终身一次」判定历史）：
--   DELETE FROM user_invites WHERE inviter_id IS NULL;
ALTER TABLE public.user_invites DROP CONSTRAINT user_invites_inviter_id_fkey;
ALTER TABLE public.user_invites ALTER COLUMN inviter_id SET NOT NULL;
ALTER TABLE public.user_invites
    ADD CONSTRAINT user_invites_inviter_id_fkey FOREIGN KEY (inviter_id) REFERENCES public.users(id) ON DELETE CASCADE;
