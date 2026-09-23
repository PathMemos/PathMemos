# 部署与运维参考

> 版本：V2.0｜状态：定稿（以当前代码为唯一事实源）。
> 面向 SaaS 与开源版两种形态的容器、环境变量、反向代理、备份与运维工具；部署编排契约另见 `docs/spec/05-development-plan.md` §3。

## 1. 容器与镜像

### 1.1 镜像构建（`deploy/Dockerfile`）

- 多阶段构建：`golang:1.25.12-alpine` 编译 → `alpine:3.21` 运行；构建 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/bin/pathmemos ./backend/cmd/server`。
- 运行镜像内置：`backend/migrations`、静态资源（`invite.png` → `/app/assets/invite-share-cover.png`、`default-cover.jpg`、`person_invite.png`、`wxmp_thumb.jpg`）。
- 运行用户 `appuser`（uid 1000）；`EXPOSE 8080 8081`；`HEALTHCHECK curl -f http://localhost:8080/health/ready`。

### 1.2 服务拓扑（`deploy/docker-compose.yml` + `deploy/docker-compose.override.yml`）

| 服务 | 镜像 | 职责与关键设置 |
|------|------|----------------|
| `app` | `papafeiji-app:<commit>`（`deploy/docker-compose.yml` 参考文件为 `papafeiji-app:latest`） | REST API，监听 `:8080`；`read_only: true`、`cap_drop: ALL` |
| `sse` | 同 `app` 镜像（`deploy/docker-compose.yml` 参考文件为 `papafeiji-app:latest`） | AI 流式对话，监听 `:8081`；`read_only: true`、`cap_drop: ALL` |
| `nginx` | `nginx:1.25-alpine` | 反向代理、限流、TLS；对外 `:80`/`:443`；**双职责**：`pro.papafeiji.cn`（API 反代）+ `papafeiji.cn`/`www`（官网静态站，只读挂载 `./web` → `/var/www/website`） |
| `postgres` | `postgres:15-alpine` | 主库；卷 `postgres_data`；挂载 `backend/migrations` 只读；`shared_preload_libraries=pg_stat_statements`（迁移 000007 建扩展，供 `scripts/sql-top.sh` 慢 SQL 榜单） |
| `redis` | `redis:7-alpine` | session/缓存/限流/临时幂等（分布式锁已迁 PostgreSQL advisory lock，ADR-0005）；`--maxmemory 307mb` + **`noeviction`**（写满显式报错而非静默淘汰带 TTL 的 session）+ **AOF everysec**（重启后 session 从 AOF 恢复，不再全员掉登录；RDB 快照保留） |
| `backup` | `postgres:15-alpine` | 每 1 小时（`BACKUP_INTERVAL` 默认 3600 秒）`pg_dump` + `uploads` 打包（临时文件成功后原子替换、失败不退出），保留最近 48 份（`BACKUP_KEEP`）；成功时写 `backups/.last_success` 心跳（watchdog 检查，超 2× 间隔输出 `alert=backup_stale`），db/uploads 失败输出 `alert=backup_failed`/`alert=backup_uploads_failed`；`tmpfs` 覆盖镜像 `VOLUME`，不产生悬空匿名卷 |
| `certbot` | 由 `deploy/deploy.sh` 生成 | 证书签发与自动续期（证书已存在时跳过申请）；显式挂载 `certbot_data`/`certbot_www`/`certbot_lib` 覆盖镜像 `VOLUME`，不产生悬空匿名卷 |

- `deploy/docker-compose.override.yml` 由 `deploy.sh` 渲染并分发，优先级高于 `docker-compose.yml`：设定各容器 CPU/内存上限、`nofile=65536`，并为 `app`/`sse` 注入 `DB_MAX_CONNS=75`、`DB_MIN_CONNS=5`、`DB_BG_MAX_CONNS=10`、`DB_BG_MIN_CONNS=2`。禁止在服务器手工修改（部署会覆盖）。
- 生产实际使用 `deploy.sh` 内联生成的 compose（远端 `/opt/papafeiji/docker-compose.yml`）；仓库内 `deploy/docker-compose.yml` 仅作手工部署参考（文件头已注明）。
- PostgreSQL 连接：`max_connections=200`；`app`/`sse` 各创建**请求池**与**后台池**两个独立 pgx pool。请求池上限 `DB_MAX_CONNS`（默认 50，`max>=5`）、下限 `DB_MIN_CONNS`（默认 10，`min>=1`）；后台池上限 `DB_BG_MAX_CONNS`（默认 10）、下限 `DB_BG_MIN_CONNS`（默认 2）。`deploy/docker-compose.override.yml` 对 app/sse 注入请求池 75/5、后台池 10/2。其余连接供迁移、备份容器、运维 CLI 与 advisory lock 持锁使用。**部署硬门槛**：`deploy.sh` 部署前调 `scripts/check-conn-budget.sh` 校验 `2×(DB_MAX_CONNS+DB_BG_MAX_CONNS) ≤ 170`（当前 `2×(75+10)=170`），越界拒绝部署。

