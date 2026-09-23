#!/usr/bin/env bash
# 迁移预演（控制机侧）：把最近一次部署备份恢复进一次性 Postgres 容器，对"生产形状的数据"
# 执行本分支的全部正向迁移，验证迁移可跑通后再上生产。不触碰生产环境。
# 用法: scripts/rehearse-migration.sh [备份.dump]   # 缺省取 /root/DeployOps/papafeiji-db-backups 最新一份
# 退出码: 0 预演通过；非 0 迁移或恢复失败。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BACKUP="${1:-$(ls -1t /root/DeployOps/papafeiji-db-backups/papafeiji-backup-*.dump 2>/dev/null | head -1 || true)}"
CONTAINER="papafeiji-rehearse"
PORT="${PAPAFEIJ_REHEARSE_PORT:-54329}"
PGPASSWORD_REHEARSE="rehearse-only"

[[ -n "${BACKUP}" && -f "${BACKUP}" ]] || { echo "错误：未找到备份文件（可传参指定 .dump）" >&2; exit 1; }
command -v pg_restore >/dev/null 2>&1 || { echo "错误：本机缺少 pg_restore" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "错误：本机缺少 docker" >&2; exit 1; }
echo "==> 预演备份：${BACKUP}"

cleanup() { docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
docker run -d --name "${CONTAINER}" -p "127.0.0.1:${PORT}:5432" \
  -e POSTGRES_USER=papafeiji -e POSTGRES_PASSWORD="${PGPASSWORD_REHEARSE}" -e POSTGRES_DB=papafeiji \
  postgres:15-alpine >/dev/null

echo "==> 等待临时 PostgreSQL 就绪..."
for _ in $(seq 1 30); do
  if docker exec "${CONTAINER}" pg_isready -U papafeiji -d papafeiji >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "${CONTAINER}" pg_isready -U papafeiji -d papafeiji >/dev/null || { echo "错误：临时库未就绪" >&2; exit 1; }

echo "==> 恢复备份（约需数十秒到数分钟）..."
pg_restore -h 127.0.0.1 -p "${PORT}" -U papafeiji -d papafeiji --no-owner --role=papafeiji "${BACKUP}"

echo "==> 渲染迁移占位符（哑值，仅验证可执行性）..."
REHEARSE_DIR="$(mktemp -d)"
cp "${PROJECT_ROOT}/backend/migrations/"*.sql "${REHEARSE_DIR}/"
sed -i \
  -e "s#{{API_HOST}}#https://rehearse.invalid#g" \
  -e "s#{{OSS_PUBLIC_URL}}#https://rehearse.invalid/oss#g" \
  -e "s#{{AI_BASE_URL}}#https://rehearse.invalid#g" \
  -e "s#{{AI_MODEL}}#rehearse#g" \
  -e "s#{{CFG_VIRTUAL_PAY_PRODUCT_ID_MONTH}}#rehearse-month#g" \
  -e "s#{{CFG_VIRTUAL_PAY_PRODUCT_ID_YEAR}}#rehearse-year#g" \
  "${REHEARSE_DIR}"/*.sql

MIGRATE_BIN="${GOPATH:-$HOME/go}/bin/migrate"
if [[ ! -x "${MIGRATE_BIN}" ]]; then
  echo "==> 编译 migrate（本机架构）..."
  go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.17.0
fi

echo "==> 执行正向迁移..."
DATABASE_URL="postgres://papafeiji:${PGPASSWORD_REHEARSE}@127.0.0.1:${PORT}/papafeiji?sslmode=disable" \
  "${MIGRATE_BIN}" -path "${REHEARSE_DIR}" -database "${DATABASE_URL}" up
echo "==> 迁移后版本：$("${MIGRATE_BIN}" -path "${REHEARSE_DIR}" -database "${DATABASE_URL}" version | awk '{print $1}')"
echo "==> 预演通过：备份 ${BACKUP} 上全部迁移可执行。"
