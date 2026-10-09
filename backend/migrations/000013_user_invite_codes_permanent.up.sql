-- 个人邀请短码是用户的稳定分享标识（分享卡二维码/邀请链接长期有效），
-- 不应过期：存量行的 7 天过期语义（baseline 000001）已由"新码不再写 expires_at"
-- 取代，此处把存量过期码统一转为永久，否则扫码解析 404（过期码被画进分享卡
-- 的真实案例：ZEWER5TN 于 2026-10-08 过期致日记分享卡扫码报"邀请码无效"）。
UPDATE user_invite_codes SET expires_at = NULL WHERE expires_at IS NOT NULL;