## 2. 环境变量字典

> 必需变量唯一数据源：`deploy/required-runtime-vars.txt`（`config.go` 的 `validate()` 必须覆盖它；`deploy.sh` 必须为每个变量提供非空来源）。

### 2.1 必需变量（15）

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
| `MCP_WORKER_SECRET` | 源站 `/internal/mcp/*` 与 mcp-worker 的共享密钥（SaaS 必填，空则启动失败；open 模式留空） |

### 2.2 服务与运行模式

| 变量 | 默认 | 用途 |
|------|------|------|
| `DEPLOYMENT_MODE` | `saas` | `saas`／`open`；open 模式注册公开 `/mcp/*`。注意：deploy.sh 生成的 SaaS compose **不注入**该变量，运行值依赖代码默认 `saas` |
| `HTTP_BIND` / `HTTP_PORT` | `127.0.0.1` / `8080` | REST 监听地址与端口 |
| `SSE_BIND` / `SSE_PORT` | `127.0.0.1` / `8081` | SSE 监听地址与端口 |
| `SSE_MAX_CONNS` | `200` | SSE 全局并发连接上限（单用户并发固定 2） |
| `LOG_LEVEL` | `INFO` | 日志级别 |
| `TRUSTED_PROXY_CIDR` | 空（SaaS 部署默认注入 `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`，`CFG_TRUSTED_PROXY_CIDR` 可覆盖） | 可信代理网段；用于解析真实客户端 IP |
| `MCP_ENABLED` | `true` | 是否对外启用 MCP 能力（`GET /system/config` 的 `features.mcp`） |
| `FREE_VIP_ENABLED` | `true` | 免费领取入口开关（`GET /system/config` 的 `features.freeVip`）；免费活动下线置 `0`，无需发版（VP-13） |

### 2.3 数据库 / Redis

| 变量 | 默认 | 用途 |
|------|------|------|
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | 50 / 10 | 请求池上下限；`min >= 1`、`max >= 5`；生产 override 为 75/5 |
| `DB_BG_MAX_CONNS` / `DB_BG_MIN_CONNS` | 10 / 2 | 后台任务池独立上下限；生产 override 为 10/2 |
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
| `STORAGE_LOCAL_PATH` | `/opt/pathmemos/uploads` | 本地存储目录；SaaS 内联 compose 对 app/sse 显式注入 `/opt/papafeiji/uploads`，代码默认值仅开源版生效 |
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

> SaaS 内联 compose 的 app/sse `environment` 已显式注入上述全部 `JOB_INTERVAL_*`，以及 `CDN_REFRESH_ENABLED`、`SSE_MAX_CONNS`、`USER_IMAGE_STORAGE_LIMIT_BYTES`(`_VIP`)、`AI_PROMPT`、`MCP_ENABLED` 与 §2.5/§2.6/§2.7 的支付/存储/AI 变量。控制机可用同名变量或 `CFG_<NAME>` 覆盖（写入 `.deploy/.env`），未设置时使用代码默认值。

### 2.10 运维工具专用

| 变量 | 用途 |
|------|------|
| `TARGET_PHONE` | 运维 CLI `cmd/admin` 的入参（一次性运维工具，按手机号删用户，由 `deploy/delete-user.sh` 调用）。`cmd/admin` 还支持 `jobs status`（读 Redis 任务观测键）与 `job run <name>`（写 `job:trigger:<name>` 由常驻 app 消费）；**非 Admin 管理系统**，无后台路由 |

### 2.11 控制机 `CFG_*` → 容器运行时变量映射（`deploy.sh` 渲染 `.env`）

控制机 `/root/DeployOps/env/.env.papafeiji` 使用 `CFG_*` 命名；部署时由 `deploy.sh` 映射为容器运行时变量。同名直传项不列。

