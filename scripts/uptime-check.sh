#!/usr/bin/env bash
# 外部拨测（控制机侧，建议 cron 每 5 分钟）：
#   */5 * * * * /root/Github/PathMemos-SaaS/scripts/uptime-check.sh >/dev/null 2>&1
# 从控制机探测四个公网面：生产 API（/health/ready，CFG_DOMAIN）、官网首页
# （CFG_WEB_DOMAIN，未配置跳过）、MCP 公网入口 /health（默认 mcp.pathmemos.com，
# CFG_MCP_PUBLIC_URL 可覆盖）与 api-worker 公网入口（默认 api.pathmemos.com，
# CFG_API_WORKER_URL 可覆盖，宽松语义见文内），与服务器内探针相互独立：
# 连续 2 个周期失败推送 alert=uptime_down；恢复时推送 alert=uptime_recovered
# （各面独立状态文件）。每个探测周期内失败即触发一次即时重试（间隔 5s），
# 两次都失败才计本周期失败——吸收大陆直连 Cloudflare 边缘的瞬时抖动。
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

# check <探测URL> <失败计数状态文件> [超时秒数，默认 10]：连续 2 次失败告警，恢复时解除。
# 超时参数须大于被探端的内部超时：MCP 面 Worker /health 回源超时 15s（大陆白天高峰
# CF→源站国际链路拥塞、回源 5s+ 属常态），外层取 20s，否则 Worker 未放弃 curl 先
# 判失败，高峰误报 down。
check() {
  local url="$1" state="$2" timeout="${3:-10}"
  local fails
  fails="$(cat "${state}" 2>/dev/null || echo 0)"
  [[ "${fails}" =~ ^[0-9]+$ ]] || fails=0

  # 单次探测内置一次即时重试（间隔 5s）：吸收控制机（大陆）直连 Cloudflare 边缘的
  # 瞬时抖动（无国内 PoP，高峰时段偶发超时属常态）——两次都失败才计本周期失败。
  if curl -fsS -m "${timeout}" "${url}" >/dev/null 2>&1 || { sleep 5; curl -fsS -m "${timeout}" "${url}" >/dev/null 2>&1; }; then
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

# MCP 公网入口（O4 商业能力的外部拨测面）：
# Worker/KV/回源任一故障只能靠付费用户报告。默认探测官方域名，
# 可经 CFG_MCP_PUBLIC_URL 覆盖（normalize_domain 已容错协议前缀/尾斜杠）。
MCP_DOMAIN="$(normalize_domain "${CFG_MCP_PUBLIC_URL:-https://mcp.pathmemos.com}")"
if [[ -n "${MCP_DOMAIN}" ]]; then
  check "https://${MCP_DOMAIN}/health" "${PAPAFEIJI_UPTIME_STATE_MCP:-/tmp/.papafeiji-uptime-fails-mcp}" 20
fi

# api-worker 公网入口：开源版私有化路由用户唯一入口。
# 该 Worker 无 GET-200 端点（/worker/register 仅 POST），探测取宽松语义：
# 任意 HTTP 响应（含 405/404）即视为存活，仅连接层失败（超时/DNS/5xx 网关错误后无响应）计为失败。
# check_lax <探测URL> <失败计数状态文件>：与 check 同款连续 2 次失败/恢复通知。
check_lax() {
  local url="$1" state="$2"
  local fails code
  fails="$(cat "${state}" 2>/dev/null || echo 0)"
  [[ "${fails}" =~ ^[0-9]+$ ]] || fails=0

  code="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "${url}" 2>/dev/null || echo 000)"
  # 与 check() 同款单次内置重试（间隔 5s），吸收 CF 边缘瞬时抖动。
  if [[ "${code}" == "000" ]]; then
    sleep 5
    code="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "${url}" 2>/dev/null || echo 000)"
  fi
  if [[ "${code}" != "000" ]]; then
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

API_WORKER_DOMAIN="$(normalize_domain "${CFG_API_WORKER_URL:-https://api.pathmemos.com}")"
if [[ -n "${API_WORKER_DOMAIN}" ]]; then
  check_lax "https://${API_WORKER_DOMAIN}/worker/register" "${PAPAFEIJI_UPTIME_STATE_API_WORKER:-/tmp/.papafeiji-uptime-fails-api-worker}"
fi

exit 0
