#!/bin/bash
set -euo pipefail

# PathMemos 数据库备份恢复脚本（R4）
# 用途：迁移 squash 后无法用 migrate goto 回滚，schema 级恢复统一走本脚本。
#
# 用法：./scripts/restore-backup.sh <备份文件> [--yes]
# 支持格式：
#   *.dump    —— pg_dump 自定义格式（-Fc），用 pg_restore 恢复
#   *.sql.gz  —— pg_dump 纯文本 + gzip，用 psql 恢复
#   *.sql     —— pg_dump 纯文本，用 psql 恢复
#
# 在部署目录（含 docker compose 与 postgres 容器）所在机器执行：
#   - 生产服务器：/opt/papafeiji/backups/pre-squash-*.sql.gz（deploy.sh 自动生成）
#   - 控制机本地：/root/DeployOps/papafeiji-db-backups/*.dump（deploy.sh 每次部署前自动备份）
#
# 安全措施：恢复前自动再做一次安全备份；默认需要 --yes 确认。

YES=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes) YES=true; shift ;;
    -*) echo "未知参数: $1" >&2; exit 1 ;;
    *) BACKUP_FILE="$1"; shift ;;
  esac
done

if [[ -z "${BACKUP_FILE:-}" ]]; then
  echo "用法：$0 <备份文件> [--yes]" >&2
  exit 1
fi
if [[ ! -f "$BACKUP_FILE" ]]; then
  echo "错误：备份文件不存在: $BACKUP_FILE" >&2
  exit 1
fi

DB_CONTAINER="$(docker compose ps -q postgres 2>/dev/null || true)"
if [[ -z "$DB_CONTAINER" ]]; then
  echo "错误：找不到运行中的 postgres 容器（请在部署目录下执行本脚本）" >&2
  exit 1
fi

echo "将恢复：$BACKUP_FILE"
echo "目标库：papafeiji（容器 $DB_CONTAINER）"
if [[ "$YES" != "true" ]]; then
  read -r -p "确认恢复？这会覆盖当前数据库内容 [y/N]: " CONFIRM
  [[ "${CONFIRM:-}" == "y" || "${CONFIRM:-}" == "Y" ]] || { echo "已取消"; exit 0; }
fi

mkdir -p backups
SAFETY_BACKUP="backups/restore-safety-$(date +%Y%m%d%H%M%S).sql.gz"
echo "恢复前安全备份 -> $SAFETY_BACKUP"
docker exec "$DB_CONTAINER" pg_dump -U papafeiji papafeiji | gzip > "$SAFETY_BACKUP"

# 恢复期间停掉依赖数据库的应用容器，避免写入冲突（不存在的服务忽略）。
echo "停止应用容器..."
docker compose stop app sse 2>/dev/null || true

restore_failed=0
case "$BACKUP_FILE" in
  *.dump)
    echo "pg_restore -Fc 恢复中..."
    cat "$BACKUP_FILE" | docker exec -i "$DB_CONTAINER" pg_restore -U papafeiji -d papafeiji --clean --if-exists --no-owner || restore_failed=1
    ;;
  *.sql.gz)
    echo "psql 恢复中（gzip 纯文本）..."
    gunzip -c "$BACKUP_FILE" | docker exec -i "$DB_CONTAINER" psql -U papafeiji -d papafeiji -v ON_ERROR_STOP=1 || restore_failed=1
    ;;
  *.sql)
    echo "psql 恢复中（纯文本）..."
    cat "$BACKUP_FILE" | docker exec -i "$DB_CONTAINER" psql -U papafeiji -d papafeiji -v ON_ERROR_STOP=1 || restore_failed=1
    ;;
  *)
    echo "错误：不支持的备份格式（仅支持 .dump / .sql.gz / .sql）" >&2
    restore_failed=1
    ;;
esac

if [[ "$restore_failed" -ne 0 ]]; then
  echo "错误：恢复失败！安全备份保存在 $SAFETY_BACKUP，可再次执行本脚本恢复。" >&2
  echo "应用容器保持停止，避免对半恢复的库写入；请人工检查后重试。" >&2
  exit 1
fi

echo "启动应用容器..."
docker compose start app sse 2>/dev/null || true

echo "恢复完成。"