| 控制机变量 | 容器运行时变量 | 备注 |
|------------|----------------|------|
| `PAPAFEIJI_PG_PASSWORD` | `DB_PASSWORD` | postgres/backup/app/sse 共用 |
| `CFG_WECHAT_APPID` / `CFG_WECHAT_SECRET` | `WECHAT_APPID` / `WECHAT_SECRET` | SaaS 必填 |
| `CFG_WECHAT_VIRTUAL_PAY_OFFER_ID` | `WECHAT_VIRTUAL_OFFER_ID` | 注意名称不同 |
| `CFG_WECHAT_VIRTUAL_PAY_APP_KEY_PRODUCTION` / `..._SANDBOX` | `WECHAT_VIRTUAL_APP_KEY_PRODUCTION` / `..._SANDBOX` | |
| `CFG_VIRTUAL_PAY_PRODUCT_ID_MONTH` / `CFG_VIRTUAL_PAY_PRODUCT_ID_YEAR` | （不注入容器） | 迁移后 `UPDATE vips SET product_id=... WHERE type='month'/'year'` 同步商品 ID |
| `CFG_WECHAT_VIRTUAL_CALLBACK_TOKEN` / `..._AES_KEY` | `WECHAT_VIRTUAL_CALLBACK_TOKEN` / `..._AES_KEY` | |
| `CFG_WECHAT_MSG_TOKEN` / `CFG_WECHAT_ENCODING_AES_KEY` | `WECHAT_MSG_TOKEN` / `WECHAT_ENCODING_AES_KEY` | |
| `CFG_WECHAT_MP_APPID` / `CFG_WECHAT_MP_SECRET` / `CFG_WECHAT_MP_GHID` | `WECHAT_MP_*` | 可选 |
| `CFG_WECHAT_MINI_LINK_ENV_VERSION` | `WECHAT_MINI_LINK_ENV_VERSION` | 默认 `release` |
| `CFG_MAP_KEY` | `TENCENT_MAP_KEY` | 多 key 逗号分隔 |
| `CFG_AI_KEY` / `CFG_AI_BASE` / `CFG_AI_MODEL` | `AI_API_KEY` / `AI_BASE_URL` / `AI_MODEL` | |
| `CFG_AI_MAX_OUTPUT_TOKENS` / `CFG_AI_THINKING_TYPE` | `AI_MAX_OUTPUT_TOKENS` / `AI_THINKING_TYPE` | 可选 |
| `CFG_DOMAIN` | `API_HOST` + `STORAGE_PUBLIC_BASE_URL` | 一变二；并写 nginx server_name |
| `CFG_WEB_DOMAIN` | （不注入容器） | 官网域名（裸域名如 `papafeiji.cn`，`www.` 子域自动推导）；生成官网 nginx server 块、官网证书签发与官网拨测；官网源码 `website/` 在控制机构建后同步到服务器 `./web/` |
| `CFG_DEFAULT_AVATAR_URL` | `DEFAULT_AVATAR_URL` | 默认 dicebear |
| `CFG_LOG_LEVEL` | `LOG_LEVEL` | 默认 INFO |
| `CFG_TRUSTED_PROXY_CIDR` | `TRUSTED_PROXY_CIDR` | 默认三段私网 CIDR |
| `CFG_WORKER_SECRET` / `CFG_MCP_WORKER_SECRET` / `CFG_MCP_PUBLIC_URL` | `WORKER_SECRET` / `MCP_WORKER_SECRET` / `MCP_PUBLIC_URL` | |
| `CFG_PAYMENT_ALLOW_SANDBOX` | `PAYMENT_ALLOW_SANDBOX` | 默认空（关闭） |
| `CFG_ALERT_WEBHOOK_URL` | `ALERT_WEBHOOK_URL` | 告警 webhook（飞书/钉钉格式），服务器侧 alert-cron/alert-p95 与控制机 uptime-check 共用；**强烈建议必配**——未配置时 deploy.sh 响亮警告（不阻断部署，所有者决策 2026-09），告警仅落日志不触达 |
| `CFG_FREE_VIP_ENABLED` | `FREE_VIP_ENABLED` | 可选，默认 true |
| `CFG_BACKUP_KEEP` / `CFG_BACKUP_INTERVAL` | `BACKUP_KEEP` / `BACKUP_INTERVAL` | 可选，默认 48 份 / 3600 秒 |
| `OSS_ACCESS_KEY_ID` / `OSS_ACCESS_KEY_SECRET` / `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_PUBLIC_URL` | 同名 | 控制机同名直传；五项**成组生效**（全配或全不配，部分配置直接报错）；全配时部署前校验 bucket 连通性、不存在则自动创建（public-read）；bucket 信息不可读（`GetBucketInfo` ServerError，最小权限 RAM key 场景）仅警告跳过预检直接上传、不中断，未配置时警告并以无 OSS 模式部署 |
| `CLOUDFLARE_API_TOKEN` | （不注入容器） | Cloudflare API Token（控制机同名导出）：部署前自动装配 API/官网域名 A 记录，后端健康后自动部署 api/mcp Worker 并同步 `WORKER_SECRET`/`MCP_WORKER_SECRET`；未配置时警告并跳过（Worker 回退手动 `npx wrangler deploy`） |
| `CFG_CF_DNS_OVERWRITE` | （不注入容器） | 置 `1` 时允许 deploy.sh 改写已存在但指向别处的 A 记录；默认已有记录只警告不改写（生产 DNS 误切即停机） |

