#!/usr/bin/env bash
# P95 观测定时入口（cron 每 15 分钟，由 deploy.sh 自动装配）：
# 统计最近 15 分钟访问日志按路由分位耗时；有路由 P95 超 SLO_P95_MS（默认 500ms）时
# 推送 webhook（关键字 slo_breach，/alert-watch.sh 同款消息格式）。正常时只打印统计。
set -euo pipefail

cd /opt/papafeiji
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$(docker compose logs --since 15m app 2>&1 | "${SCRIPT_DIR}/accesslog-p95.sh" - 15 2>&1)" || true
echo "${OUT}"

if grep -q "slo_breach" <<< "${OUT}"; then
  if [[ -n "${ALERT_WEBHOOK_URL:-}" ]]; then
    sample="$(grep -m1 'slo_breach' <<< "${OUT}" | cut -c1-500)"
    payload="$(python3 - "$sample" <<'PY'
import json, sys
print(json.dumps({"msg_type": "text", "content": {"text": f"[papafeiji] 告警关键字: slo_breach\n{sys.argv[1]}"}}))
PY
)"
    curl -fsS -m 10 -H 'Content-Type: application/json' -d "${payload}" "${ALERT_WEBHOOK_URL}" >/dev/null 2>&1 || true
  fi
fi
