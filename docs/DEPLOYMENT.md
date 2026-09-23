# 部署与运维参考

> 版本：V2.0｜状态：定稿（以当前代码为唯一事实源）。
> 面向 SaaS 与开源版两种形态的容器、环境变量、反向代理、备份与运维工具；部署编排契约另见 `docs/spec/05-development-plan.md` §3。

## 1. 容器与镜像

### 1.1 镜像构建（`deploy/Dockerfile`）

- 多阶段构建：`golang:1.25.12-alpine` 编译 → `alpine:3.21` 运行；构建 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/bin/pathmemos ./backend/cmd/server`。
- 运行镜像内置：`backend/migrations`、静态资源（`invite.png` → `/app/assets/invite-share-cover.png`、`default-cover.jpg`、`person_invite.png`、`wxmp_thumb.jpg`）。
- 运行用户 `appuser`（uid 1000）；`EXPOSE 8080 8081`；`HEALTHCHECK curl -f http://localhost:8080/health/live`。

### 1.2 服务拓扑（`deploy/docker-compose.yml` + `deploy/docker-compose.override.yml`）

| 服务 | 镜像 | 职责与关键设置 |
|------|------|----------------|
| `app` | `papafeiji-app:<commit>` | REST API，监听 `:8080`；`read_only: true`、`cap_drop: ALL` |
| `sse` | 同 `app` 镜像 | AI 流式对话，监听 `:8081`；`read_only: true`、`cap_drop: ALL` |
| `nginx` | `nginx:1.25-alpine` | 反向代理、限流、TLS；对外 `:80`/`:443` |
| `postgres` | `postgres:15-alpine` | 主库；卷 `postgres_data`；挂载 `backend/migrations` 只读 |
| `redis` | `redis:7-alpine` | session/缓存/限流/临时幂等（分布式锁已迁 PostgreSQL advisory lock，ADR-0005）；`volatile-lru`（调优覆盖 `--maxmemory 307mb`） |
| `backup` | `postgres:15-alpine` | 每 6 小时 `pg_dump` + `uploads` 打包，保留最近 7 份（`BACKUP_KEEP` 可调） |
| `certbot` | 由 `deploy/deploy.sh` 生成 | 证书签发与自动续期（证书已存在时跳过申请） |

- `deploy/docker-compose.override.yml` 由 `deploy.sh` 渲染并分发，优先级高于 `docker-compose.yml`：设定各容器 CPU/内存上限、`nofile=65536`，并为 `app`/`sse` 注入 `DB_MAX_CONNS=75`、`DB_MIN_CONNS=5`。禁止在服务器手工修改（部署会覆盖）。
- 生产实际使用 `deploy.sh` 内联生成的 compose（远端 `/opt/papafeiji/docker-compose.yml`）；仓库内 `deploy/docker-compose.yml` 仅作手工部署参考（文件头已注明）。

## 2. 环境变量字典

> 必需变量唯一数据源：`deploy/required-runtime-vars.txt`（`config.go` 的 `validate()` 必须覆盖它；`deploy.sh` 必须为每个变量提供非空来源）。

### 2.1 必需变量（14）

| 变量 | 用途 |
|------|------|
| `DATABASE_URL` | PostgreSQL 连接串 |
| `REDIS_ADDR` | Redis 地址 |
| `WECHAT_APPID` / `WECHAT_SECRET` | 小程序登录凭证 |
| `TENCENT_MAP_KEY` | 腾讯地图 Key（支持多 key，逗号分隔） |
| `AI_API_KEY` | AI 上游密钥 |
| `WECHAT_VIRTUAL_OFFER_ID` | 虚拟支付 offerId |
| `WECHAT_VIRTUAL_APP_KEY_PRODUCTION` / `WECHAT_VIRTUAL_APP_KEY_SANDBOX` | 虚拟支付 appKey（现网/沙箱） |
| `API_HOST` | 对外访问域名（回调与链接拼接） |
| `WECHAT_MSG_TOKEN` | 公众号消息校验 token |
| `WECHAT_VIRTUAL_CALLBACK_TOKEN` / `WECHAT_VIRTUAL_CALLBACK_AES_KEY` | 虚拟支付回调验签与解密 |
| `WORKER_SECRET` | api-worker 中转流量共享密钥（SaaS 必填，空则启动失败；open 模式必须留空） |