### 2.12 开源版 / 根 compose 专用变量（`.env.example`）

> 供根目录 `docker-compose.yml` 与 `scripts/install.sh` / `init.sh` / `expose.sh` / `upgrade.sh` / `rollback.sh` 使用；SaaS 由 `deploy.sh` 渲染 `.deploy/.env`，不使用本节变量。

| 变量 | 默认 | 用途 |
|------|------|------|
| `DB_USER` / `DB_NAME` | `papafeiji` | 开源版 postgres 账号 / 库名（根 `docker-compose.yml` 硬编码 `papafeiji`；`deploy/optimize.sh` 读取这两个变量）；SaaS 由 `deploy.sh` 固定为 `papafeiji` |
| `DB_PASSWORD` | 空 | postgres 口令（开源版直接写入 `.env`；SaaS 由 `PAPAFEIJI_PG_PASSWORD` 映射，见 §2.11） |
| `NGINX_HTTP_PORT` | `80` | 根 compose nginx 对外 HTTP 端口（`"${NGINX_HTTP_PORT:-80}:80"`）；`upgrade.sh`/`rollback.sh` 健康检查使用同一端口 |
| `CLOUDFLARE_TUNNEL_TOKEN` | 空 | 开源版 Cloudflare Named Tunnel 令牌（`install.sh` / `expose.sh` 写入 `.env`；为空则不启用 Tunnel） |
| `OSS_REGION` | `cn-hangzhou` | 当前后端与脚本均未读取（无运行效果） |

## 3. 反向代理与限流（`deploy/nginx/`）

- `default.conf`（SaaS）：`upstream papafeiji_http → papafeiji-app:8080`、`upstream papafeiji_sse → papafeiji-sse:8081`；限流区 `api=20r/s`、`upload=10r/s`、`web=25r/s`（官网静态，burst 50）；单 IP 并发连接 `perip_api=30`（API/SSE）、`perip_web=30`（官网）；`client_max_body_size 50m`。持续打穿限流者由 fail2ban `papafeiji-nginx-limit` jail 读 error.log 自动封禁 80/443 十分钟。
- 路由：`/.well-known/acme-challenge/`（证书校验）、`/uploads/`（静态）、`/ai/chat` → SSE、`/api/prod/payment/virtualPayNotify` → app、`/` → app；`location ~ \.\.` 拒绝路径穿越；空 `Host` 时 `return 444`（默认 `location /` 反代 app）。
- **官网 server 块**（`deploy.sh` 生成，追加在同一 `nginx.conf` 内，按 Host 与 API 共用 80/443 端口）：`server_name` 为 `CFG_WEB_DOMAIN` + `www.`；`root /var/www/website`；`/_astro/` 永久缓存（immutable 1y），`try_files $uri $uri/ =404` + `error_page 404 /404.html`；保留 ACME webroot。官网 HTML 含内联脚本（主题/语言重定向），官网块**不设** API 的严格 CSP；HTTPS 块会话缓存用独立 zone `SSL_WEB`（与 API 的 `shared:SSL` 不能同名）。三种变体随 `CFG_SSL_MODE` 拼装：`0`=仅 HTTP；`1`=HTTPS+跳转（自带证书须同时覆盖 API 与官网域名）；`2`=先 HTTP 引导，远端签发/命中 `papafeiji.cn` 证书后由 `remote_deploy.sh` 追加 HTTPS+跳转（签发失败非致命，保持 HTTP，DNS 切换后重跑部署自动补签）。
- 访问日志双写：持久化到宿主机 `/opt/papafeiji/logs/nginx`，同时保留 `docker logs` 可见性。
- 应用日志持久化：app/sse 的 `LOG_FILE` 使 `slog` 写 `io.MultiWriter(stdout, file)`，落盘 `/opt/papafeiji/logs/{app,sse}/{app,sse}.log`（容器重建不丢，`docker logs` 同时可见）。
- `nginx.open.conf`（开源版）：`upstream pathmemos_http/sse`，额外 `location /system-assets/`，无 SaaS 支付回调 location。
- TLS 证书由 certbot 容器签发/续期，`deploy.sh` 在证书存在时跳过申请。

## 4. 部署与自动合入

