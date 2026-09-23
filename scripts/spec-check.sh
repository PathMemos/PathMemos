#!/usr/bin/env bash
# spec-check.sh — PathMemos-SaaS spec 门禁（见 docs/spec-standards.md §九）
# 用法：bash scripts/spec-check.sh [--warn]
#   --warn：把「spec 随代码变更」从阻断降级为提示（默认阻断）
# 退出码：0 全部通过；1 存在阻断项。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 2
WARN_ONLY=false
for a in "$@"; do
  case "$a" in --warn) WARN_ONLY=true;; --strict) WARN_ONLY=false;; esac
done

RED=$'\033[31m'; GRN=$'\033[32m'; YEL=$'\033[33m'; RST=$'\033[0m'
FAIL=0
block() { printf '%s阻断%s %s\n' "$RED" "$RST" "$1"; FAIL=1; }
warn()  { printf '%s提示%s %s\n' "$YEL" "$RST" "$1"; }
ok()    { printf '%s通过%s %s\n' "$GRN" "$RST" "$1"; }

printf '=== spec-check @ %s ===\n' "$(git rev-parse --short HEAD 2>/dev/null || echo no-git)"

# 1. spec 制品齐备（阻断）
MISSING=""; MISSING_N=0
for f in docs/spec-standards.md docs/spec/01-product-overview.md \
  docs/spec/02a-auth-account.md docs/spec/02b-diary.md docs/spec/02c-autorecord.md \
  docs/spec/02d-family-invite.md docs/spec/02e-vip-payment.md docs/spec/02f-ai-chat.md \
  docs/spec/02g-file-push.md docs/spec/02h-mcp-open.md \
  docs/spec/03-api.md docs/spec/04-database.md docs/spec/05-development-plan.md \
  docs/spec/06-miniapp-pages.md docs/spec/07-acceptance-flows.md docs/CAPABILITIES.md; do
  if [[ ! -s "$f" ]]; then MISSING="$MISSING $f"; MISSING_N=$((MISSING_N+1)); fi
done
if [[ $MISSING_N -eq 0 ]]; then ok "spec 制品齐备"; else block "spec 制品缺失/为空:$MISSING"; fi

# 2. migration up/down 配对（阻断）
MP=0
while IFS= read -r up; do
  dn="$(printf '%s' "$up" | sed 's/\.up\.sql$/.down.sql/')"
  if [[ ! -s "$dn" ]]; then block "migration 缺少 down: $up"; MP=1; fi
done < <(find backend/migrations -name '*.up.sql' | sort)
while IFS= read -r dn; do
  up="$(printf '%s' "$dn" | sed 's/\.down\.sql$/.up.sql/')"
  if [[ ! -s "$up" ]]; then block "migration 缺少 up: $dn"; MP=1; fi
done < <(find backend/migrations -name '*.down.sql' | sort)
if [[ $MP -eq 0 ]]; then ok "migration up/down 配对"; fi

# 3. ADR 索引双向（阻断）
ADR_OK=0
while IFS= read -r f; do
  n="$(basename "$f" | cut -c1-4)"
  if ! grep -q "$n" docs/decisions/README.md; then block "ADR 未登记进索引: $f"; ADR_OK=1; fi
done < <(find docs/decisions -maxdepth 1 -name '[0-9][0-9][0-9][0-9]-*.md' ! -name '0000-template.md' | sort)
while IFS= read -r n; do
  if [[ "$n" == "0000" ]]; then continue; fi
  p="$(printf 'docs/decisions/%s-*.md' "$n")"
  if ! compgen -G "$p" >/dev/null; then block "ADR 索引指向不存在文件: $n"; ADR_OK=1; fi
done < <(grep -oE '\[[0-9][0-9][0-9][0-9]\]' docs/decisions/README.md 2>/dev/null | tr -d '[]' | sort -u)
if [[ $ADR_OK -eq 0 ]]; then ok "ADR 索引双向一致"; fi

# 4. migration 编号 ↔ L4 变更记录（阻断）
M4=0
while IFS= read -r up; do
  n="$(basename "$up" | cut -c1-6)"
  if ! grep -qE "^\| *$n " docs/spec/04-database.md; then block "migration $n 未登记进 L4 §7"; M4=1; fi