### 2.2 服务与运行模式

| 变量 | 默认 | 用途 |
|------|------|------|
| `DEPLOYMENT_MODE` | `saas` | `saas`／`open`；open 模式注册公开 `/mcp/*`。注意：deploy.sh 生成的 SaaS compose **不注入**该变量，运行值依赖代码默认 `saas` |
| `HTTP_BIND` / `HTTP_PORT` | `127.0.0.1` / `8080` | REST 监听地址与端口 |
| `SSE_BIND` / `SSE_PORT` | `127.0.0.1` / `8081` | SSE 监听地址与端口 |
| `LOG_LEVEL` | `INFO` | 日志级别 |
| `TRUSTED_PROXY_CIDR` | 空（SaaS 部署默认注入 `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`，`CFG_TRUSTED_PROXY_CIDR` 可覆盖） | 可信代理网段；用于解析真实客户端 IP |
| `MCP_ENABLED` | `true` | 是否对外启用 MCP 能力（`GET /system/config` 的 `features.mcp`） |

### 2.3 数据库 / Redis

| 变量 | 默认 | 用途 |
|------|------|------|
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | 50 / 10（后台池 2） | 连接池上下限；`min >= 1`、`max >= 5`；生产 override 为 75/5 |
| `REDIS_PASSWORD` | 空 | Redis 密码（compose 与 `redis-server --requirepass`） |
| `REDIS_POOL_SIZE` | 见 `redis` 包默认 | Redis 连接池大小 |

### 2.4 微信 / 公众号

| 变量 | 用途 |
|------|------|
| `WECHAT_MP_APPID` / `WECHAT_MP_SECRET` / `WECHAT_MP_GHID` | 公众号 AppID / Secret / 原始 ID |
| `WECHAT_ENCODING_AES_KEY` | 公众号消息加解密密钥 |
| `WECHAT_MINI_LINK_ENV_VERSION` | 小程序码 env-version（现网/体验/开发） |

### 2.5 虚拟支付

| 变量 | 用途 |
|------|------|
| `PAYMENT_ALLOW_SANDBOX` | 取值 `1` 时允许沙箱下单；生产默认关闭 |

### 2.6 存储（本地 / OSS）

| 变量 | 默认 | 用途 |
|------|------|------|
| `STORAGE_LOCAL_PATH` | `/opt/pathmemos/uploads` | 本地存储目录 |
| `OSS_ACCESS_KEY_ID` / `OSS_ACCESS_KEY_SECRET` | — | 阿里云 OSS 凭证 |
| `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_PUBLIC_URL` | — | OSS 端点 / Bucket / 公网访问前缀 |
| `USER_IMAGE_STORAGE_LIMIT_BYTES` / `..._VIP` | 见 `config.go` | 普通用户 / VIP 图片存储配额 |
| `STORAGE_PUBLIC_BASE_URL` | 空 | 文件对外基址（缺省回退 `OSS_PUBLIC_URL`） |
| `DEFAULT_COVER_IMAGE` / `DEFAULT_AVATAR_URL` / `DEFAULT_TRAJECTORY_ICON` | 空 | 默认封面 / 头像 / 轨迹图标 URL |
| `CDN_REFRESH_ENABLED` | `false` | 已删对象边缘缓存批量收敛开关（ADR-0013）：开启后删除 OSS 对象会记录 URL，后台任务定期调阿里云 CDN 刷新；凭据复用 OSS AccessKey（需授 CDN 刷新权限） |

### 2.7 AI

| 变量 | 默认 | 用途 |
|------|------|------|
| `AI_BASE_URL` / `AI_MODEL` | 空 | AI 上游地址与模型 |
| `AI_MAX_OUTPUT_TOKENS` | 空 | 上游 max_tokens |
| `AI_THINKING_TYPE` | 空 | 上游 thinking.type |
| `AI_PROMPT` | 代码默认 | 小程序系统提示词 |
| `WECHAT_MP_PROMPT` | 空 | 公众号系统提示词；空回退 AI_PROMPT |

