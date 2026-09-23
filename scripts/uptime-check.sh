#!/usr/bin/env bash
# 外部拨测（控制机侧，建议 cron 每 5 分钟）：
#   */5 * * * * /root/Github/PathMemos-SaaS/scripts/uptime-check.sh >/dev/null 2>&1
# 从控制机探测生产 API（/health/ready）与官网首页（配置 CFG_WEB_DOMAIN 时启用），
# 与服务器内探针相互独立：连续 2 次失败推送 alert=uptime_down；恢复时推送 alert=uptime_recovered。
# 依赖 /root/DeployOps/env/.env.papafeiji 提供 CFG_DOMAIN 与 CFG_ALERT_WEBHOOK_URL。
# 控制机 cron 不由 deploy.sh 管理，需手工安装一次。
set -uo pipefail

ENV_FILE="${PAPAFEIJI_ENV_FILE:-/root/DeployOps/env/.env.papafeiji}"
[[ -f "${ENV_FILE}" ]] && { set -a; . "${ENV_FILE}"; set +a; }

WEBHOOK="${CFG_ALERT_WEBHOOK_URL:-}"

notify() {
  [[ -n "${WEBHOOK}" ]] || return 0
  local payload
  payload="$(python3 - "$1" <<'PY'
import json, sys
print(json.dumps({"msg_type": "text", "content": {"text": f"[papafeiji] {sys.argv[1]}"}}))
PY
)"
  curl -fsS -m 10 -H 'Content-Type: application/json' -d "${payload}" "${WEBHOOK}" >/dev/null 2>&1 || true
}

# check <探测URL> <失败计数状态文件>：连续 2 次失败告警，恢复时解除。
check() {
  local url="$1" state="$2"
  local fails
  fails="$(cat "${state}" 2>/dev/null || echo 0)"
  [[ "${fails}" =~ ^[0-9]+$ ]] || fails=0

  if curl -fsS -m 10 "${url}" >/dev/null 2>&1; then
    if (( fails >= 2 )); then
      notify "告警关键字: alert=uptime_recovered domain=${url}（连续失败 ${fails} 次后恢复）"
    fi
    echo 0 > "${state}"
    return 0
  fi

  fails=$((fails + 1))
  echo "${fails}" > "${state}"
  if (( fails == 2 )); then
    notify "告警关键字: alert=uptime_down domain=${url}（连续 2 次探测失败）"
  fi
}

# 域名归一：CFG_DOMAIN / CFG_WEB_DOMAIN 可能带协议前缀/尾斜杠（如 https://pro.papafeiji.cn）。
normalize_domain() {
  local d="$1"
  d="${d#http://}"
  d="${d#https://}"
  d="${d%%/*}"
  echo "${d}"
}

DOMAIN="$(normalize_domain "${CFG_DOMAIN:-}")"
if [[ -n "${DOMAIN}" ]]; then
  check "https://${DOMAIN}/health/ready" "${PAPAFEIJI_UPTIME_STATE:-/tmp/.papafeiji-uptime-fails}"
fi

WEB_DOMAIN="$(normalize_domain "${CFG_WEB_DOMAIN:-}")"
if [[ -n "${WEB_DOMAIN}" ]]; then
  check "https://${WEB_DOMAIN}/" "${PAPAFEIJI_UPTIME_STATE_WEB:-/tmp/.papafeiji-uptime-fails-web}"
fi

exit 0
