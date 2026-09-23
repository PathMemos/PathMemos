-- 可逆：删除扩展（postgres command 中的 shared_preload_libraries 可保留，无副作用）。
DROP EXTENSION IF EXISTS pg_stat_statements;
