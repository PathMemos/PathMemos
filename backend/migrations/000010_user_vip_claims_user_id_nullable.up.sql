-- 第三步逐文件扫描发现（000008 遗漏）：user_vip_claims.user_id 改为 FK ON DELETE SET NULL
-- 时未同步 DROP NOT NULL，导致注销（DELETE FROM users）触发 23502、注销事务失败。
-- 本迁移补齐 nullable，使"领取墓碑行"真正可保留（与 user_invites 侧的 000008 处理对齐）。
ALTER TABLE public.user_vip_claims ALTER COLUMN user_id DROP NOT NULL;
