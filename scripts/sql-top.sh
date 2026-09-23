#!/usr/bin/env bash
# 慢 SQL 榜单（服务器侧执行）：按总耗时输出 pg_stat_statements Top N。
# 前置：postgres 已启用 shared_preload_libraries=pg_stat_statements（deploy.sh 渲染），
#       且迁移 000007 已创建扩展。首次部署新参数后需等待 postgres 容器重建生效。
# 用法: 在生产服务器上执行  bash /opt/papafeiji/src/scripts/sql-top.sh [条数=20]
set -euo pipefail

cd /opt/papafeiji
LIMIT="${1:-20}"
docker compose exec -T postgres psql -U papafeiji -d papafeiji -P pager=off -c "
SELECT round(total_exec_time::numeric, 1) AS total_ms,
       calls,
       round(mean_exec_time::numeric, 2) AS mean_ms,
       round(rows::numeric, 0) AS rows,
       left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 100) AS query
FROM pg_stat_statements
WHERE query NOT LIKE 'BEGIN%' AND query NOT LIKE 'COMMIT%'
ORDER BY total_exec_time DESC
LIMIT ${LIMIT};"
