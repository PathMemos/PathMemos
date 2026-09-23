-- 可逆：恢复 NOT NULL。若领取墓碑行（user_id IS NULL）已存在，需先人工清理：
--   DELETE FROM user_vip_claims WHERE user_id IS NULL;
--（删除墓碑意味着放弃注销防重历史。）
ALTER TABLE public.user_vip_claims ALTER COLUMN user_id SET NOT NULL;