done < <(find backend/migrations -name '*.up.sql' | sort)
while IFS= read -r n; do
  p="$(printf 'backend/migrations/%s_*.up.sql' "$n")"
  if ! compgen -G "$p" >/dev/null; then warn "L4 登记了 $n 但磁盘无对应 migration"; fi
done < <(grep -oE '^\| *[0-9]{6} ' docs/spec/04-database.md 2>/dev/null | grep -oE '[0-9]{6}' | sort -u)
if [[ $M4 -eq 0 ]]; then ok "migration 编号 ↔ L4 变更记录"; fi

# 5. 安全红线：密钥/口令 + 上传类型白名单（阻断）
SEC=0
if git ls-files --error-unmatch .env >/dev/null 2>&1; then block ".env 被纳入版本控制"; SEC=1; fi
if git grep -nIE -- '-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----' >/dev/null 2>&1; then block "仓库内出现私钥块（文本文件）"; SEC=1; fi
WHITELIST="$(sed -n '/allowedExts = map/,/}/p' backend/internal/file/handler.go 2>/dev/null)"
if [[ -z "$WHITELIST" ]]; then
  block "未找到上传扩展名白名单（backend/internal/file/handler.go allowedExts）"; SEC=1
elif grep -qiE '\.(php|phtml|jsp|asp|aspx|js|mjs|sh|bash|html|htm|svg|exe|dll|bat)' <<< "$WHITELIST"; then
  block "上传白名单包含危险类型"; SEC=1
fi
if [[ $SEC -eq 0 ]]; then ok "安全红线（密钥/私钥 + 上传白名单）"; fi

# 6. spec 随代码变更（默认阻断；--warn 降级）
BASE="origin/main"
if ! git rev-parse --verify -q "$BASE" >/dev/null 2>&1; then BASE="HEAD~1"; fi
if git rev-parse --verify -q "$BASE" >/dev/null 2>&1; then
  CHANGED="$(git diff --name-only "$BASE"...HEAD 2>/dev/null || true)"
  CODE="$(printf '%s\n' "$CHANGED" | grep -E '^(backend/|api-worker/src/|mcp-worker/src/)' || true)"
  SPEC="$(printf '%s\n' "$CHANGED" | grep -E '^(docs/spec/|docs/CAPABILITIES\.md)' || true)"
  MSG="$(git log --format=%B "$BASE"..HEAD 2>/dev/null || true)"
  if [[ -n "$CODE" && -z "$SPEC" ]] && ! grep -qi 'spec:nochange' <<< "$MSG"; then
    if $WARN_ONLY; then warn "代码变更但 spec 未同步（或 commit 写 spec:nochange）"; else block "代码变更但 spec 未同步（或 commit 写 spec:nochange）"; fi
  else
    ok "spec 随代码变更"
  fi
else
  warn "无基线（浅克隆？），跳过 spec 随代码变更检查"
fi

