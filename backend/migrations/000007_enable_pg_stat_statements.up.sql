-- R-20/B1：启用 pg_stat_statements 供慢 SQL 榜单（scripts/sql-top.sh）。
-- 前置：postgres command 已加 shared_preload_libraries=pg_stat_statements（deploy.sh 渲染）；
-- 扩展创建不依赖 preload，但统计需 preload 生效后才有数据。
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