### 2.8 MCP / Worker

| 变量 | 用途 |
|------|------|
| `WORKER_SECRET` | api-worker → 源站中转流量的共享密钥（`X-Worker-Secret`，存在 `X-Forwarded-Host` 时校验）；SaaS 必填，open 模式必须留空 |
| `MCP_WORKER_SECRET` | 源站 `/internal/mcp/*` 与 mcp-worker 之间的共享密钥 |
| `MCP_PUBLIC_URL` | 对外 MCP 地址（客户端配置展示） |
| `OPEN_API_KEY` | 开源版默认 API Key（种子身份） |

### 2.9 后台任务间隔

| 变量 | 用途 |
|------|------|
| `JOB_INTERVAL_AUTO_RECORD` | 自动记录成文任务间隔 |
| `JOB_INTERVAL_ABNORMAL_ALERT` | 异常告警扫描间隔 |
| `JOB_INTERVAL_ORDER_CLOSE` | 超时订单关闭间隔 |
| `JOB_INTERVAL_CLEANUP_AI_LOGS` | AI 对话日志清理间隔 |
| `JOB_INTERVAL_CLEANUP_TRAJECTORIES` | 轨迹清理间隔 |
| `JOB_INTERVAL_CLEANUP_ORPHAN_FILES` | 孤儿文件清理间隔 |
| `JOB_INTERVAL_CLEANUP_ORPHAN_TRAJ_MAPS` | 孤儿轨迹图清理间隔 |
| `JOB_INTERVAL_CLEANUP_CLIENT_OPS_LOGS` | 客户端日志清理间隔（默认 24h，删除 >30 天） |
| `JOB_INTERVAL_PURGE_DELETED_OBJECTS` | 已删对象 CDN 刷新间隔（默认 24h；单轮最多 20 批 × 500 URL） |

### 2.10 运维工具专用

| 变量 | 用途 |
|------|------|
| `TARGET_PHONE` | 运维 CLI `cmd/admin` 的入参（一次性运维工具，按手机号删用户，由 `deploy/delete-user.sh` 调用；**非 Admin 管理系统**，无后台路由） |

### 2.11 控制机 `CFG_*` → 容器运行时变量映射（`deploy.sh` 渲染 `.env`）

控制机 `/root/DeployOps/env/.env.papafeiji` 使用 `CFG_*` 命名；部署时由 `deploy.sh` 映射为容器运行时变量。同名直传项不列。

| 控制机变量 | 容器运行时变量 | 备注 |
|------------|----------------|------|
| `PAPAFEIJI_PG_PASSWORD` | `DB_PASSWORD` | postgres/backup/app/sse 共用 |
| `CFG_WECHAT_APPID` / `CFG_WECHAT_SECRET` | `WECHAT_APPID` / `WECHAT_SECRET` | SaaS 必填 |
| `CFG_WECHAT_VIRTUAL_PAY_OFFER_ID` | `WECHAT_VIRTUAL_OFFER_ID` | 注意名称不同 |
| `CFG_WECHAT_VIRTUAL_PAY_APP_KEY_PRODUCTION` / `..._SANDBOX` | `WECHAT_VIRTUAL_APP_KEY_PRODUCTION` / `..._SANDBOX` | |
| `CFG_WECHAT_VIRTUAL_CALLBACK_TOKEN` / `..._AES_KEY` | `WECHAT_VIRTUAL_CALLBACK_TOKEN` / `..._AES_KEY` | |
| `CFG_WECHAT_MSG_TOKEN` / `CFG_WECHAT_ENCODING_AES_KEY` | `WECHAT_MSG_TOKEN` / `WECHAT_ENCODING_AES_KEY` | |
| `CFG_WECHAT_MP_APPID` / `CFG_WECHAT_MP_SECRET` / `CFG_WECHAT_MP_GHID` | `WECHAT_MP_*` | 可选 |
| `CFG_MAP_KEY` | `TENCENT_MAP_KEY` | 多 key 逗号分隔 |
| `CFG_AI_KEY` / `CFG_AI_BASE` / `CFG_AI_MODEL` | `AI_API_KEY` / `AI_BASE_URL` / `AI_MODEL` | |
| `CFG_AI_MAX_OUTPUT_TOKENS` / `CFG_AI_THINKING_TYPE` | `AI_MAX_OUTPUT_TOKENS` / `AI_THINKING_TYPE` | 可选 |
| `CFG_DOMAIN` | `API_HOST` + `STORAGE_PUBLIC_BASE_URL` | 一变二；并写 nginx server_name |
| `CFG_DEFAULT_AVATAR_URL` | `DEFAULT_AVATAR_URL` | 默认 dicebear |
| `CFG_LOG_LEVEL` | `LOG_LEVEL` | 默认 INFO |
| `CFG_TRUSTED_PROXY_CIDR` | `TRUSTED_PROXY_CIDR` | 默认三段私网 CIDR |
| `CFG_WORKER_SECRET` / `CFG_MCP_WORKER_SECRET` / `CFG_MCP_PUBLIC_URL` | `WORKER_SECRET` / `MCP_WORKER_SECRET` / `MCP_PUBLIC_URL` | |
| `CFG_PAYMENT_ALLOW_SANDBOX` | `PAYMENT_ALLOW_SANDBOX` | 默认空（关闭） |
| `OSS_ACCESS_KEY_ID` / `OSS_ACCESS_KEY_SECRET` / `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_PUBLIC_URL` | 同名 | 控制机同名直传 |

