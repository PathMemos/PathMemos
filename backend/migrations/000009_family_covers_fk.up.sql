-- R-25：family_daily_covers.family_id 补外键（此前一致性完全依赖三条删除路径的人工纪律，
-- 未来新增删家庭路径一旦遗漏将产生永久孤儿行且无清理任务兜底）。
-- 先清存量孤儿（历史上不应存在，防御性删除），再加 ON DELETE CASCADE。
DELETE FROM public.family_daily_covers c
WHERE NOT EXISTS (SELECT 1 FROM public.families f WHERE f.id = c.family_id);

ALTER TABLE public.family_daily_covers
    ADD CONSTRAINT family_daily_covers_family_id_fkey
    FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;
