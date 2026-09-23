-- 可逆：恢复 CASCADE 与 NOT NULL（若墓碑行已存在，SET NOT NULL 会失败——需先人工清理
-- user_id IS NULL 的墓碑行；删除墓碑意味着放弃注销防重历史）。
ALTER TABLE public.user_invites DROP CONSTRAINT user_invites_user_id_fkey;
ALTER TABLE public.user_invites
    ADD CONSTRAINT user_invites_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE public.user_invites ALTER COLUMN user_id SET NOT NULL;
DROP INDEX IF EXISTS uq_user_invites_user_open_id;
ALTER TABLE public.user_invites DROP COLUMN IF EXISTS user_open_id;

ALTER TABLE public.user_vip_claims DROP CONSTRAINT user_vip_claims_user_id_fkey;
ALTER TABLE public.user_vip_claims
    ADD CONSTRAINT user_vip_claims_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
DROP INDEX IF EXISTS uq_user_vip_claims_open_id_vip_id;
ALTER TABLE public.user_vip_claims DROP COLUMN IF EXISTS open_id;