- 编排契约（执行顺序、失败处理分类、密钥注入边界、备份与恢复、质量门禁）见 `docs/spec/05-development-plan.md` §3。
- 部署由 `deploy/deploy.sh --branch <feat-branch>` 执行；健康门禁通过后由脚本自动把分支合入 `main` 并推送（`--skip-merge` 可跳过）。
- 分支硬约束：显式拒绝 `--branch main`；要求本地分支与 `origin/<branch>` 指向完全一致（不一致即退出）；rsync 同步排除 `AGENTS.md`/`agent.md`。
- 双锁互斥：本地 `/tmp/papafeiji-deploy.lock`（flock）；远端与 watchdog 共享 `/var/run/papafeiji-watchdog.lock`，部署时以 `-w 300` 最多等待在途看门狗 300 秒。
- **零停机切换（ADR-0015）**：部署不再 `docker rm -f` 全部容器——分层替换为 `up -d postgres redis backup` → `up -d app sse`（逐容器秒级 stop→start，sse `stop_grace_period: 35s` 让在途流优雅收尾）→ nginx 最后统一处理：未运行才启动、配置相对 `.prev` 有变化才经一次性容器 `nginx -t` 校验后 force-recreate，最后总是 `nginx -s reload`（重新解析 upstream DNS + 排空旧连接）。全程仅 API 秒级瞬断，不再全站硬停机。
- 部署期自动运维：远端 nginx/app/sse 日志 logrotate（daily/保留 14 天/compress）；fail2ban 未装则安装并幂等装配 `papafeiji-nginx-limit` jail（nginx-limit-req 过滤器，60 秒内 20 次 limit_req 打穿即封 10 分钟）；部署自动装配 4 个 cron（带 marker 去重）：watchdog 每分钟自愈、`alert-cron.sh` 每 2 分钟告警扫描、`alert-p95.sh` 每 15 分钟 P95 观测、`cert-check.sh` 每小时证书守卫（到期告警 + 证书变化时 reload，取代原每日 03:17 SIGHUP）；Docker 缺失时自动安装（阿里云源）并写入 daemon.json registry-mirrors；部署成功后 `docker builder prune`；首次部署等待 PostgreSQL 就绪（最长 60s）。
- **Cloudflare DNS 装配**（配置 `CLOUDFLARE_API_TOKEN` 时）：部署早期经 Cloudflare API 校验/创建 API 域名与官网域名(+www) 的 A 记录，指向 `REMOTE_HOST`、DNS-only（certbot webroot 签发要求直连解析）；记录缺失则创建，已存在且一致则跳过，指向别处默认仅警告（`CFG_CF_DNS_OVERWRITE=1` 才改写）；zone 不在 token 账号内或接口失败仅警告不阻断（需 token 具备 Zone:Read + DNS:Edit）。Worker 入口 `api/mcp.pathmemos.com` 的 DNS 由 wrangler `custom_domain` 路由自动创建，不在此列。
- **Cloudflare Worker 自动部署**（`CLOUDFLARE_API_TOKEN` 在且未带 `--skip-workers`）：后端健康门禁通过后串行部署 `api-worker`（`--var SAAS_BACKEND_URL:<CFG_DOMAIN>` 覆写回源 + `secret put WORKER_SECRET`）与 `mcp-worker`（`--var BACKEND_URL:<CFG_DOMAIN>` + `secret put MCP_WORKER_SECRET`）——回源始终指向本次 `CFG_DOMAIN`，全新域名部署无需手改 `wrangler.toml`；部署失败不回退后端但中止后续（不合 main、不上传小程序）；随后拨测 `mcp.pathmemos.com/health`（失败仅警告，边缘传播需数十秒）。手动 `npx wrangler deploy` 保留为 `--skip-workers` 下的兜底。
- **OSS 连接自检与自动建桶**：`OSS_*` 五项成组（语义同后端 `config.validate`，部分配置报错退出）；全配时部署早期经 oss2 校验 bucket 可达，不存在则自动创建（public-read），随后上传 `deploy/assets` 默认静态资源；bucket 信息不可读（`GetBucketInfo` ServerError，最小权限 RAM key 常被拒 bucket 级操作）仅警告跳过预检、直接逐文件上传（真正失败由上传暴露），不中断部署；未配置时响亮警告，默认封面/轨迹图标字段留空（后端无 OSS 模式运行）。
- `--miniapp-only` 仅执行小程序上传（跳过远端清理与远程部署）；**全量发布按兼容性顺序**：先部署向后兼容的后端并通过健康门禁，再自动部署 Cloudflare Worker，最后串行上传小程序；Worker/小程序失败不回退后端，系统停在「新后端 + 旧入口/旧前端」（后端兼容 N-1、服务可用），修复后重跑或用 `--miniapp-only` 重试小程序。
- SSL 三态（交互配置 `CFG_SSL_MODE`）：`1`=使用自带证书（强制提供 `CFG_DOMAIN_CERT` 与 `CFG_DOMAIN_CERT_KEY` 两条路径）、`2`=certbot 签发（强制 `CFG_SSL_EMAIL`）、其他值=不配置 TLS（仅 80 端口）。
- **官网静态站随部署发布**（`website/`，Astro；域名 `CFG_WEB_DOMAIN`，当前 `papafeiji.cn`）：控制机 `npm ci && npm run build`（**构建失败不阻断后端部署**：响亮警告后跳过官网同步，服务器保留上一版 `./web/` 继续服务，官网为非核心静态制品）→ rsync `out/` 到服务器 `./web/`（`--delete` 同步）→ nginx 只读挂载。官网证书独立 lineage（`papafeiji.cn`+`www`，webroot 验证），`remote_deploy.sh` 在证书缺失时尝试签发、失败仅警告；部署末尾以本机 `Host:` 头拨测（`200/301` 为通过，异常仅警告不回滚）。官网内容更新随任意后端部署生效，无需单独发布流程。
- `AGENTS.md` 或 `docs/` 的变更不应触发部署；实际部署用于代码/配置变更。
- `deploy/legacy-migrations/` 为 squash 前旧迁移的归档副本（旧编号系列），不参与部署、无脚本引用，仅供历史追溯。

