-- families.removed_members：{userID: 移除时间(RFC3339)} 记录被 owner 移出的成员，
-- 供 /family/invite-link/join 做 7 天冷却检查（防止被移除者经链接循环重入，ADR-0019）。
ALTER TABLE public.families
    ADD COLUMN removed_members jsonb DEFAULT '{}'::jsonb NOT NULL;
