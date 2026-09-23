#!/usr/bin/env bash
# 服务器侧告警扫描入口（cron 每 2 分钟，由 deploy.sh 自动装配）：
# 汇总 docker 容器日志与主机告警日志，喂给 alert-watch.sh 做 关键字匹配 → webhook。
# 主机侧脚本（watchdog/cert-check）写的是宿主机日志文件，容器日志扫描不到，
# 由本脚本一并纳入扫描面；ALERT_WEBHOOK_URL 从 /opt/papafeiji/.env 读取。
#
# 增量读取（防旧告警无限重推）：主机日志文件只喂"上次扫描之后新增的行"——
# 此前用 tail -N，而这些日志仅在异常时写入、几乎不增长，恢复后旧行会长期留在
# tail 窗口内，叠加 alert-watch 的 5 分钟去重过期，导致同一告警每 5 分钟重推。
# offset 状态存 /var/lib/papafeiji/alert-cron-offsets/<file>.offset：
#   - 首次见到某文件：初始化 offset 为当前大小（不回放历史，只看未来）；
#   - 文件被截断/轮转（size < offset）：重置为 0 全量重读。
# 容器日志 --since 3m 保持不变：持续异常仍每 5 分钟重提醒，恢复后自动掉出窗口。
set -euo pipefail

cd /opt/papafeiji
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

OFFSET_DIR=/var/lib/papafeiji/alert-cron-offsets
mkdir -p "${OFFSET_DIR}" 2>/dev/null || true

# feed_file <日志文件路径>：向 stdout 输出该文件自上次扫描后的新增内容，并推进 offset。
feed_file() {
  local file="$1" offset_file current_size offset
  [[ -f "${file}" ]] || return 0
  offset_file="${OFFSET_DIR}/$(basename "${file}").offset"
  current_size=$(stat -c %s "${file}" 2>/dev/null || echo 0)
  if [[ ! -f "${offset_file}" ]]; then
    echo "${current_size}" > "${offset_file}"   # 首次：只看未来，不回放历史
    return 0
  fi
  offset=$(cat "${offset_file}" 2>/dev/null || echo 0)
  [[ "${offset}" =~ ^[0-9]+$ ]] || offset=0
  if (( current_size < offset )); then
    offset=0   # 截断/轮转：全量重读
  fi
  if (( current_size > offset )); then
    tail -c +"$((offset + 1))" "${file}"
    echo "${current_size}" > "${offset_file}"
  fi
}

{
  docker compose logs --since 3m app sse backup certbot 2>&1 || true
  feed_file /var/log/papafeiji-watchdog.log
  feed_file /var/log/papafeiji-cert-check.log
  feed_file /var/log/papafeiji-alert-p95.log
} | exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/alert-watch.sh" -