## 5. 备份与恢复

- 部署前：`deploy.sh` 每次部署前对生产库做 `pg_dump -Fc`，保存到控制机 `/root/DeployOps/papafeiji-db-backups/`（按 mtime 保留 7 天；备份产出 `pg_restore -l` 校验失败则中止部署）。**备份门禁**：仅全新环境（无 `postgres_data` 卷）允许 postgres 未运行时跳过；已有数据卷的存量库无法取得可验证备份时中止部署。
- 连续备份：`backup` 容器每 1 小时 `pg_dump` + `uploads` 打包到部署目录 `backups/`，保留最近 48 份（RPO ≤ 1 小时；心跳与失败告警见 §1.2 backup 行）。
- schema 级恢复：`scripts/restore-backup.sh <备份文件> --yes`（支持 `.dump` / `.sql.gz` / `.sql`；恢复前自动安全备份并停启应用容器）。恢复失败时保留 restore-safety 备份并**保持 app/sse 停止**，需人工介入排查。
- 迁移回滚（部署失败自动）：`_rollback_and_exit` 先回滚迁移到部署前版本（`migrate goto <prev>`；部署前无迁移则逐条 `migrate down 1`。正向迁移同样在 postgres 容器内对 `/tmp/migrations` 执行），再恢复 `.env`/nginx/override/migrations/compose 的 `.prev` 快照并重建旧容器，脚本以非零码退出（不重试健康检查，异常容器由 watchdog 兜底）。仅保当次部署；更早版本需 `git checkout <旧 commit>` 重新部署。
- 数据级回滚（人工）：统一走 `restore-backup.sh` + 部署前 `pg_dump` 备份；结构变更遵循 expand → 兼容旧代码 → contract（04 §8 第 6 条、05 §3.4、ARCHITECTURE §5.3）。

## 6. 自愈与调优

