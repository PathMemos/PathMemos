-- 死列清理第一步（expand-only）：api_keys.expires_at 补 DEFAULT，
-- 使代码可停止显式写入该列（值恒为 9999-12-31、认证不判过期，01 §6.2 待办）。
-- 列与 idx_api_keys_expires_at 的 DROP 在第二步（待本步代码上线后）执行。
ALTER TABLE public.api_keys
    ALTER COLUMN expires_at SET DEFAULT '9999-12-31 23:59:59+00';
