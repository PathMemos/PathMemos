#!/usr/bin/env bash
# 最小告警通道：扫描应用日志中的资金/数据完整性关键字并 POST 到 webhook。
#
# 生产建议 cron（每 2 分钟）：
#   cd /opt/papafeiji && docker compose logs --since 3m app sse 2>&1 | scripts/alert-watch.sh -
#
# 环境变量：
#   ALERT_WEBHOOK_URL  告警 webhook（飞书/钉钉/Slack 等）；为空时只打印不发送并退出 0
#   ALERT_WINDOW_SEC   同类关键字去重窗口，默认 300 秒
#   ALERT_STATE_FILE   去重状态文件，默认 /var/lib/papafeiji/alert-state
#   ALERT_DRY_RUN      为 1 时只打印命中，不发送
set -euo pipefail

SRC="${1:--}"
WEBHOOK="${ALERT_WEBHOOK_URL:-}"
WINDOW="${ALERT_WINDOW_SEC:-300}"
STATE="${ALERT_STATE_FILE:-/var/lib/papafeiji/alert-state}"
DRY="${ALERT_DRY_RUN:-0}"

# 资金/数据完整性/可用性相关关键字（命中即告警，全集见 docs/DEPLOYMENT.md §10 告警关键字表）。
# job_stale / watchdog_circuit_open 由 R-03/R-07 产出；
# redis_mem_high / backup_* / ai_upstream_error / auto_record_failed / cert_expiry_soon 由 R-09 加固批次产出。
# "alert":"redis_down"：main.go JSON 日志里 alert 属性（docs 关键字 alert=redis_down）的实际渲染形态，
#   Redis 故障健康降级 200 后唯一兜底触达通道；wx mp kf：wxmp/handler.go 的 msg 带 [ALERT] 前缀，
#   grep BRE 中 [ALERT] 是字符组，故去掉方括号前缀取 msg 主体匹配。
KEYWORDS='payment_notify_missing_msg_signature|payment_notify_bad_signature|payment_notify_decrypt_failed|payment_notify_plaintext_rejected|payment_notify_receive_id_mismatch|payment_notify_amount_missing|payment_notify_amount_invalid|payment_notify_amount_mismatch|payment_notify_business_rejected|payment_notify_closed_order_reissued|payment_notify_transient_retry|alert:payment_parse_failed|alert:ai_quota_refund_failed|alert=ai_upstream_error|alert=auto_record_failed|alert=backup_failed|alert=backup_uploads_failed|alert=backup_stale|alert=redis_mem_high|alert=papafeiji_cpu_high|alert=papafeiji_cpu_recovered|alert=uptime_down|alert=uptime_recovered|"alert":"redis_down"|wx mp all kf segments failed|cert_expiry_soon|job_stale|watchdog_circuit_open'

mkdir -p "$(dirname "$STATE")" 2>/dev/null || true
exec 9>"${STATE}.lock" 2>/dev/null || true
flock -n 9 2>/dev/null || exit 0

if [[ "$SRC" == "-" ]]; then
  INPUT="$(cat)"
else
  INPUT="$(cat "$SRC")"
fi

now=$(date +%s)
hits=0
# 逐关键字去重：同一关键字在窗口内只告警一次；记录最近命中时间去重。
while IFS= read -r kw; do
  [[ -z "$kw" ]] && continue
  grep -q -- "$kw" <<< "$INPUT" || continue
  last=$(grep -F "$kw" "$STATE" 2>/dev/null | tail -1 | awk '{print $2}' || true)
  if [[ -n "$last" && $((now - last)) -lt "$WINDOW" ]]; then
    continue
  fi
  sample=$(grep -m1 -- "$kw" <<< "$INPUT" | cut -c1-500)
  hits=$((hits + 1))
  if [[ "$DRY" == "1" || -z "$WEBHOOK" ]]; then
    echo "[ALERT] $kw :: $sample"
  else
    payload=$(python3 - "$kw" "$sample" <<'PY'
import json, sys
print(json.dumps({"msg_type": "text", "content": {"text": f"[papafeiji] 告警关键字: {sys.argv[1]}\n{sys.argv[2]}"}}))
PY
)
    if curl -fsS -m 10 -H 'Content-Type: application/json' -d "$payload" "$WEBHOOK" >/dev/null 2>&1; then
      echo "[ALERT-SENT] $kw"
    else
      echo "[ALERT-FAIL] $kw" >&2
    fi
  fi
  printf '%s %s\n' "$kw" "$now" >> "$STATE"
done < <(tr '|' '\n' <<< "$KEYWORDS")

# 状态文件去重记录保留最近 1000 行。
if [[ -f "$STATE" ]]; then
  tail -1000 "$STATE" > "${STATE}.tmp" 2>/dev/null && mv "${STATE}.tmp" "$STATE" || true
fi
exit 0