- `deploy/watchdog.sh`：cron 每分钟检查 `postgres`/`redis`/`app`/`sse`/`nginx` 5 个服务（不含 `backup`/`certbot`），异常强制恢复；`flock` 与 `deploy.sh` 共享，部署期间自动让行。退避/熔断：按 1/2/4/8min 退避，连续失败达 5 次时输出一次 `watchdog_circuit_open`，之后停止自动重启、转 15min 慢探测，恢复后清零并记 `watchdog_recovered`。
- watchdog 附带三项资源检查（ADR-0015，只输出告警关键字不自愈）：Redis `used_memory ≥ 90% maxmemory` → `alert=redis_mem_high`；`backups/.last_success` 心跳超 2× 备份间隔（或缺失）→ `alert=backup_stale`；五个核心容器（app/sse/postgres/redis/nginx，backup/certbot 无 CPU 限额不在检查列表）任一 CPU 超其 compose 限额 90% 且连续 3 次探测 → `alert=papafeiji_cpu_high`（回落输出 `alert=papafeiji_cpu_recovered`；限额从容器 `NanoCpus` 动态读取——防挖矿/失控进程的宿主侧观测，容器本身只读 + `cap_drop ALL` + CPU/内存限额，矿马难以存活持久化）。
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
| `scripts/check_mcp_static_sync.py` | MCP 静态方法双源对账（`mcp-worker/src/lib.ts` ↔ `backend/internal/mcp/server.go` 的 tools/prompts 定义；spec-check 提示级 11 项） |
| `scripts/accesslog-p95.sh` | 按路由统计访问日志 P50/P95/P99；P95 超 `SLO_P95_MS`（默认 500）输出 `slo_breach` 并以退出码 2 结束 |
| `scripts/alert-watch.sh` | 扫描日志关键字 → `ALERT_WEBHOOK_URL`（最小告警通道，见 §10） |
| `scripts/check-conn-budget.sh` | 校验 `2×(DB_MAX_CONNS+DB_BG_MAX_CONNS) ≤ 170`（`deploy.sh` 调用） |
| `scripts/alert-cron.sh` | 服务器侧告警扫描入口（cron 每 2 分钟，deploy.sh 自动装配）：汇总 app/sse/backup/certbot 容器日志与 watchdog/cert-check/p95 主机日志喂 `alert-watch.sh`；主机日志走 **offset 增量读取**（首次只看未来、截断自动全量重读），条件恢复后旧行不会重复推送 |
| `scripts/alert-p95.sh` | P95 观测定时入口（cron 每 15 分钟，deploy.sh 自动装配）：跑 `accesslog-p95.sh`，超阈值推 `slo_breach` webhook |
| `deploy/cert-check.sh` | 证书守卫（cron 每小时，deploy.sh 自动装配）：剩余 <21 天输出 `cert_expiry_soon`；证书文件变化时优雅 reload nginx |
| `scripts/uptime-check.sh` | 外部拨测（**控制机**侧，需手工装 cron `*/5 * * * *`）：从控制机探测 API `/health/ready` 与官网首页 `https://${CFG_WEB_DOMAIN}/`（配置 `CFG_WEB_DOMAIN` 时启用，独立状态文件），各自连续 2 次失败推 `alert=uptime_down`，恢复推 `alert=uptime_recovered` |
| `scripts/sql-top.sh` | 慢 SQL 榜单（服务器侧执行）：`pg_stat_statements` 按总耗时 Top N |
| `scripts/ai-cost.sh` | AI 成本观测（服务器侧执行）：按 `msg="ai chat usage"` 日志汇总最近 N 小时 token 用量与按用户 Top |
| `scripts/rehearse-migration.sh` | 迁移预演（控制机侧，部署前可选执行）：最新部署备份恢复进一次性 postgres:15 容器，跑全部正向迁移验证可执行 |
| `deploy/miniapp-uploader/` | 部署期小程序构建/上传工具链（`miniprogram-ci`），由 `deploy.sh` 调用；**不进入小程序运行时、不随用户交付** |
| `.github/workflows/ci.yml` | CI：**backend**（golangci-lint + `go test -race` + `check-sqlc-sync` + `check_mcp_static_sync.py` + 连接预算脚本测试 + `spec-check.sh` + `audit-patterns.sh` 信息性）、**frontend**（`npm run lint`）、**worker**（api-worker / mcp-worker typecheck） |

> `deploy/miniapp-uploader` 的传递依赖中可安全修补的已通过 `overrides` 固定到已修补版本（共 20 项：`sharp`/`minimatch`/`phin`/`protobufjs`/`lodash`/`form-data`/`tough-cookie`/`fast-xml-parser`/`@xmldom/xmldom`/`qs`/`svgo`/`adm-zip`/`tmp`/`js-yaml`/`colord`/`@babel-*`，全集见 `deploy/miniapp-uploader/package.json`）；无法兼容升级的少数（`request`/`lodash.template`/`html-minifier`/`image-size` 无补丁；`uuid`/`file-type` 的修复版为破坏性 major/ESM-only）按可容忍风险接受（Dependabot 告警的处置记录在 GitHub 平台侧，仓库内不可核验）。

## 9. 内置微信密钥（开源版）

- `backend/internal/wechatsecrets` 将开源版默认的微信小程序 AppID/Secret 以 AES-GCM 加密后内置在代码中（密钥由固定盐经 SHA-256 派生），使开源版用户无需手动填写即可运行（取舍见 ADR-0009）。
- 该机制是**轻量混淆**（避免明文直接出现在源码），不提供高强度保密；有心的逆向分析仍可还原。
- `scripts/encrypt-wechat-secret.go` 用于生成替换密文；未初始化时 `DefaultAppID()` / `DefaultSecret()` 返回空字符串。

## 10. 观测与告警口径（最小集）

