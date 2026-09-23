-- 可逆：仅移除约束；已由 CASCADE 联动删除的行无法恢复（与约束语义一致）。
ALTER TABLE public.family_daily_covers
    DROP CONSTRAINT IF EXISTS family_daily_covers_family_id_fkey;
