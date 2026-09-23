#!/usr/bin/env bash
# 数据库连接预算硬门槛：两容器常态连接 2×(请求池上限 + 后台池上限) 必须 ≤ 170。
# 请求池上限 DB_MAX_CONNS、后台池上限 DB_BG_MAX_CONNS 均取自 override 文件（缺省 75 / 10）。
# max_connections=200，固定 reserve ≥ 30（迁移/备份/运维 CLI/advisory lock 持锁）。
# 用法: scripts/check-conn-budget.sh [override.yml]
set -euo pipefail
OVERRIDE="${1:-deploy/docker-compose.override.yml}"
HARD_LIMIT="${DB_BUDGET_HARD_LIMIT:-170}"
if [[ ! -f "${OVERRIDE}" ]]; then
  echo "错误：找不到 ${OVERRIDE}" >&2
  exit 2
fi
DB_MAX_CONNS_EFFECTIVE=$(grep -oE 'DB_MAX_CONNS:[[:space:]]*"?[0-9]+' "${OVERRIDE}" | grep -oE '[0-9]+' | sort -un | tail -1)
DB_MAX_CONNS_EFFECTIVE="${DB_MAX_CONNS_EFFECTIVE:-75}"
DB_BG_MAX_CONNS_EFFECTIVE=$(grep -oE 'DB_BG_MAX_CONNS:[[:space:]]*"?[0-9]+' "${OVERRIDE}" | grep -oE '[0-9]+' | sort -un | tail -1)
DB_BG_MAX_CONNS_EFFECTIVE="${DB_BG_MAX_CONNS_EFFECTIVE:-10}"
DB_BUDGET=$(( 2 * (DB_MAX_CONNS_EFFECTIVE + DB_BG_MAX_CONNS_EFFECTIVE) ))
if (( DB_BUDGET > HARD_LIMIT )); then
  echo "错误：DB_MAX_CONNS=${DB_MAX_CONNS_EFFECTIVE} + DB_BG_MAX_CONNS=${DB_BG_MAX_CONNS_EFFECTIVE} 时两容器常态连接为 ${DB_BUDGET}，超过硬门槛 ${HARD_LIMIT}（max_connections=200，reserve≥30）；不得单独调大，须同时提高 max_connections" >&2
  exit 1
fi
echo "==> 数据库连接预算校验通过：2×(${DB_MAX_CONNS_EFFECTIVE}+${DB_BG_MAX_CONNS_EFFECTIVE})=${DB_BUDGET} ≤ ${HARD_LIMIT}"
