#!/usr/bin/env bash
# 统计结构化访问日志（slog JSON，msg="api access"）的按路由分位耗时。
#
# 用法: scripts/accesslog-p95.sh [日志文件] [窗口分钟=5]
#   日志文件省略或为 - 时读 stdin；兼容 docker logs 前缀行（按行 JSON 解析，跳过非 JSON）。
# 环境: SLO_P95_MS 核心接口 P95 阈值（毫秒，默认 500）
# 退出码: 0 正常；2 有路由 P95 超阈值（slo_breach）
set -euo pipefail
LOG="${1:--}"
WINDOW_MIN="${2:-5}"
SLO_P95_MS="${SLO_P95_MS:-500}"
python3 - "${LOG}" "${WINDOW_MIN}" "${SLO_P95_MS}" <<'PY'
import json, sys
from collections import defaultdict
from datetime import datetime, timezone, timedelta

path, window, slo = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
buckets = defaultdict(list)
now = datetime.now(timezone.utc)
stream = sys.stdin if path in ("-", "/dev/stdin") else open(path, encoding="utf-8", errors="replace")

for line in stream:
    line = line.strip()
    if "api access" not in line:
        continue
    # docker logs 前缀等：截取第一个 '{'
    idx = line.find("{")
    if idx < 0:
        continue
    try:
        rec = json.loads(line[idx:])
    except Exception:
        continue
    if rec.get("msg") != "api access":
        continue
    t = rec.get("time")
    if t:
        try:
            ts = datetime.fromisoformat(str(t).replace("Z", "+00:00"))
            if ts.tzinfo is None:
                ts = ts.replace(tzinfo=timezone.utc)
            if now - ts > timedelta(minutes=window):
                continue
        except Exception:
            pass
    route = rec.get("route") or rec.get("path") or "?"
    d = rec.get("duration_ms")
    if d is None:
        dv = rec.get("duration")
        if dv is None:
            continue
        d = float(dv) / 1e6  # slog.Duration 以纳秒编码
    buckets[route].append(float(d))

def pct(xs, p):
    xs = sorted(xs)
    if not xs:
        return 0.0
    k = (len(xs) - 1) * p
    lo = int(k)
    hi = min(lo + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)

breach = False
print(f"window={window:g}min routes={len(buckets)}")
for route in sorted(buckets, key=lambda r: -len(buckets[r])):
    xs = buckets[route]
    p50, p95, p99 = pct(xs, 0.5), pct(xs, 0.95), pct(xs, 0.99)
    flag = ""
    if p95 > slo:
        flag = " SLO_BREACH"
        breach = True
        print(f"slo_breach route={route} p95={p95:.1f}ms threshold={slo:.0f}ms n={len(xs)}", file=sys.stderr)
    print(f"{route:40s} n={len(xs):6d} p50={p50:8.1f}ms p95={p95:8.1f}ms p99={p99:8.1f}ms{flag}")
if not buckets:
    print("no api access records in window")
sys.exit(2 if breach else 0)
PY