# 7. 路由 ↔ L3 覆盖（提示）
ROUTE_MISS=""
ROUTES="$(grep -rhoE '\.(Get|Post|Put|Delete|Patch)\("[^"]*"' backend/internal backend/cmd --include=*.go --exclude='*_test.go' --exclude='rpc.go' 2>/dev/null | sed -E 's/^\.(Get|Post|Put|Delete|Patch)\("//; s/"$//' | grep '^/' | sort -u)"
for p in $ROUTES; do grep -qF "$p" docs/spec/03-api.md || ROUTE_MISS="$ROUTE_MISS $p"; done
if [[ -z "$ROUTE_MISS" ]]; then ok "路由 ↔ L3 契约覆盖"; else warn "路由未登记进 L3:$ROUTE_MISS"; fi

# 8. 错误码词汇表 ↔ pkg/errors（提示）
CODE_MISS=""
CODES="$(grep -oE '^[[:space:]]+(Biz|Code)[A-Za-z0-9_]+' backend/pkg/errors/codes.go 2>/dev/null | tr -d ' ' | sort -u)"
for c in $CODES; do grep -qF "$c" docs/spec/03-api.md || CODE_MISS="$CODE_MISS $c"; done
if [[ -z "$CODE_MISS" ]]; then ok "错误码词汇表 ↔ pkg/errors"; else warn "错误码未登记进 L3:$CODE_MISS"; fi

# 9. 新增后端实现文件带同包测试（提示）
if git rev-parse --verify -q "$BASE" >/dev/null 2>&1; then
  NEWGO="$(git diff --name-only --diff-filter=A "$BASE"...HEAD -- 'backend/**/*.go' 2>/dev/null | grep -v '_test\.go$' || true)"
  NOTEST=""
  for f in $NEWGO; do
    d="$(dirname "$f")"
    compgen -G "$d/*_test.go" >/dev/null || NOTEST="$NOTEST $f"
  done
  if [[ -z "$NOTEST" ]]; then ok "新增后端文件带测试"; else warn "新增后端实现文件所在包无测试:$NOTEST"; fi
fi

# 10. down 破坏性操作声明（提示）
for f in backend/migrations/*.down.sql; do
  if grep -qiE 'DROP SCHEMA|TRUNCATE' "$f" && ! grep -qE '不可逆|破坏性' "$f"; then
    warn "down 含破坏性操作但未声明「不可逆/破坏性」: $f"
  fi
done

# 11. 能力索引反向差异（提示）
for f in docs/spec/02?.md; do
  [[ -f "$f" ]] || continue
  n="$(basename "$f" | cut -c1-3)"
  grep -q "$n" docs/CAPABILITIES.md || warn "领域分册未登记能力索引: $f"
done
while IFS= read -r n; do
  p="$(printf 'docs/spec/%s*.md' "$n")"
  compgen -G "$p" >/dev/null || warn "能力索引引用不存在的分册: $n"
done < <(grep -oE '02[a-z]' docs/CAPABILITIES.md | sort -u)

# 12. spec 内 file:line 引用范围（提示）— SR-12
LINEREF="$(python3 - <<'PY' 2>/dev/null || true
import re, pathlib
roots = ["docs/spec", "docs/CAPABILITIES.md", "docs/ARCHITECTURE.md"]
refs = {}
paths = []
for root in roots:
    p = pathlib.Path(root)
    paths.extend(p.rglob("*.md") if p.is_dir() else [p])
for f in paths:
    try:
        text = f.read_text(encoding="utf-8")
    except OSError:
        continue
    for m in re.finditer(r"([A-Za-z0-9_./-]+\.(?:go|ts|sql|sh|ya?ml|json|md)):(\d+)", text):
        path, line = m.group(1), int(m.group(2))
        if "/" in path:
            refs[(path, line)] = True
PREFIXES = ["", "backend/internal/", "backend/pkg/", "backend/",
            "frontend/miniapp/miniprogram/", "deploy/", "scripts/",
            "mcp-worker/src/", "api-worker/src/"]

def resolve(path):
    for pre in PREFIXES:
        fp = pathlib.Path(pre + path)
        if fp.is_file():
            return fp
    return None

bad = []
for (path, line) in refs:
    fp = resolve(path)
    if fp is None:
        continue
    n = sum(1 for _ in fp.open(encoding="utf-8"))
    if line > n:
        bad.append(f"{path}:{line}(超出 {n} 行)")
print(" ".join(bad[:20]))
PY
)"
if [[ -z "$LINEREF" ]]; then ok "spec file:line 引用在范围内"; else warn "file:line 引用失效: $LINEREF"; fi

# 13. docs 无过程产物/评审残留（阻断）— SR-13
ARTIFACTS="$(grep -rnE '\bR-[0-9]{2}\b|plans/|第三步整改' docs/ --exclude='spec-standards.md' 2>/dev/null || true)"
if [[ -z "$ARTIFACTS" ]]; then ok "docs 无过程产物（R-xx / plans / 第三步整改）"; else
  block "docs 含过程产物/评审残留（应放入 plans/，保持 docs 为代码终态）"
  printf '%s\n' "$ARTIFACTS" | head -20
fi

# 11. MCP 静态方法双源对账（提示）— SR-11
if [[ -f scripts/check_mcp_static_sync.py ]]; then
  MCP_OUT="$(python3 scripts/check_mcp_static_sync.py 2>&1)"; MCP_RC=$?
  if [[ $MCP_RC -eq 0 ]]; then ok "MCP 静态方法双源一致"; else warn "MCP 静态方法漂移: $(tr '\n' ';' <<< "$MCP_OUT")"; fi
else
  warn "缺少 scripts/check_mcp_static_sync.py，跳过 MCP 静态方法对账"
fi

if [[ $FAIL -eq 0 ]]; then printf '%s=== spec-check: PASS ===%s\n' "$GRN" "$RST"; else printf '%s=== spec-check: FAIL ===%s\n' "$RED" "$RST"; fi
exit $FAIL