> 定位：**观测口径，非验收项**（01 §4 性能指标同此定位）。指标采集仍无时序基建；告警通道已随部署自动装配（ADR-0015）：`alert-cron.sh`（每 2 分钟）+ `alert-p95.sh`（每 15 分钟）+ watchdog/cert-check 关键字，经 `ALERT_WEBHOOK_URL`（`CFG_ALERT_WEBHOOK_URL`，强烈建议必配，未配置时 deploy.sh 警告且告警仅落日志）推送；外部拨测 `uptime-check.sh`（探测 API `/health/ready` 与官网首页两个探测面，独立状态文件）在控制机手工装 cron。

- 健康探针：Docker HEALTHCHECK 与 watchdog 使用 `/health/ready`（`deploy.sh` 生成 compose 与 `deploy/Dockerfile` 一致）：DB 故障 → 503 → 容器 unhealthy → watchdog 重建；Redis 故障 → 200 + `status: degraded`（不重启，靠日志告警兜底）。`/health/live`（仅进程存活）与别名 `/health`（等价 ready）仍保留供人工/其他探针使用。
- watchdog：cron 每分钟检查 `postgres`/`redis`/`app`/`sse`/`nginx` 5 个服务，异常强制恢复（见 §6）。
- 任务观测：Runner 在 Redis 记录每任务 `job:last_success` / `job:last_failure` / `job:fail_streak`，每分钟检查 `now - last_success > 3× 周期` 时输出 `job_stale`（由告警通道捕获）；人工补跑 `papafeiji-admin job run <name>`（写触发键，≤1 分钟内由常驻 app 复用锁 + 任务幂等执行），状态查询 `papafeiji-admin jobs status`。
- 访问日志：`AccessLogMiddleware` 输出结构化 JSON（`msg="api access"`，字段 `route`/`status`/`duration_ms`/`request_id`），公开路由（登录、回调、`/system/config`）与鉴权路由均记录；`scripts/accesslog-p95.sh` 按固定窗口统计分位，核心接口 P95 > `SLO_P95_MS` 时输出 `slo_breach`。
- 告警通道（最小实现）：`scripts/alert-watch.sh` 扫描日志中的资金/数据完整性/可用性关键字，命中且超过去重窗口时 POST 到 `ALERT_WEBHOOK_URL`（同类关键字窗口去重，默认 300s）。扫描入口 `scripts/alert-cron.sh` 由 deploy.sh 装配为每 2 分钟 cron（覆盖 app/sse/backup/certbot 容器日志与 watchdog/cert-check/p95 主机日志）；未配置 webhook 时只打印不发送。
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
| `alert:ai_quota_refund_failed` | Error | AI 配额退款失败（共 4 次尝试后放弃） | `ai/service.go` |
| `[ALERT] wx mp all kf segments failed` | Error | 公众号客服消息分段全部发送失败 | `wxmp/handler.go` |
| `alert=ai_upstream_error` | Error | AI 上游连接失败/非 200/流中断（5 分钟限频去重） | `ai/service.go` |
| `alert=auto_record_failed` | Error | 自动记录轮次存在用户级失败（轨迹保留待下轮重试） | `autorecord/service.go` |
| `alert=backup_failed` | Error | 备份容器 pg_dump 失败 | backup 容器 entrypoint |
| `alert=backup_uploads_failed` | Error | 备份容器 uploads 打包失败 | backup 容器 entrypoint |
| `alert=backup_stale` | Error | 备份心跳缺失或超 2× 间隔 | `watchdog.sh` |
| `alert=redis_mem_high` | Error | Redis 内存 ≥90% maxmemory（noeviction 下写满将显式报错） | `watchdog.sh` |
| `alert=papafeiji_cpu_high` / `alert=papafeiji_cpu_recovered` | Error/Info | 核心容器（app/sse/postgres/redis/nginx）CPU 超其 compose 限额 90% 且连续 3 次探测（防挖矿/失控进程）/ 回落恢复 | `watchdog.sh` |
| `watchdog_circuit_open` | Error | 容器连续失败达 5 次熔断：停止自动重启、转 15min 慢探测，等待人工介入 | `watchdog.sh` |
| `job_stale` | Error | 后台任务 `job:last_success` 超 3× 周期未更新 | `jobs/runner.go` |
| `slo_breach` | Error | 路由 P95 超 `SLO_P95_MS`（默认 500ms） | `alert-p95.sh` |
| `cert_expiry_soon` | Warn | TLS 证书剩余 <21 天 | `cert-check.sh` |
| `alert=uptime_down` / `alert=uptime_recovered` | Error/Info | 控制机外部拨测失败/恢复 | `uptime-check.sh` |

> 支付链路关键字（`payment_notify_*`）涉及资金，优先级最高。
> 破坏性演练（Redis 故障注入等）仍不在常规清单；迁移预演与部署失败自动回滚见 §8/§5。
