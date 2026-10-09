#!/usr/bin/env bash
# 生产备份离机拉取。
#
# 背景：小时级备份（db_*.sql.gz）与主库同宿主机，防数据损坏但不防整机灭失；
# 本脚本让控制机每日拉取服务器最新一份 db 备份到独立子目录 daily-pull/
# （与部署前 dump 的 papafeiji-backup-*.dump 分开，避开其 7 天 mtime 清理逻辑），
# 整机灭失 RPO 由「距上次部署」收敛到 ≤24h。
#
# 用法（控制机 cron 建议）：15 4 * * * /root/Github/PathMemos-SaaS/scripts/pull-backup.sh >> /var/log/papafeiji-pull-backup.log 2>&1
# 服务器最新备份 mtime 超 25 小时（备份容器异常）时响亮告警（stderr + 退出码 2），不覆盖已拉取件。
set -euo pipefail

# ssh 别名跟随生产（别名见控制机 ~/.ssh/config）；
# 注释不得写进变量赋值的引号内——注释会被并入 REMOTE 值，导致每日拉取静默失败
REMOTE="${PAPAFEIJI_REMOTE:-papafeiji}"
REMOTE_DIR="${PAPAFEIJI_DIR:-/opt/papafeiji}"
DEST="${PULL_BACKUP_DEST:-/root/DeployOps/papafeiji-db-backups/daily-pull}"
# 1500 分钟 = 25 小时（小时备份的正常节律为 60 分钟）
STALE_MINUTES=1500

mkdir -p "${DEST}"

latest="$(ssh "${REMOTE}" "ls -1t ${REMOTE_DIR}/backups/db_*.sql.gz 2>/dev/null | head -1 || true")"
if [[ -z "${latest}" ]]; then
  echo "[$(date -Is)] ERROR: 服务器 ${REMOTE} ${REMOTE_DIR}/backups/ 下无 db_*.sql.gz 备份可拉取" >&2
  exit 1
fi
fname="$(basename "${latest}")"

if [[ -f "${DEST}/${fname}" ]]; then
  echo "[$(date -Is)] OK: ${fname} 已是最新，跳过拉取"
else
  scp -q "${REMOTE}:${latest}" "${DEST}/${fname}"
  # 拉取成功后仅保留最近 7 份，防控制机磁盘被慢性吃满
  ls -1t "${DEST}"/db_*.sql.gz 2>/dev/null | tail -n +8 | xargs -r rm -f --
  echo "[$(date -Is)] OK: 已拉取 ${fname} -> ${DEST}/"
fi

# 新鲜度校验：服务器侧最新备份超 25h 未更新说明备份容器异常（本地件不回退覆盖）
if [[ -n "$(ssh "${REMOTE}" "find '${latest}' -mmin +${STALE_MINUTES} 2>/dev/null")" ]]; then
  echo "[$(date -Is)] ERROR alert=backup_stale_remote: 服务器最新备份 ${fname} 超 25h 未更新，备份容器疑似异常" >&2
  exit 2
fi
