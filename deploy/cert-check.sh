#!/usr/bin/env bash
# 证书守卫（服务器侧，cron 每小时，由 deploy.sh 自动装配；CERT_DOMAIN 由 cron 行注入）：
#  1) 证书剩余有效期 < 21 天 → 输出 cert_expiry_soon（由 alert-watch 捕获推送 webhook）；
#  2) 证书文件内容变化（certbot 续期 / 控制机重推）→ 对 nginx 优雅 reload。
# 证书来源自适应（2026-09-23 v2 部署体系）：
#  - certbot 卷模式（deploy.sh CFG_SSL_MODE=2）→ 读 papafeiji_certbot_data 卷内证书；
#  - 上传证书模式（CFG_SSL_MODE=1，deploy-all.sh --ssl-mode certbot/upload 走此路径，
#    证书由控制机 issue-cert.sh DNS-01 签发/续期后重推）→ 读 nginx 容器内
#    /etc/nginx/ssl/cert.pem；
#  - 纯 HTTP（CFG_SSL_MODE=0，无卷且容器内无证书文件）→ 直接退出。
# 无 TLS 部署没有证书数据卷，直接退出。
set -euo pipefail

LOG="${PAPAFEIJI_CERT_LOG:-/var/log/papafeiji-cert-check.log}"
STATE="${PAPAFEIJI_CERT_MD5:-/var/run/papafeiji-cert.md5}"
CERT_DOMAIN="${CERT_DOMAIN:-}"

say() { echo "[$(date '+%F %T')] $*" >> "${LOG}"; }

read_cert() {
  # 优先 certbot 数据卷；无卷则读 nginx 容器内上传的证书（/etc/nginx/ssl/cert.pem）
  if docker volume inspect papafeiji_certbot_data >/dev/null 2>&1 && [[ -n "${CERT_DOMAIN}" ]]; then
    docker run --rm -v papafeiji_certbot_data:/etc/letsencrypt:ro alpine:latest \
      sh -c "test -f /etc/letsencrypt/live/${CERT_DOMAIN}/fullchain.pem && cat /etc/letsencrypt/live/${CERT_DOMAIN}/fullchain.pem" 2>/dev/null && return 0
  fi
  if docker exec papafeiji-nginx test -f /etc/nginx/ssl/cert.pem >/dev/null 2>&1; then
    docker exec papafeiji-nginx cat /etc/nginx/ssl/cert.pem 2>/dev/null && return 0
  fi
  return 1
}

if ! CERT_PEM="$(read_cert)"; then
  # 无任何证书来源：纯 HTTP 部署属正常，静默退出；有域名注入却无文件才告警
  [[ -z "${CERT_DOMAIN}" ]] || say "WARN cert_expiry_soon reason=cert_file_missing domain=${CERT_DOMAIN}"
  exit 0
fi

# 1) 有效期检查
END_LINE="$(printf '%s' "${CERT_PEM}" | docker run --rm -i alpine:latest openssl x509 -enddate -noout 2>/dev/null || true)"
END_TS="$(awk -F'=| GMT' '{print $2}' <<< "${END_LINE}" | tail -1)"
if [[ -n "${END_TS}" ]]; then
  # openssl 输出形如 notAfter=Mon DD HH:MM:SS YYYY GMT
  end_epoch="$(date -d "$(sed -E 's/^[^=]*=//' <<< "${END_LINE}")" +%s 2>/dev/null || echo 0)"
  if [[ "${end_epoch}" -gt 0 ]]; then
    days=$(( (end_epoch - $(date +%s)) / 86400 ))
    if (( days < 21 )); then
      say "WARN cert_expiry_soon domain=${CERT_DOMAIN:-uploaded} days_left=${days}"
    fi
  fi
fi

# 2) 证书变更 → 优雅 reload
CURRENT_MD5="$(printf '%s' "${CERT_PEM}" | md5sum | awk '{print $1}')"
LAST_MD5="$(cat "${STATE}" 2>/dev/null || echo '')"
if [[ "${CURRENT_MD5}" != "${LAST_MD5}" ]]; then
  if docker exec papafeiji-nginx nginx -s reload >/dev/null 2>&1; then
    say "OK cert changed, nginx reloaded domain=${CERT_DOMAIN:-uploaded}"
  else
    say "WARN cert changed but nginx reload failed domain=${CERT_DOMAIN:-uploaded}"
  fi
  echo "${CURRENT_MD5}" > "${STATE}"
fi
