#!/usr/bin/env bash
# APK 完整发布：前置全量部署（后端 + 小程序体验版上传）→ 云构建 APK → 验签 → 上传官网。
#
# 用法:
#   node/bash scripts/release-apk.sh [--branch <feat 分支>] [--skip-deploy] [--skip-upload]
#
# 流程:
#   1.（默认）对 --branch 指定的分支执行 deploy.sh 全量部署：后端 + 小程序体验版上传，
#      健康检查通过后自动合入 main（--skip-deploy 跳过，要求 main 已是最新）。
#   2. 从合并后的 origin/main 云构建 APK（miniprogram-ci@beta buildApk，版本取
#      project.miniapp.json 的 version，构建前校验 version 必须大于官网最新已发布
#      版本——同版本与版本回退均拒绝，覆盖安装依赖 version/versionCode 同步递增）。
#   3. keytool 验签（指纹必须与历史版本一致，保证覆盖安装兼容）。
#   4. scp 上传官网 downloads 并验证下载 URL 200。
#
# 凭据: 同 build-apk-ci.js（CI 上传私钥 / APK_CI_KEYSTORE / APK_CI_KSPASS）；
#       体验版上传需 MINIAPP_CI_KEY 指向仓库外gitignore 的私钥（控制机默认路径见下）。
set -euo pipefail

REPO="${APK_RELEASE_REPO:-/root/Github/PathMemos-SaaS}"
# ssh 别名跟随生产（别名定义见控制机 ~/.ssh/config）
REMOTE_HOST="${APK_RELEASE_HOST:-papafeiji}"
REMOTE_DIR="${APK_RELEASE_DIR:-/opt/papafeiji/web/downloads}"
DOWNLOAD_BASE="${APK_RELEASE_URL:-https://papafeiji.cn/downloads}"
CI_KEY="${MINIAPP_CI_KEY:-${REPO}/deploy/private.wxe56d190a14826b0d.key}"
KEYSTORE="${APK_CI_KEYSTORE:-/root/DeployOps/keys/papafeiji.keystore}"
EXPECTED_FP="30:9B:92:E9:06:91:E0:85:2D:9A:AE:06:CB:A4:E2:6C:22:FE:57:91:AA:0D:0B:EF:23:BA:2D:5F:B5:49:68:0B"
OUT_DIR="${APK_RELEASE_OUT:-/tmp/apk-out}"

BRANCH=""
SKIP_DEPLOY=0
SKIP_UPLOAD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --branch) BRANCH="$2"; shift 2 ;;
    --skip-deploy) SKIP_DEPLOY=1; shift ;;
    --skip-upload) SKIP_UPLOAD=1; shift ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

command -v node >/dev/null || { echo "错误：需要 node" >&2; exit 1; }
[[ -f "${REPO}/scripts/build-apk-ci.js" ]] || { echo "错误：${REPO} 缺少 scripts/build-apk-ci.js（需在 main 最新版本上）" >&2; exit 1; }
[[ -f "${KEYSTORE}" ]] || { echo "错误：缺少签名库 ${KEYSTORE}" >&2; exit 1; }

if [[ "${SKIP_DEPLOY}" != "1" ]]; then
  [[ -n "${BRANCH}" ]] || { echo "错误：部署模式需要 --branch <feat 分支>（或 --skip-deploy）" >&2; exit 1; }
  echo "==> [1/4] 全量部署 ${BRANCH}（后端 + 小程序体验版上传，健康检查后自动合入 main）"
  bash -c "source /root/DeployOps/env/.env.papafeiji && cd '${REPO}' && MINIAPP_CI_KEY='${CI_KEY}' ./deploy/deploy.sh --branch '${BRANCH}'"
else
  echo "==> [1/4] 跳过部署（--skip-deploy）"
fi

echo "==> [2/4] 同步 origin/main 并确定发布版本"
git -C "${REPO}" fetch origin main 2>/dev/null
SRC_TREE="/tmp/apk-release-src-$$"
git -C "${REPO}" worktree add --detach "${SRC_TREE}" origin/main >/dev/null 2>&1
trap 'git -C "${REPO}" worktree remove --force "${SRC_TREE}" 2>/dev/null || true' EXIT