## 3. 反向代理与限流（`deploy/nginx/`）

- `default.conf`（SaaS）：`upstream papafeiji_http → papafeiji-app:8080`、`upstream papafeiji_sse → papafeiji-sse:8081`；限流区 `api=20r/s`、`upload=10r/s`；`client_max_body_size 50m`。
- 路由：`/.well-known/acme-challenge/`（证书校验）、`/uploads/`（静态）、`/ai/chat` → SSE、`/api/prod/payment/virtualPayNotify` → app、`/` → app；`location ~ \.\.` 拒绝路径穿越；默认 `return 444`。
- 访问日志双写：持久化到宿主机 `/opt/papafeiji/logs/nginx`，同时保留 `docker logs` 可见性。
- `nginx.open.conf`（开源版）：`upstream pathmemos_http/sse`，额外 `location /system-assets/`，无 SaaS 支付回调 location。
- TLS 证书由 certbot 容器签发/续期，`deploy.sh` 在证书存在时跳过申请。

## 4. 部署与自动合入

- 编排契约（执行顺序、失败处理分类、密钥注入边界、备份与恢复、质量门禁）见 `docs/spec/05-development-plan.md` §3。
- 部署由 `deploy/deploy.sh --branch <feat-branch>` 执行；健康门禁通过后由脚本自动把分支合入 `main` 并推送（`--skip-merge` 可跳过）。
- 分支硬约束：显式拒绝 `--branch main`；要求本地分支与 `origin/<branch>` 指向完全一致（不一致即退出）；rsync 同步排除 `AGENTS.md`/`agent.md`。
- 双锁互斥：本地 `/tmp/papafeiji-deploy.lock`（flock）；远端与 watchdog 共享 `/var/run/papafeiji-watchdog.lock`，部署时以 `-w 300` 最多等待在途看门狗 300 秒。
- 部署期自动运维：远端 nginx 日志 logrotate（daily/保留 14 天/compress）；每日 03:17 cron 对 nginx 发 SIGHUP；Docker 缺失时自动安装（阿里云源）并写入 daemon.json registry-mirrors；部署成功后 `docker builder prune`；首次部署等待 PostgreSQL 就绪（最长 60s）。
- `--miniapp-only` 仅执行小程序上传（跳过远端清理与远程部署）；全量部署时小程序上传与后端部署**并行**，任一失败整体判败。
- SSL 三态（交互配置 `CFG_SSL_MODE`）：`1`=使用自带证书（强制提供 `CFG_DOMAIN_CERT` 与 `CFG_DOMAIN_CERT_KEY` 两条路径）、`2`=certbot 签发（强制 `CFG_SSL_EMAIL`）、其他值=不配置 TLS（仅 80 端口）。
- `AGENTS.md` 或 `docs/` 的变更不应触发部署；实际部署用于代码/配置变更。

## 5. 备份与恢复

