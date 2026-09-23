#!/usr/bin/env bash
# 连接预算验收测试：DB_MAX_CONNS=75 + DB_BG_MAX_CONNS=10 必须通过；DB_MAX_CONNS=90 必须被拒。
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECK="${DIR}/scripts/check-conn-budget.sh"
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
printf 'services:\n  app:\n    environment:\n      DB_MAX_CONNS: 75\n      DB_BG_MAX_CONNS: 10\n' > "${tmp}/ok.yml"
printf 'services:\n  app:\n    environment:\n      DB_MAX_CONNS: 90\n      DB_BG_MAX_CONNS: 10\n' > "${tmp}/bad.yml"
if ! bash "${CHECK}" "${tmp}/ok.yml" >/dev/null; then echo "FAIL: 75+10 应通过"; exit 1; fi
if bash "${CHECK}" "${tmp}/bad.yml" >/dev/null 2>&1; then echo "FAIL: 90+10 应被拒"; exit 1; fi
echo "PASS: 连接预算校验（75+10 通过 / 90+10 拒绝）"
