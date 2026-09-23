#!/usr/bin/env bash
# AI 成本观测（服务器侧执行）：按结构化日志（msg="ai chat usage"）汇总最近 N 小时的
# token 用量——总量与按用户 Top。日志是尽力而为的观测数据（不落库），仅容器日志保留期内可查。
# 用法: bash /opt/papafeiji/src/scripts/ai-cost.sh [小时=24]
set -euo pipefail

cd /opt/papafeiji
HOURS="${1:-24}"
docker compose logs --since "${HOURS}h" app 2>&1 | python3 - "${HOURS}" <<'PY'
import json, sys
from collections import defaultdict

hours = sys.argv[1]
total_prompt = total_completion = turns = 0
per_user = defaultdict(lambda: [0, 0, 0])  # prompt, completion, turns

for line in sys.stdin:
    idx = line.find("{")
    if idx < 0 or '"ai chat usage"' not in line:
        continue
    try:
        rec = json.loads(line[idx:])
    except Exception:
        continue
    if rec.get("msg") != "ai chat usage":
        continue
    p, c = int(rec.get("prompt_tokens", 0)), int(rec.get("completion_tokens", 0))
    uid = rec.get("user_id", "?")
    total_prompt += p
    total_completion += c
    turns += 1
    per_user[uid][0] += p
    per_user[uid][1] += c
    per_user[uid][2] += 1

print(f"window={hours}h turns={turns} prompt_tokens={total_prompt} completion_tokens={total_completion}")
for uid, (p, c, n) in sorted(per_user.items(), key=lambda kv: -(kv[1][0] + kv[1][1]))[:10]:
    print(f"user={uid} turns={n} prompt_tokens={p} completion_tokens={c}")
PY