- 部署前：`deploy.sh` 每次部署前对生产库做 `pg_dump -Fc`，保存到控制机 `/root/DeployOps/papafeiji-db-backups/`（按 mtime 保留 7 天；postgres 未运行时跳过备份继续部署；备份产出 `pg_restore -l` 校验失败则中止部署）。
- 连续备份：`backup` 容器每 6 小时 `pg_dump` + `uploads` 打包到部署目录 `backups/`，保留最近 7 份。
- schema 级恢复：`scripts/restore-backup.sh <备份文件> --yes`（支持 `.dump` / `.sql.gz` / `.sql`；恢复前自动安全备份并停启应用容器）。恢复失败时保留 restore-safety 备份并**保持 app/sse 停止**，需人工介入排查。
- 迁移回滚（部署失败自动）：`_rollback_and_exit` 先回滚迁移到部署前版本（`migrate goto <prev>`；部署前无迁移则逐条 `migrate down 1`。注意该迁移在 postgres 容器内对 `/tmp/migrations` 执行，与常规迁移路径不同），再恢复 `.env`/nginx/override/migrations/compose 的 `.prev` 快照并重建旧容器，脚本以非零码退出（不重试健康检查，异常容器由 watchdog 兜底）。仅保当次部署；更早版本需 `git checkout <旧 commit>` 重新部署。
- 数据级回滚（人工）：统一走 `restore-backup.sh` + 部署前 `pg_dump` 备份；结构变更遵循 expand → 兼容旧代码 → contract（04 §8.6、05 §3.4、ARCHITECTURE §5.3）。

## 6. 自愈与调优

- `deploy/watchdog.sh`：cron 每分钟检查 `postgres`/`redis`/`app`/`sse`/`nginx` 5 个服务（不含 `backup`/`certbot`），异常强制恢复；`flock` 与 `deploy.sh` 共享，部署期间自动让行。
- `deploy/optimize.sh` / `deploy/optimize-system.sh`：容器 / 宿主机调优脚本，经 SSH 远端执行，默认只打印方案，`--apply`（别名 `-y`）才落地。

## 7. 开源版安装与生命周期

| 脚本 | 用途 |
|------|------|
| `scripts/install.sh` | 一键安装（Ubuntu/macOS）：装 Docker → 下载代码 → 交互配置 → Cloudflare Tunnel → 健康检查 |
| `scripts/init.sh` | 交互式初始化（生成 `.env`、可选 gum TUI） |
| `scripts/expose.sh` | 公网暴露（Named Tunnel，需 Cloudflare Tunnel Token） |
| `scripts/upgrade.sh` | 升级：拉代码 → 备份（`backups/pre_upgrade_*.sql.gz`）→ 重建容器 → 健康检查 |
| `scripts/rollback.sh` | 回滚到上一个发布 commit（open 分支每次发布为单个 commit）；备份为 `backups/pre_rollback_*.sql.gz`；浅克隆仓库无历史可回滚时提示先 `git fetch --unshallow`。两者均**不自动执行 migration down**，需要回迁移时仅提示手工执行 |
| `scripts/backup.sh` | 本地备份 `pg_dump` + `uploads`，`--keep N` / `--output DIR` |
| `scripts/restore-backup.sh` | 数据库恢复（见 §5） |

## 8. 构建/校验工具

| 脚本 | 用途 |
|------|------|
| `scripts/check_sqlc_sync.py` | 校验 sqlc 生成代码的 SELECT 列数与 Scan 目标一致（`make check-sqlc-sync`） |
| `scripts/encrypt-wechat-secret.go` | 生成内置微信 AppID/Secret 的加密密文（见 §9） |
| `scripts/audit-patterns.sh` | 静态扫描可疑模式（信息性，恒 exit 0） |
| `scripts/spec-check.sh` | spec 门禁（§九） |
| `scripts/check_mcp_static_sync.py` | MCP 静态方法双源对账（mcp-worker/lib.ts ↔ backend mcp/server.go 的 tools/prompts 定义；spec-check 提示级 11 项） |
| `deploy/miniapp-uploader/` | 部署期小程序构建/上传工具链（`miniprogram-ci`），由 `deploy.sh` 调用；**不进入小程序运行时、不随用户交付** |

