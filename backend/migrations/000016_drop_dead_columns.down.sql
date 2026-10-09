-- 可逆重建：值均为恒值/空，语义可完整恢复（api_keys.expires_at 恒 9999-12-31；
-- user_invite_codes.used_at 恒 NULL——000013 起短码永久、解析纯读）。
CREATE INDEX idx_api_keys_expires_at ON public.api_keys USING btree (expires_at);
ALTER TABLE public.api_keys ADD COLUMN expires_at timestamp with time zone NOT NULL DEFAULT '9999-12-31 23:59:59+00';
ALTER TABLE public.user_invite_codes ADD COLUMN used_at timestamp with time zone;
