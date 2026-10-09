-- 死列清理第二步（contract）：第一步代码（停引用 api_keys.expires_at、
-- 恒真过期谓词移除）已在线上运行，执行 drop。
-- api_keys.expires_at 恒 9999-12-31 且认证不判过期（02h D4）；
-- idx_api_keys_expires_at 无任何查询使用；user_invite_codes.used_at 全库无写入点（01 §6.2 待办）。
DROP INDEX IF EXISTS public.idx_api_keys_expires_at;
ALTER TABLE public.api_keys DROP COLUMN IF EXISTS expires_at;
ALTER TABLE public.user_invite_codes DROP COLUMN IF EXISTS used_at;