> `deploy/miniapp-uploader` 的传递依赖中可安全修补的已通过 `overrides` 固定到已修补版本（共 20 项：`sharp`/`minimatch`/`phin`/`protobufjs`/`lodash`/`form-data`/`tough-cookie`/`fast-xml-parser`/`@xmldom/xmldom`/`qs`/`svgo`/`adm-zip`/`tmp`/`js-yaml`/`colord`/`@babel-*`，全集见 `deploy/miniapp-uploader/package.json`）；无法兼容升级的少数（`request`/`lodash.template`/`html-minifier`/`image-size` 无补丁；`uuid`/`file-type` 的修复版为破坏性 major/ESM-only）按可容忍风险接受，并在 GitHub Dependabot 记录为 `tolerable_risk`。

## 9. 内置微信密钥（开源版）

- `backend/internal/wechatsecrets` 将开源版默认的微信小程序 AppID/Secret 以 AES-GCM 加密后内置在代码中（密钥由固定盐经 SHA-256 派生），使开源版用户无需手动填写即可运行（取舍见 ADR-0009）。
- 该机制是**轻量混淆**（避免明文直接出现在源码），不提供高强度保密；有心的逆向分析仍可还原。
- `scripts/encrypt-wechat-secret.go` 用于生成替换密文；未初始化时 `DefaultAppID()` / `DefaultSecret()` 返回空字符串。

## 10. 观测与告警口径（最小集）

> 定位：**观测口径，非验收项**（01 §4 性能指标同此定位）。当前无指标采集/告警渠道基建，靠健康探针 + watchdog + 日志关键字兜底。

- 健康探针：`/health/live` 由 Docker HEALTHCHECK 与 watchdog 使用（进程存活）；`/health/ready` DB 故障 → 503（watchdog 判定异常并重建容器），Redis 故障 → 200 + `status: degraded`（不重启，靠日志告警兜底）。
- watchdog：cron 每分钟检查 `postgres`/`redis`/`app`/`sse`/`nginx` 5 个服务，异常强制恢复（见 §6）。
- 告警日志关键字（级别为代码实际值，Warn/Error 混合；接入日志告警时按下表匹配）：

| 关键字 | 级别 | 含义 | 位置 |
|--------|------|------|------|
| `alert=redis_down` | Error | Redis 健康检查失败（5 分钟限频去重） | `cmd/server/main.go` |
| `payment_notify_missing_msg_signature` | Warn | 加密封文缺 msg_signature | `payment/handler.go` |
| `payment_notify_bad_signature` | Warn | 支付回调验签失败 | `payment/handler.go` |
| `payment_notify_decrypt_failed` | Error | 支付回调解密失败（含排障参数日志） | `payment/handler.go` |
| `payment_notify_plaintext_rejected` | Warn | 明文回调被拒绝 | `payment/handler.go` |
| `payment_notify_receive_id_mismatch` | Error | 回调 receive_id 与本小程序 AppID 不匹配（可能配错 AppID） | `payment/handler.go` |
| `payment_notify_amount_missing` / `payment_notify_amount_invalid` | Error | 回调金额缺失/非正数 | `payment/service.go` |
| `payment_notify_amount_mismatch` | Error | 回调金额与标价不一致（照常发货，ADR-0011） | `payment/service.go` |
| `payment_notify_business_rejected` | Warn | 业务性拒绝（终止重试） | `payment/handler.go` |
| `payment_notify_closed_order_reissued` | Error | closed 订单被支付，补记 paid 并补发 | `payment/service.go` |
| `payment_notify_transient_retry` | Error | 瞬时故障待微信重试 | `payment/handler.go` |
| `alert:payment_parse_failed` | Error | 回调 body 解析失败 | `payment/handler.go` |
| `alert:ai_quota_refund_failed` | Error | AI 配额退款失败（4 次重试后放弃） | `ai/service.go` |
| `[ALERT] wx mp all kf segments failed` | Error | 公众号客服消息分段全部发送失败 | `wxmp/handler.go` |

> 支付链路关键字（`payment_notify_*`）涉及资金，接入告警时设最高优先级。