VERSION=$(node -e "console.log(JSON.parse(require('fs').readFileSync('${SRC_TREE}/frontend/miniapp/project.miniapp.json')).version)")
CODE=$(node -e "console.log(JSON.parse(require('fs').readFileSync('${SRC_TREE}/frontend/miniapp/project.miniapp.json')).versionCode)")
LAST_APK=$(ssh -o BatchMode=yes "${REMOTE_HOST}" "ls -1 '${REMOTE_DIR}' 2>/dev/null | grep -oE 'papafeiji-v[0-9.]+-test\\.apk' | sort -V | tail -1 | grep -oE '[0-9.]+' | head -1" || true)
if [[ -n "${LAST_APK}" ]]; then
  if [[ "${VERSION}" == "${LAST_APK}" ]]; then
    echo "错误：官网已存在同版本 ${LAST_APK}，请先在 project.miniapp.json 递增 version/versionCode" >&2
    exit 1
  fi
  # 覆盖安装要求版本单调递增（version 与 versionCode 同步维护），回退发布直接拒绝
  if [[ "$(printf '%s\n' "${LAST_APK}" "${VERSION}" | sort -V | tail -1)" != "${VERSION}" ]]; then
    echo "错误：发布版本 ${VERSION} 不大于官网最新测试包 ${LAST_APK}，Android 将拒绝覆盖安装" >&2
    exit 1
  fi
fi
echo "    发布版本 ${VERSION}（versionCode ${CODE}）；官网现有最新测试包: ${LAST_APK:-无}"

echo "==> [3/4] 云构建 APK（消耗微信云构建额度 1 次）"
node "${REPO}/scripts/build-apk-ci.js" "${SRC_TREE}/frontend/miniapp" "${VERSION}" "${OUT_DIR}"
APK_FILE=$(ls -t "${OUT_DIR}"/*-"${VERSION}".apk 2>/dev/null | head -1)
[[ -n "${APK_FILE}" && -f "${APK_FILE}" ]] || { echo "错误：输出目录中未找到版本 ${VERSION} 的构建产物" >&2; exit 1; }

ACTUAL_FP=$(keytool -printcert -jarfile "${APK_FILE}" 2>/dev/null | grep 'SHA256:' | head -1 | awk '{print $2}')
if [[ "${ACTUAL_FP}" != "${EXPECTED_FP}" ]]; then
  echo "错误：签名指纹不符（${ACTUAL_FP:-未取到}），中止上传" >&2
  exit 1
fi
echo "    验签通过：${ACTUAL_FP}"

if [[ "${SKIP_UPLOAD}" == "1" ]]; then
  echo "==> [4/4] 跳过上传（--skip-upload）；产物: ${APK_FILE}"
  exit 0
fi

DEST_NAME="papafeiji-v${VERSION}-test.apk"
echo "==> [4/4] 上传官网: ${DEST_NAME}"
scp -o BatchMode=yes "${APK_FILE}" "${REMOTE_HOST}:${REMOTE_DIR}/${DEST_NAME}"
HTTP_CODE=$(curl -sI -o /dev/null -w '%{http_code}' "${DOWNLOAD_BASE}/${DEST_NAME}")
[[ "${HTTP_CODE}" == "200" ]] || { echo "错误：下载 URL 返回 ${HTTP_CODE}" >&2; exit 1; }
echo "完成: ${DOWNLOAD_BASE}/${DEST_NAME}（HTTP ${HTTP_CODE}）"

# 旧包清理：Android 不支持降级安装，历史测试包无回滚价值；
# 官网 downloads 与本地 OUT_DIR 各只保留刚发布的最新一个，防止无限堆积。
# 清理在 URL 验证 200 之后执行，绝不影响刚发布的包。
ssh -o BatchMode=yes "${REMOTE_HOST}" \
  "find '${REMOTE_DIR}' -maxdepth 1 -name 'papafeiji-v*.apk' ! -name '${DEST_NAME}' -delete"
find "${OUT_DIR}" -maxdepth 1 -name 'com.pathmemos.app-*.apk' ! -name "$(basename "${APK_FILE}")" -delete 2>/dev/null || true
echo "==> 旧包已清理（官网与本地各仅保留 ${DEST_NAME}）"
