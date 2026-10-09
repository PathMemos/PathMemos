# 部署与运维参考

> 版本：当前（以代码为唯一事实源）。
> 面向 SaaS 与开源版两种形态的容器、环境变量、反向代理、备份与运维工具；部署编排契约即本文 §4/§5/§8（`docs/spec/05-development-plan.md` §3 已声明以本文为唯一权威源，本句不构成双权威源）。

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
| `nginx` | `nginx:1.25-alpine` | 反向代理、限流、TLS；对外 `:80`；`:443` 映射与 TLS 证书卷由 `deploy.sh` 内联生成的 compose 注入（`CFG_SSL_MODE` 三态，本参考文件仅含 `:80`）；**双职责**：`pro.papafeiji.cn`（API 反代）+ `papafeiji.cn`/`www`（官网静态站，只读挂载 `./web` → `/var/www/website`） |
| `postgres` | `postgres:15-alpine` | 主库；卷 `postgres_data`；挂载 `backend/migrations` 只读；`shared_preload_libraries=pg_stat_statements`（迁移 000007 建扩展，供 `scripts/sql-top.sh` 慢 SQL 榜单） |
| `redis` | `redis:7-alpine` | session/缓存/限流/临时幂等（分布式锁已迁 PostgreSQL advisory lock，ADR-0005）；`--maxmemory 307mb` + **`noeviction`**（写满显式报错而非静默淘汰带 TTL 的 session）+ **AOF everysec**（重启后 session 从 AOF 恢复，不再全员掉登录；RDB 快照保留）。容量假设：session 键（`session:*`/`session:abs:*`/`sessions:user:*` 三键/会话，多端与重登使会话数 ≈ 1.5–2 × 活跃用户）+ MCP/AI 缓存与 `ai:turn`/`ai:reply` 等短 TTL 瞬态键——当前用户量级余量充足；逼近 90% 水位由 `redis_mem_high` 提前告警，扩容路径 = 同步调大 maxmemory 与宿主内存（单实例，无分片计划） |
| `backup` | `postgres:15-alpine` | 每 1 小时（`BACKUP_INTERVAL` 默认 3600 秒）`pg_dump` + `uploads` 打包（临时文件成功后原子替换、失败不退出），保留最近 48 份（`BACKUP_KEEP`）；成功时写 `backups/.last_success` 心跳（watchdog 检查，超 2× 间隔输出 `alert=backup_stale`），db/uploads 失败输出 `alert=backup_failed`/`alert=backup_uploads_failed`；`tmpfs` 覆盖镜像 `VOLUME`，不产生悬空匿名卷 |
| `certbot` | 由 `deploy/deploy.sh` 生成 | 证书签发与自动续期（证书已存在时跳过申请）；显式挂载 `certbot_data`/`certbot_www`/`certbot_lib` 覆盖镜像 `VOLUME`，不产生悬空匿名卷 |

- `deploy/docker-compose.override.yml` 由 `deploy.sh` 渲染并分发，优先级高于 `docker-compose.yml`：保留 `shm_size`、`nofile=65536` 调优，并**整体重写 redis `command`**（`--maxmemory 307mb`、noeviction、AOF everysec、RDB save、requirepass——§1.2 redis 行的 307mb 以此为生效值，`deploy/docker-compose.yml` 参考文件为 384mb），为 `app`/`sse` 注入 `DB_MAX_CONNS=75`、`DB_MIN_CONNS=5`、`DB_BG_MAX_CONNS=10`、`DB_BG_MIN_CONNS=2`；**不设 CPU/内存限额**（生产栈可成长占用演示项目限额让出的全部资源，见 override 文件头注）。禁止在服务器手工修改（部署会覆盖）。
- 生产实际使用 `deploy.sh` 内联生成的 compose（远端 `/opt/papafeiji/docker-compose.yml`）；仓库内 `deploy/docker-compose.yml` 仅作手工部署参考（文件头已注明）。
- PostgreSQL 连接：`max_connections=200`；`app`/`sse` 各创建**请求池**与**后台池**两个独立 pgx pool。请求池上限 `DB_MAX_CONNS`（默认 50，`max>=5`）、下限 `DB_MIN_CONNS`（默认 10，`min>=1`）；后台池上限 `DB_BG_MAX_CONNS`（默认 10）、下限 `DB_BG_MIN_CONNS`（默认 2）。`deploy/docker-compose.override.yml` 对 app/sse 注入请求池 75/5、后台池 10/2。其余连接供迁移（含迁移专用会话级锁）、备份容器与运维 CLI 使用；advisory lock 不构成独立连接组成——事务级 `pg_advisory_xact_lock` 在发起方业务池连接的事务内持有，会话级任务锁占用的就是后台池连接本身（ADR-0005 两类锁边界）。**部署硬门槛**：`deploy.sh` 部署前调 `scripts/check-conn-budget.sh` 校验 `2×(DB_MAX_CONNS+DB_BG_MAX_CONNS) ≤ 170`（当前 `2×(75+10)=170`），越界拒绝部署。

## 2. 环境变量字典

> 必需变量唯一数据源：`deploy/required-runtime-vars.txt`（`config.go` 的 `validate()` 必须覆盖它；`deploy.sh` 必须为每个变量提供非空来源）。

### 2.1 必需变量（必填 15 + 可选 2，同表登记；可选两项为 DONUT 运行时变量）

> 除下表（`config.validate` 第一层 + `required-runtime-vars.txt`）外，`BuildSysConfig → SysConfig.validate` 另有**第二层启动校验**（两种模式同，不在 txt 清单）：`AI_BASE_URL`、`AI_MODEL`、`STORAGE_PUBLIC_BASE_URL`（open 经 `API_HOST` 覆盖满足）、`DEFAULT_COVER_IMAGE`、`DEFAULT_TRAJECTORY_ICON`、`DEFAULT_AVATAR_URL`（须 `?seed=` 结尾）——缺失任一启动即失败；SaaS 部署链均已渲染注入，但其中 `DEFAULT_COVER_IMAGE`/`DEFAULT_TRAJECTORY_ICON` 的注入依赖 `OSS_PUBLIC_URL` 非空（未配 OSS 时渲染为空值，见 §2.11 的启动失败约束）。

| 变量 | 用途 |
|------|------|
| `DATABASE_URL` | PostgreSQL 连接串 |
| `REDIS_ADDR` | Redis 地址 |
| `WECHAT_APPID` / `WECHAT_SECRET` | 小程序登录凭证 |
| `TENCENT_MAP_KEY` | 腾讯地图 Key（支持多 key，逗号分隔） |
| `AI_API_KEY` | AI 上游密钥 |
| `WECHAT_VIRTUAL_OFFER_ID` | 虚拟支付 offerId |
| `WECHAT_VIRTUAL_APP_KEY_PRODUCTION` / `WECHAT_VIRTUAL_APP_KEY_SANDBOX` | 虚拟支付 appKey（现网/沙箱） |
| `DONUT_APPID` / `DONUT_APPSECRET` | 多端应用（Donut）凭据，`/auth/login/app` 换取 openapp_info 用。**可选运行时变量**：不阻断启动，未配置仅该端点 500（控制机经 `CFG_DONUT_APPID`/`CFG_DONUT_APPSECRET` 渲染注入；不在 `required-runtime-vars.txt` 启动校验清单内） |
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
| `WECHAT_ENCODING_AES_KEY` | 公众号消息加解密密钥。**SaaS 必配**：已配置时公众号回调强制安全模式（明文推送 403，02g P-1）；留空仅允许开源极简部署（明文签名可被三元组重放，生产不得留空） |
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
| `STORAGE_PUBLIC_BASE_URL` | 空（saas 必填，部署经 `CFG_DOMAIN` 注入） | 文件/系统资源对外基址。**无 `OSS_PUBLIC_URL` 回退**（`SysConfig.validate` 空即启动失败）；`OSS_PUBLIC_URL` 是 OSS 模式文件 URL 的独立前缀（storage.URL 直用），不作为本变量回退 |
| `DEFAULT_COVER_IMAGE` / `DEFAULT_AVATAR_URL` / `DEFAULT_TRAJECTORY_ICON` | 空 | 默认封面 / 头像 / 轨迹图标 URL |
| `CDN_REFRESH_ENABLED` | `false` | 已删对象边缘缓存批量收敛开关（ADR-0013）：开启后删除 OSS 对象会记录 URL，后台任务定期调阿里云 CDN 刷新；凭据复用 OSS AccessKey（需授 CDN 刷新权限）。仅接了 CDN 的部署形态（私有）有意义——SaaS 为 OSS 直连无边缘层，保持关闭 |

### 2.7 AI

| 变量 | 默认 | 用途 |
|------|------|------|
| `AI_BASE_URL` / `AI_MODEL` | 空 | AI 上游地址与模型 |
| `AI_MAX_OUTPUT_TOKENS` | 空 | 上游 max_tokens；**生产建议必配**（输出 token 成本的唯一上限闸，未配置时上游按模型默认输出、成本敞口无界，deploy.sh 未配置时于控制机告警——警告仅在写 `.env` 前输出到 stderr，不落入 `.env` 文件） |
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
| `TARGET_PHONE` | 运维 CLI `cmd/admin` 的入参（一次性运维工具，按手机号删用户，由 `deploy/delete-user.sh` 调用）。`cmd/admin` 还支持 `jobs status`（读 Redis 任务观测键）与 `job run <name>`（写 `job:trigger:<name>` 由两容器的 Runner 竞争消费）；**非 Admin 管理系统**，无后台路由 |

### 2.11 控制机 `CFG_*` → 容器运行时变量映射（`deploy.sh` 渲染 `.env`）

控制机 `/root/DeployOps/env/.env.papafeiji` 使用 `CFG_*` 命名；部署时由 `deploy.sh` 映射为容器运行时变量。同名直传项不列。

**env 加载链**：env 文件存在即自动加载（默认 `/root/DeployOps/env/.env.papafeiji`，`PAPAFEIJI_ENV_FILE` 可覆盖路径），逐行安全解析（不 source、值不做命令/变量展开、仅填充未设置的键），命令行显式传参优先于 env 文件。SSH 认证：`REMOTE_PASS` 留空时自动改用密钥认证并在参数校验阶段做 BatchMode 预检。

| 控制机变量 | 容器运行时变量 | 备注 |
|------------|----------------|------|
| `PAPAFEIJI_PG_PASSWORD` | `DB_PASSWORD` | postgres/backup/app/sse 共用 |
| `CFG_WECHAT_APPID` / `CFG_WECHAT_SECRET` | `WECHAT_APPID` / `WECHAT_SECRET` | SaaS 必填 |
| `CFG_WECHAT_VIRTUAL_PAY_OFFER_ID` | `WECHAT_VIRTUAL_OFFER_ID` | 注意名称不同 |
| `CFG_WECHAT_VIRTUAL_PAY_APP_KEY_PRODUCTION` / `..._SANDBOX` | `WECHAT_VIRTUAL_APP_KEY_PRODUCTION` / `..._SANDBOX` | |
| `CFG_VIRTUAL_PAY_PRODUCT_ID_MONTH` / `CFG_VIRTUAL_PAY_PRODUCT_ID_YEAR` | （不注入容器） | 迁移后 `UPDATE vips SET product_id=... WHERE type='month'/'year'` 同步商品 ID |
| `CFG_WECHAT_VIRTUAL_CALLBACK_TOKEN` / `..._AES_KEY` | `WECHAT_VIRTUAL_CALLBACK_TOKEN` / `..._AES_KEY` | |
| `CFG_WECHAT_MSG_TOKEN` / `CFG_WECHAT_ENCODING_AES_KEY` | `WECHAT_MSG_TOKEN` / `WECHAT_ENCODING_AES_KEY` | AES Key SaaS 必配（强制安全模式，见 §2.4） |
| `CFG_WECHAT_MP_APPID` / `CFG_WECHAT_MP_SECRET` / `CFG_WECHAT_MP_GHID` | `WECHAT_MP_*` | 可选 |
| `CFG_WECHAT_MINI_LINK_ENV_VERSION` | `WECHAT_MINI_LINK_ENV_VERSION` | 默认 `release` |
| `CFG_EXTRA_DOMAIN` | （不注入容器） | 可选；额外证书 SAN 域（如同时签 www/旧域名）：未配置时补空默认，配置后进 certbot 双 SAN 签发（仅 SSL 模式 `2` 全新签发路径生效）并写入模式 `1` 的 API 80 跳转 `server_name`（模式 `2` 的 80 跳转不含该域，ACME 校验经 default_server 兜底） |
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
| `CFG_DONUT_APPID` / `CFG_DONUT_APPSECRET` | `DONUT_APPID` / `DONUT_APPSECRET` | 可选，多端应用（Donut）凭据；未配置时 `/auth/login/app` 500 |
| `CFG_ALERT_WEBHOOK_URL` | `ALERT_WEBHOOK_URL` | 告警 webhook（飞书/钉钉格式），服务器侧 alert-cron/alert-p95 与控制机 uptime-check 共用；**强烈建议必配**——未配置时 deploy.sh 响亮警告（不阻断部署），告警仅落日志不触达 |
| `CFG_FREE_VIP_ENABLED` | `FREE_VIP_ENABLED` | 可选，默认 true |
| `CFG_BACKUP_KEEP` / `CFG_BACKUP_INTERVAL` | `BACKUP_KEEP` / `BACKUP_INTERVAL` | 可选，默认 48 份 / 3600 秒 |
| `OSS_ACCESS_KEY_ID` / `OSS_ACCESS_KEY_SECRET` / `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_PUBLIC_URL` | 同名 | 控制机同名直传；**部署侧五项必配**（部分配置报错退出；**全部未配同样报错退出**——fail-fast，无 OSS 的 SaaS 部署在健康门禁必然失败并整轮回滚，与其部署到一半才发现不如预检直接拒绝；后端 `config.validate` 口径为「五项任一非空（含单独 `OSS_PUBLIC_URL`）即要求四项核心齐备」，部署侧更严格、二者不冲突）；全配时部署前校验 bucket 连通性、不存在则自动创建（public-read）；bucket 信息不可读（`GetBucketInfo` ServerError，最小权限 RAM key 场景）仅警告跳过预检直接上传、不中断部署。（open 模式经 `API_HOST` 填充不受影响。）**`OSS_PUBLIC_URL` 空值回退**：OSS 四件套已配置（`config.go` `OSSConfigured`）时 `OSS_PUBLIC_URL` 为空不报错，`OSSPublicURLBase()` 回退拼接 `https://<bucket>.<endpoint>` 作文件 URL 基址；四件套未配置则不走 OSS 分支（本地存储路径下 `file/storage.go` `URL()` 对空基址显式报错「storage base url not configured」）——不存在「启动能过、运行期坏 URL」窗口 |
| `CLOUDFLARE_API_TOKEN` | （不注入容器） | Cloudflare API Token（控制机同名导出；历史键 `CFG_CLOUDFLARE_API_TOKEN` 亦兼容读取，显式同名键优先）：部署前自动装配 API/官网域名 A 记录，后端健康后自动部署 api/mcp Worker 并同步 `WORKER_SECRET`/`MCP_WORKER_SECRET`；未配置时警告并跳过（Worker 回退手动 `npx wrangler deploy`） |
| `CFG_CF_DNS_OVERWRITE` | （不注入容器） | 置 `1` 时允许 deploy.sh 改写已存在但指向别处的 A 记录；默认已有记录只警告不改写（生产 DNS 误切即停机） |
| `CFG_SSH_STRICT_HOST_KEY_CHECKING` | （不注入容器） | 可选，ssh `StrictHostKeyChecking` 覆盖（deploy.sh / optimize.sh / optimize-system.sh 三处 scp/ssh 共用），默认 `accept-new`；已知主机指纹变更的受控场景可临时置 `no` |

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

- `default.conf`（SaaS）：`upstream papafeiji_http → papafeiji-app:8080`、`upstream papafeiji_sse → papafeiji-sse:8081`；**代理头防伪造链**：nginx 覆写 `X-Real-IP=$remote_addr`、`X-Forwarded-For=$proxy_add_x_forwarded_for`（追加而非盲信客户端值），后端 `TRUSTED_PROXY_CIDR` 仅信任私网代理段解析真实 IP——应用层 IP 限流的防伪造前提由该配置成立。限流区 `api=20r/s`、`upload=10r/s`、`web=25r/s`（官网静态，burst 50）；单 IP 并发连接 `perip_api=30`（API/SSE）、`perip_web=30`（官网）；`client_max_body_size 50m`。持续打穿限流者由 fail2ban `papafeiji-nginx-limit` jail 读 error.log 自动封禁 80/443 十分钟。家迹 home.papafeiji.cn 443 回退块由 deploy.sh 在共享卷家迹证书在位时条件追加（`deploy/nginx/home.vhost.conf`；nginx 启动硬校验证书文件，无条件生成会让证书缺失的机器整个网关起不来；小程序正式入口为 9443 直连，此块仅浏览器回退）。
- 路由：`/.well-known/acme-challenge/`（证书校验）、`/uploads/`（静态）、`/ai/chat` → SSE、`/api/prod/payment/virtualPayNotify` → app、`/` → app；`location ~ \.\.` 拒绝路径穿越；空 `Host` 时 `return 444`（默认 `location /` 反代 app）。
- **官网 server 块**（`deploy.sh` 生成，追加在同一 `nginx.conf` 内，按 Host 与 API 共用 80/443 端口）：`server_name` 为 `CFG_WEB_DOMAIN` + `www.`；`root /var/www/website`；`/_astro/` 永久缓存（immutable 1y），`try_files $uri $uri/ =404` + `error_page 404 /404.html`；保留 ACME webroot。官网 HTML 含内联脚本（主题/语言重定向），官网块**不设** API 的严格 CSP；HTTPS 块会话缓存用独立 zone `SSL_WEB`（与 API 的 `shared:SSL` 不能同名）。三种变体随 `CFG_SSL_MODE` 拼装：`0`=仅 HTTP；`1`=HTTPS+跳转（自带证书须同时覆盖 API 与官网域名）；`2`=先 HTTP 引导，远端签发/命中 `papafeiji.cn` 证书后由 `remote_deploy.sh` 追加 HTTPS+跳转（签发失败非致命，保持 HTTP，DNS 切换后重跑部署自动补签）。
- 访问日志双写：持久化到宿主机 `/opt/papafeiji/logs/nginx`，同时保留 `docker logs` 可见性。
- 应用日志持久化：app/sse 的 `LOG_FILE` 使 `slog` 写 `io.MultiWriter(stdout, file)`，落盘 `/opt/papafeiji/logs/{app,sse}/{app,sse}.log`（容器重建不丢，`docker logs` 同时可见）。
- `nginx.open.conf`（开源版）：`upstream pathmemos_http/sse`，额外 `location /system-assets/`，无 SaaS 支付回调 location。
- TLS 证书由 certbot 容器签发/续期，`deploy.sh` 在证书存在时跳过申请。

## 4. 部署与自动合入

- 编排契约（执行顺序、失败处理分类、密钥注入边界、备份与恢复、质量门禁）即本文 §4/§5/§8；`docs/spec/05-development-plan.md` §3 已声明以本文为唯一权威源、不复述。
- 部署由 `deploy/deploy.sh --branch <feat-branch>` 执行；健康门禁通过后由脚本自动把分支合入 `main` 并推送（`--skip-merge` 可跳过）。**deploy.sh 本体的修改必须经真实部署验证**——无脚本级测试为有意取舍（低频变更路径，模块化/测试成本高于收益）。
- 分支硬约束：部署 `main`（无论隐式检出还是显式 `--branch main`）一律需 `FORCED_MAIN=true` 放行（全新服务器一键部署场景；脚本不区分是否显式传参，同一检查）；要求本地分支与 `origin/<branch>` 指向完全一致（不一致即退出）；rsync 同步排除 `AGENTS.md`/`agent.md`。
- 参数全集：`--branch <feat>`（必选之一）、`--skip-miniapp`/`--backend-only`（跳过小程序上传）、`--miniapp-only`（仅前端）、`--skip-workers`（跳过 Worker 部署，DNS 装配仍执行）、`--skip-merge`（不自动合入 main）、`--skip-checks`（跳过本地门禁）、`--optimize`（部署完成后远端执行 `optimize.sh --apply`，见 §6）、`--auto-align-schema-version`（squash 后旧库 `schema_migrations` 版本高于迁移目录最大编号时，备份后自动对齐 `version=1`）。全量部署（含体验版上传）另需控制机环境提供 `MINIAPP_CI_KEY`（CI 上传私钥路径；缺省回退 `<仓库>/deploy/private.<appid>.key`，私钥被 gitignore 挡在构建树外，控制机直接用仓库路径即可）。
- 双锁互斥：本地 `/tmp/papafeiji-deploy.lock`（flock）；远端与 watchdog 共享 `/var/run/papafeiji-watchdog.lock`，部署时以 `-w 300` 最多等待在途看门狗 300 秒。
- **零停机切换（ADR-0015）**：部署不再 `docker rm -f` 全部容器——分层替换为 `up -d postgres redis backup` → `up -d app sse`（逐容器秒级 stop→start，app 与 sse 均配 `stop_grace_period: 35s`，让 app 的 Shutdown(30s) 优雅关闭在途请求（如 5min 上传窗口）、sse 在途流收尾，不被默认 10s grace 截断）→ nginx 最后统一处理：未运行才启动、配置相对 `.prev` 有变化才经一次性容器 `nginx -t` 校验后 force-recreate，最后总是 `nginx -s reload`（重新解析 upstream DNS + 排空旧连接）。全程仅 API 秒级瞬断，不再全站硬停机。
- 部署期自动运维：远端 nginx/app/sse 日志 logrotate（daily/保留 14 天/compress）；主机级配置（fail2ban 双 jail + 内核加固 BBR/sysctl + I/O 调度/udev + swap）统一经 `deploy/bootstrap-host.sh` **一次性引导**（标记文件 `/var/lib/papafeiji/host-bootstrap.done` 在位即跳过，`FORCE_HOST_BOOTSTRAP=1` 强制重放，内部逐步幂等）——日常部署不再重放全局配置（同机多项目共存）；部署自动装配 4 个 cron（带 marker 去重，crontab 读改写持主机级锁 `/var/run/host-crontab.lock`、与家迹 deploy 互斥；其中 cert-check 仅 `CFG_SSL_MODE=1/2` 装配，纯 HTTP 部署实际 3 个）：watchdog 每分钟自愈、`alert-cron.sh` 每 2 分钟告警扫描、`alert-p95.sh` 每 15 分钟 P95 观测、`cert-check.sh` 每小时证书守卫（三模式自适应，见 §8；到期告警 + 证书变化时 reload，取代原每日 03:17 SIGHUP）。**目标机初始化（冷部署幂等）**：Docker 缺失时优先离线安装（控制机 `/root/DeployOps/docker-offline/` 经 rsync 分发，远端解包 docker/compose/buildx 并写入 systemd unit），无离线包再走阿里云源在线安装；`daemon.json` 写入 registry-mirrors + `live-restore: true`，每次部署 `systemctl restart docker`（daemon.json 写入幂等；live-restore 下容器不中断）并等待 `docker info` 就绪（最长 30s，live-restore 首次开启不支持热载）；主机级引导 `bootstrap-host.sh`（fail2ban `papafeiji-nginx-limit` jail：nginx-limit-req 过滤器 60 秒内 20 次打穿封 10 分钟；sshd jail maxretry 5/findtime 600s/bantime 3600s；内核加固 BBR+fq、sysctl 基线、I/O 调度器 none + udev 规则、4G swap）；compose 顶层 certbot 三卷显式 `external: true`（固定名 `papafeiji_certbot_data/www/lib`，远端 `docker volume create` 预创建）——防 `down -v` 误删与家迹共享的证书卷；**基座镜像离线打包**：postgres/redis/nginx/alpine 基座镜像打 tar 缓存于真仓库 `.deploy/`（持久、gitignore）并随 rsync 分发，远端按 manifest tag「已存在即跳过」装载（全新机不再从外网 registry 拉取）；另有第三条镜像通道——**基础镜像随行包** `docker-offline/infra-images.tgz.gz`（控制机 `images-prepare.sh` 制备，含 postgres/redis/nginx/qdrant/certbot/alpine 全集，随 docker-offline rsync 上新机整体 `docker load`，免公共源拉取）。其余：日志 logrotate、部署成功后 `docker builder prune` 与 `docker volume prune`（清理悬空匿名卷）、首次部署等待 PostgreSQL 就绪（最长 60s）。
- **Cloudflare DNS 装配**（配置 `CLOUDFLARE_API_TOKEN` 时）：部署早期经 Cloudflare API 校验/创建 API 域名与官网域名(+www) 的 A 记录，指向 `REMOTE_HOST`、DNS-only（certbot webroot 签发要求直连解析）；记录缺失则创建，已存在且一致则跳过，指向别处默认仅警告（`CFG_CF_DNS_OVERWRITE=1` 才改写）；zone 不在 token 账号内或接口失败仅警告不阻断（需 token 具备 Zone:Read + DNS:Edit）。Worker 入口 `api/mcp.pathmemos.com` 的 DNS 由 wrangler `custom_domain` 路由自动创建，不在此列。
- **Cloudflare Worker 自动部署**（`CLOUDFLARE_API_TOKEN` 在且未带 `--skip-workers`）：后端健康门禁通过后串行部署 `api-worker`（`--var SAAS_BACKEND_URL:<CFG_DOMAIN>` 覆写回源 + `secret put WORKER_SECRET`）与 `mcp-worker`（`--var BACKEND_URL:<CFG_DOMAIN>` + `secret put MCP_WORKER_SECRET`）——回源始终指向本次 `CFG_DOMAIN`，全新域名部署无需手改 `wrangler.toml`；部署失败不回退后端但中止后续（不合 main、不上传小程序）；随后拨测 `mcp.pathmemos.com/health`（失败仅警告、不回滚不阻断——**有意设计**：边缘传播需数十秒，即时拨测会把正常部署误判为失败；坏版本的处置=人工 `wrangler rollback <version-id>`，见 mcp-worker-ops.md）。手动 `npx wrangler deploy` 保留为 `--skip-workers` 下的兜底。
- **OSS 连接自检与自动建桶**：`OSS_*` 五项必配——部分配置报错退出，**全部未配同样报错退出**（fail-fast，见 §2.11；后端 `config.validate` 为「任一非空要求四项齐备」，部署侧更严格）；全配时部署早期经 oss2 校验 bucket 可达，不存在则自动创建（public-read），随后上传 `deploy/assets` 默认静态资源；bucket 信息不可读（`GetBucketInfo` ServerError，最小权限 RAM key 常被拒 bucket 级操作）仅警告跳过预检、直接逐文件上传（真正失败由上传暴露），不中断部署。
- `--miniapp-only` 跳过远端预检/清理/远程部署/`.env` 渲染，仅上传小程序；但本地门禁（golangci-lint/tsc，可用 `--skip-checks` 跳过）照跑，且上传成功后**仍自动合入 main 并推送**（可用 `--skip-merge` 跳过合入）；合入方式为**直接推送构建树的合并提交**（即生产镜像 tag 所指 commit，使「生产 ⊆ main」严格成立；部署期间 main 被推进时退回临时树重新 merge）。**合入护栏**：miniapp-only 未经验证后端部署面，自动合入前经一次 SSH 探测取生产运行镜像 commit 为基点做 merge-base diff——待合入提交相对生产镜像在 `backend/`、`deploy/`、`mcp-worker/`、`api-worker/` 四路径任一有差异（未验证后端变更搭车进 main），或生产 tag 不可得/无 merge-base 时**拒绝自动合入**（出路：全量部署，或 `--skip-merge` 后人工核验合入）。**全量发布按兼容性顺序**：先部署向后兼容的后端并通过健康门禁，再自动部署 Cloudflare Worker，最后串行上传小程序；Worker/小程序失败不回退后端，系统停在「新后端 + 旧入口/旧前端」（后端兼容 N-1、服务可用），修复后重跑或用 `--miniapp-only` 重试小程序。
- **部分失败恢复纪律**：部分失败后生产**领先/分叉** main（存在未合入的后端变更）。deploy.sh 启动时预检用 git 祖先关系判定「生产运行镜像是否为 origin/main 祖先」——不是（生产含 main 没有的提交）即拒绝并给出恢复路径，确认知情可 `ALLOW_PROD_AHEAD=1` 放行；main 领先生产（文档/脚本等未部署提交）不拦截。恢复完成前禁止部署其他分支（约束所有部署操作者，规则见 AGENTS.md 部署章节）。
- SSL 三态（交互配置 `CFG_SSL_MODE`）：`1`=使用自带证书（强制提供 `CFG_DOMAIN_CERT` 与 `CFG_DOMAIN_CERT_KEY` 两条路径）、`2`=certbot 签发（强制 `CFG_SSL_EMAIL`）、其他值=不配置 TLS（仅 80 端口）。
- **官网静态站随部署发布**（`website/`，Astro；域名 `CFG_WEB_DOMAIN`，当前 `papafeiji.cn`）：控制机 `npm ci && npm run build`（**构建失败不阻断后端部署**：响亮警告后跳过官网同步，服务器保留上一版 `./web/` 继续服务，官网为非核心静态制品）→ rsync `out/` 到服务器 `./web/`（`--delete` 同步，`--exclude=/downloads`——保护服务器手工上传的 APK 分发文件不被清理）→ nginx 只读挂载。官网证书独立 lineage（`papafeiji.cn`+`www`，webroot 验证），`remote_deploy.sh` 在证书缺失时尝试签发、失败仅警告；部署末尾以本机 `Host:` 头拨测（`200/301` 为通过，异常仅警告不回滚）。官网内容更新随任意后端部署生效，无需单独发布流程。
- `AGENTS.md` 或 `docs/` 的变更不应触发部署；实际部署用于代码/配置变更。
- `deploy/legacy-migrations/` 为 squash 前旧迁移的归档副本，不参与部署、无脚本引用，仅供旧库回滚排查参考。

## 5. 备份与恢复

- 部署前：`deploy.sh` 每次部署前对生产库做 `pg_dump -Fc`，保存到控制机 `/root/DeployOps/papafeiji-db-backups/`（按 mtime 保留 7 天；备份产出 `pg_restore -l` 校验失败则中止部署）。**备份门禁**：仅全新环境（无 `postgres_data` 卷）允许 postgres 未运行时跳过；已有数据卷的存量库无法取得可验证备份时中止部署。
- 连续备份：`backup` 容器每 1 小时 `pg_dump` + `uploads` 打包到部署目录 `backups/`，保留最近 48 份（心跳与失败告警见 §1.2 backup 行）。**SaaS 形态下 uploads 打包为空目录打包**（图片在 OSS，本地 uploads 为空卷）：tar 空卷退出码 0、报 `backup uploads ok`——属 no-op 而非错误；`alert=backup_uploads_failed` 仅 tar 本身失败（如挂载缺失）时触发，无误报。**RPO 口径分两层**：小时备份与主库同宿主机，防的是数据损坏/误删（同机 RPO ≤ 1 小时）；**整机灭失**的恢复点 = 控制机离机副本（部署前 dump + `scripts/pull-backup.sh` 每日拉取的最新小时备份，≤ 24h）。
- **灾备覆盖面边界**：整机灭失的可恢复对象**仅数据库**（≤24h）；开源/本地形态的 uploads 打包只存同机 `backups/`（不离机）；SaaS 图片在 OSS——bucket 已按 ADR-0018 由 deploy.sh 幂等开启版本化，误删/误覆盖可从非当前版本恢复；同机凭证泄露式故意破坏（逐版本删除）仍不可抵御，依赖 RAM 风控；Redis（会话/缓存/观测键）无备份——灭失重建后全员登出、`job:*` 观测基线按「键缺失=宽限不告警」语义自然重建（无 job_stale 误报）。**RTO 未定**：冷部署 + restore-backup + Worker/DNS 装配链路齐备但未做过整机重建演练，恢复时长不可预期——接受现状（单实例基调），所有者可随时要求演练。
- **控制机单点**：`/root/DeployOps`（密钥唯一副本、部署锁、离机备份、拨测 cron）灭失不影响生产运行，但**阻断部署、灾备恢复与外部拨测**；无备用控制机/密钥托管约定，密钥备份策略归所有者自理。
- schema 级恢复：`scripts/restore-backup.sh <备份文件> --yes`（支持 `.dump` / `.sql.gz` / `.sql`；恢复前自动安全备份并停启应用容器）。**恢复全程持有与 deploy/watchdog 共享的 `/var/run/papafeiji-watchdog.lock`（`flock -w 60`）**——watchdog 非阻塞取锁失败即让行，恢复进行期间看门狗不会强制拉起容器（取代旧流程「人工临时停用 watchdog cron」）。恢复失败时保留 restore-safety 备份并**保持 app/sse 停止**、锁随脚本退出释放，其后的人工排查窗口仍需自行注意 watchdog 会把「未运行」按异常拉起（首轮探测即拉起、之后 1/2/4/8min 退避）——长时人工排查前应临时停用 watchdog cron，避免对半恢复的库写入。
- 迁移回滚（部署失败自动）：`_rollback_and_exit` 先回滚迁移到部署前版本（`migrate goto <prev>`；部署前无迁移则逐条 `migrate down 1`。正向迁移同样在 postgres 容器内对 `/tmp/migrations` 执行），再恢复 `.env`/nginx/override/migrations/compose 的 `.prev` 快照并重建旧容器，脚本以非零码退出（不重试健康检查，异常容器由 watchdog 兜底）。仅保当次部署；更早版本需 `git checkout <旧 commit>` 重新部署。
- 数据级回滚（人工）：统一走 `restore-backup.sh` + 部署前 `pg_dump` 备份；结构变更遵循 expand → 兼容旧代码 → contract（04 §8 第 6 条、ARCHITECTURE §5.3）。

## 6. 自愈与调优

- `deploy/watchdog.sh`：cron 每分钟检查 `postgres`/`redis`/`app`/`sse`/`nginx` 5 个服务（不含 `backup`/`certbot`），异常强制恢复；`flock` 与 `deploy.sh` 共享，部署期间自动让行。退避/熔断：按 1/2/4/8min 退避，连续失败达 5 次时输出一次 `watchdog_circuit_open`，之后停止自动重启、转 15min 慢探测，恢复后清零并记 `watchdog_recovered`。
- watchdog 附带四项资源检查（ADR-0015，只输出告警关键字不自愈）：`/`、`/var/lib/docker`、`/opt/papafeiji` 三个路径磁盘使用率 ≥85% 且连续 3 次探测 → `alert=disk_high`（磁盘检查无豁免条件；写满将致数据库/AOF/备份全部失效）；Redis `used_memory ≥ 90% maxmemory` → `alert=redis_mem_high`；`backups/.last_success` 心跳超 2× 备份间隔（或缺失）→ `alert=backup_stale`；五个核心容器（app/sse/postgres/redis/nginx，backup/certbot 不在检查列表）任一 CPU 超其限额 90% 且连续 3 次探测 → `alert=papafeiji_cpu_high`（回落输出 `alert=papafeiji_cpu_recovered`；限额从容器 `NanoCpus` 动态读取，**无限额（NanoCpus=0）自动跳过**——当前生产栈不设资源限额，该检查默认休眠；容器只读根文件系统 + `cap_drop ALL` + fail2ban 仍为防持久化的实际屏障）。
- `deploy/optimize.sh` / `deploy/optimize-system.sh`：容器 / 宿主机调优脚本，经 SSH 远端执行（`REMOTE_PASS` 密码经 sshpass，或留空走 SSH 密钥认证，与 deploy.sh 同款），默认只打印方案，`--apply`（别名 `-y`）才落地。`optimize.sh` 生成的 override 与 §1.2 生产基线同口径——**不设 CPU/内存限额、redis noeviction + AOF everysec、保留 DB_BG_***——按硬件规格覆写：postgres 调优参数（含 `shared_preload_libraries=pg_stat_statements`，`max_connections` 随内存档同步调大）、redis 内存上限、app/sse 请求池 `DB_MAX_CONNS`（= max_connections/4，50~100）与 `DB_MIN_CONNS=5`、postgres 容器 shm_size——连接预算硬门槛（§5，2×(请求池+后台池) ≤ max_connections−30）在 optimize 后**不会自动重算**：门槛值固定 170（按基线 `max_connections=200`），按新基线收紧需手工以 `DB_BUDGET_HARD_LIMIT` 传入 `scripts/check-conn-budget.sh`；下一次常规部署会重新分发基线 override 并按 170 校验。

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
| `scripts/spec-check.sh` | spec 门禁（语义见 AGENTS.md §五与 docs/spec-standards.md SR-11） |
| `scripts/check_mcp_static_sync.py` | MCP 静态方法双源对账（`mcp-worker/src/lib.ts` ↔ `backend/internal/mcp/server.go` 的 tools/prompts 定义；spec-check 提示级 SR-11） |
| `scripts/accesslog-p95.sh` | 按路由统计访问日志 P50/P95/P99；P95 超 `SLO_P95_MS`（默认 500）输出 `slo_breach` 并以退出码 2 结束 |
| `scripts/alert-watch.sh` | 扫描日志关键字 → `ALERT_WEBHOOK_URL`（最小告警通道，见 §10） |
| `scripts/check-conn-budget.sh` | 校验 `2×(DB_MAX_CONNS+DB_BG_MAX_CONNS) ≤ 170`（`deploy.sh` 调用） |
| `scripts/alert-cron.sh` | 服务器侧告警扫描入口（cron 每 2 分钟，deploy.sh 自动装配）：汇总 app/sse/backup/certbot 容器日志与 watchdog/cert-check/p95 主机日志喂 `alert-watch.sh`；主机日志走 **offset 增量读取**（首次只看未来、截断自动全量重读），条件恢复后旧行不会重复推送 |
| `scripts/alert-p95.sh` | P95 观测定时入口（cron 每 15 分钟，deploy.sh 自动装配）：聚合 **app 与 sse 双容器**最近 15 分钟访问日志（`/ai/chat` 走 sse 容器）跑 `accesslog-p95.sh`，超阈值推 `slo_breach` webhook |
| `deploy/cert-check.sh` | 证书守卫（cron 每小时，deploy.sh 自动装配——**仅 `CFG_SSL_MODE=1/2` 装配**，纯 HTTP（`=0`）无证书数据源不装配；`CERT_DOMAIN` 由 cron 行注入 API 域名）：证书来源三模式自适应——certbot 卷模式（`CFG_SSL_MODE=2`，读 `papafeiji_certbot_data` 卷内对应 domain lineage）/ 上传证书模式（`CFG_SSL_MODE=1`，读 nginx 容器 `/etc/nginx/ssl/cert.pem`）/ 纯 HTTP（不装配；手工运行且未注入 `CERT_DOMAIN` 时静默退出）；有域名注入却无证书文件输出 `cert_expiry_soon reason=cert_file_missing`；剩余 <21 天输出 `cert_expiry_soon`；证书文件变化时优雅 reload nginx |
| `scripts/uptime-check.sh` | 外部拨测（**控制机**侧，需手工装 cron `*/5 * * * *`）：从控制机探测四个公网面——API `/health/ready`（`CFG_DOMAIN`）、官网首页 `https://${CFG_WEB_DOMAIN}/`（配置 `CFG_WEB_DOMAIN` 时启用）、MCP 公网入口 `/health`（默认 `mcp.pathmemos.com`，`CFG_MCP_PUBLIC_URL` 可覆盖；curl 超时单独放宽至 20s，须大于 Worker /health 内部回源超时 15s——大陆白天高峰 CF→源站国际链路拥塞、回源 5s+ 属常态）与 **api-worker 公网入口**（默认 `api.pathmemos.com`，`CFG_API_WORKER_URL` 可覆盖；该 Worker 无 GET-200 端点，取宽松语义：任意 HTTP 响应即存活，仅连接层失败计为 down），各面独立状态文件，各自连续 2 个周期失败推 `alert=uptime_down`，恢复推 `alert=uptime_recovered`。每个探测周期内失败即触发一次即时重试（间隔 5s，两次都失败才计本周期失败）——吸收大陆直连 Cloudflare 边缘的瞬时抖动（CF 免费版无国内 PoP，高峰时段偶发超时属常态）。探测固定拼 `https://`，**仅适用于启用 HTTPS 的部署**（`CFG_SSL_MODE=1/2`；纯 HTTP 部署会持续误报 down，当前生产 mode 2 不受影响） |
| `scripts/sql-top.sh` | 慢 SQL 榜单（服务器侧执行）：`pg_stat_statements` 按总耗时 Top N |
| `scripts/ai-cost.sh` | AI 成本观测（服务器侧执行）：按 `msg="ai chat usage"` 日志汇总最近 N 小时 token 用量与按用户 Top |
| `scripts/rehearse-migration.sh` | 迁移预演（控制机侧，部署前可选执行）：最新部署备份恢复进一次性 postgres:15 容器，跑全部正向迁移验证可执行 |
| `scripts/release-apk.sh` | APK 完整发布一条命令（控制机）：全量部署该分支（含体验版上传，健康检查后自动合入 main）→ miniprogram-ci 云构建 APK（`build-apk-ci.js`）→ keytool 验签（指纹不符即中止）→ 上传官网 `web/downloads/`（同版本已存在拒绝重复发布；上传成功自动清理旧包，各仅保留最新一个）。版本号/versionCode 取自 `project.miniapp.json`，发布前必须先递增并随分支提交 |
| `scripts/build-apk-ci.js` | Donut APK 云构建（`miniprogram-ci@beta` buildApk，无需微信开发者工具）：暂存工程 → tsc 预编译 TS→ES2018 → 剥离 .ts → 云构建 + 本地 keystore 签名；详见 AGENTS.md「多端应用 APK 构建与分发」 |
| `scripts/pull-backup.sh` | 生产备份离机拉取（控制机 cron 每日 04:15）：scp 服务器最新 `db_*.sql.gz` 到 `/root/DeployOps/papafeiji-db-backups/daily-pull/`（保留 7 份），服务器最新备份超 25h 未更新时退出码 2 告警（`backup_stale_remote`，写入 cron 日志） |
| `scripts/grant_vip_by_phone.sh` | 按手机号补发 VIP 天数（客服补偿）：docker psql 直连，语义同后端 `ExtendVIPDaysWithTx`，记录 `logs/grant-vip.log`。**权益回收（误补发/退款后减天数）无脚本**：处置=手工 SQL `UPDATE user_vips SET expire_time=... WHERE user_id=...`（ExtendVIPDaysWithTx 仅支持正数，方向性单向膨胀为已接受现状） |
| `deploy/miniapp-uploader/` | 部署期小程序构建/上传工具链（`miniprogram-ci`，仅 `upload.js` 入库），由 `deploy.sh` 调用；依赖装在控制机 `/root/DeployOps/mp-uploader-deps/`（首跑自动安装，`MP_UPLOADER_DEPS` 可覆盖），**仓库不含清单 → 不被 Dependabot 扫描**；不进入小程序运行时、不随用户交付 |
| `.github/workflows/ci.yml` | CI：**backend**（golangci-lint + `go test -race` + `check-sqlc-sync` + `check_mcp_static_sync.py` + 连接预算脚本测试 + `spec-check.sh`）、**frontend**（`npm run lint`）、**worker**（api-worker / mcp-worker typecheck） |

> 工具链依赖（含 20 项安全 `overrides`：`sharp`/`minimatch`/`phin`/`protobufjs`/`lodash`/`form-data`/`tough-cookie`/`fast-xml-parser`/`@xmldom/xmldom`/`qs`/`svgo`/`adm-zip`/`tmp`/`js-yaml`/`colord`/`@babel-*`，全集见 `deploy.sh` 内嵌的首次安装模板）装在控制机 `/root/DeployOps/mp-uploader-deps/`，**仓库不含清单、GitHub 不扫描**（第三方依赖统一放仓库外）。已接受风险：`image-size` 两条 DoS 告警（ICNS/JXL/HEIF 死循环）——补丁在 2.x，但导出形态与 miniprogram-ci 的 v1 调用（`require` 直接返回函数）不兼容，且输入全部为自有可信 PNG 资源、无不可信图片解析路径，按可容忍风险接受，待上游升级自然解决；`request`/`lodash.template`/`html-minifier`/`uuid`/`file-type` 同族按可容忍风险接受。

## 9. 内置微信密钥（开源版）

- `backend/internal/wechatsecrets` 将开源版默认的微信小程序 AppID/Secret 以 AES-GCM 加密后内置在代码中（密钥由固定盐经 SHA-256 派生），使开源版用户无需手动填写即可运行（取舍见 ADR-0009）。
- 该机制是**轻量混淆**（避免明文直接出现在源码），不提供高强度保密；有心的逆向分析仍可还原。
- `scripts/encrypt-wechat-secret.go` 用于生成替换密文；未初始化时 `DefaultAppID()` / `DefaultSecret()` 返回空字符串。

## 10. 观测与告警口径（最小集）

> 定位：**观测口径，非验收项**（01 §4 性能指标同此定位）。指标采集仍无时序基建；告警通道已随部署自动装配（ADR-0015）：`alert-cron.sh`（每 2 分钟）+ `alert-p95.sh`（每 15 分钟）+ watchdog/cert-check 关键字，经 `ALERT_WEBHOOK_URL`（`CFG_ALERT_WEBHOOK_URL`，强烈建议必配，未配置时 deploy.sh 警告且告警仅落日志）推送；外部拨测 `uptime-check.sh`（API / 官网 / MCP / api-worker 四个探测面，独立状态文件）在控制机手工装 cron。

- 健康探针：Docker HEALTHCHECK 与 watchdog 使用 `/health/ready`（`deploy.sh` 生成 compose 与 `deploy/Dockerfile` 一致）：DB 故障 → 503 → 容器 unhealthy → watchdog 重建；Redis 故障 → 200 + `status: degraded`（不重启，靠日志告警兜底）。`/health/live`（仅进程存活）与别名 `/health`（等价 ready）仍保留供人工/其他探针使用。
- watchdog：cron 每分钟检查 `postgres`/`redis`/`app`/`sse`/`nginx` 5 个服务，异常强制恢复（见 §6）。
- 任务观测：Runner 在 Redis 记录每任务 `job:last_success` / `job:last_failure` / `job:fail_streak`，每分钟检查 `now - last_success > 3× 周期` 时输出 `job_stale`（由告警通道捕获）；人工补跑 `papafeiji-admin job run <name>`（写触发键，≤1 分钟内由 app/sse 两容器 Runner GETDEL 竞争消费、advisory lock + 任务幂等保证单执行），状态查询 `papafeiji-admin jobs status`。
- 访问日志：`AccessLogMiddleware` 输出结构化 JSON（`msg="api access"`，字段 `route`/`status`/`duration_ms`/`request_id`），公开路由（登录、回调、`/system/config`）与鉴权路由均记录；`scripts/accesslog-p95.sh` 按固定窗口统计分位，核心接口 P95 > `SLO_P95_MS` 时输出 `slo_breach`。
- 告警通道（最小实现）：`scripts/alert-watch.sh` 扫描日志中的资金/数据完整性/可用性关键字，命中且超过去重窗口时 POST 到 `ALERT_WEBHOOK_URL`（同类关键字窗口去重，默认 300s；webhook 发送失败不落去重状态，下一扫描周期自动重试）。扫描入口 `scripts/alert-cron.sh` 由 deploy.sh 装配为每 2 分钟 cron（覆盖 app/sse/backup/certbot 容器日志与 watchdog/cert-check/p95 主机日志）；未配置 webhook 时只打印不发送。
- 告警日志关键字：**全集权威源为 `scripts/alert-watch.sh` 的 KEYWORDS 清单**（新增关键字先入脚本后补表，勿反向）；下表是排障视图的代表性子集与语义注释（级别为代码实际值，Warn/Error 混合），不在表内≠无该关键字：

| 关键字 | 级别 | 含义 | 位置 |
|--------|------|------|------|
| `"alert":"redis_down"`（该行实际匹配形态——main.go 经 slog JSON 渲染键值对，非 `alert=` 字面；alert-watch 按此形态匹配） | Error | Redis 健康检查失败（5 分钟限频去重） | `cmd/server/main.go` |
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
| `payment_notify_read_failed` | Error | 回调 body 读失败（网络截断）——500 触发微信补发（VP-9-AC2b），连续出现需查 nginx/网络 | `payment/handler.go` |
| `payment_notify_body_too_large` | Warn | 回调 body 超 64KB（垃圾/攻击输入，固定成功终止重试） | `payment/handler.go` |
| `alert:payment_parse_failed` | Error | 回调 body 解析失败 | `payment/handler.go` |
| `alert:ai_quota_refund_failed` | Error | AI 配额退款失败（共 4 次尝试后放弃） | `ai/service.go` |
| `[ALERT] wx mp all kf segments failed` | Error | 公众号客服消息分段全部发送失败 | `wxmp/handler.go` |
| `alert=ai_upstream_error` | Error | AI 上游连接失败/非 200/流中断（5 分钟限频去重） | `ai/service.go` |
| `alert=auto_record_failed` | Error | 自动记录轮次存在用户级失败（轨迹保留待下轮重试） | `autorecord/service.go` |
| `alert=invite_reward_failed` | Error | 家庭链接加入奖励（被邀请人 +3 / 邀请人 +7 天 VIP）发放失败（`grantJoinReward` 预检/事务任一失败分支） | `family/handler.go` `grantJoinReward` |
| `auto_record_backlog_warn` | Warn | 自动记录候选用户满批（100）后总数 >1000（公平轮转上界告警） | `autorecord/service.go` |
| `record deleted url for purge failed` | Warn | OSS 删除成功但 purge 队列满溢弃新（CDN_REFRESH 形态；SaaS 直连无 CDN 不触发） | `cmd/server/main.go` |
| `alert=backup_failed` | Error | 备份 pg_dump 失败 | backup 容器 entrypoint、`scripts/backup.sh`（控制机备份） |
| `alert=backup_uploads_failed` | Error | 备份 uploads 打包失败 | backup 容器 entrypoint、`scripts/backup.sh`（uploads 分支失败同置非零退出码） |
| `alert=backup_stale` | Error | 备份心跳缺失或超 2× 间隔 | `watchdog.sh` |
| `alert=redis_mem_high` | Error | Redis 内存 ≥90% maxmemory（noeviction 下写满将显式报错） | `watchdog.sh` |
| `alert=disk_high` | Error | `/`、docker root 或部署目录磁盘 ≥85% 连续 3 次 | `watchdog.sh` |
| `alert=papafeiji_cpu_high` / `alert=papafeiji_cpu_recovered` | Error/Info | 核心容器（app/sse/postgres/redis/nginx）CPU 超其限额 90% 且连续 3 次探测 / 回落恢复；**当前生产栈无限额，检查自动跳过（休眠）** | `watchdog.sh` |
| `watchdog_circuit_open` | Error | 容器连续失败达 5 次熔断：停止自动重启、转 15min 慢探测，等待人工介入 | `watchdog.sh` |
| `watchdog_recovered` | Info | 熔断/退避后容器**单次健康探测**（running 且非 unhealthy）即清零并输出（通知对称：值班知熔断也知恢复） | `watchdog.sh` |
| `job_stale` | Error | 后台任务 `job:last_success` 超 3× 周期未更新 | `jobs/runner.go` |
| `slo_breach` | Error | 路由 P95 超 `SLO_P95_MS`（默认 500ms） | `accesslog-p95.sh` 产出、`alert-p95.sh` 聚合转发 |
| `cert_expiry_soon` | Warn | TLS 证书剩余 <21 天 | `cert-check.sh` |
| `alert=uptime_down` / `alert=uptime_recovered` | Error/Info | 控制机外部拨测失败/恢复 | `uptime-check.sh` |
| `alert=backup_stale_remote` | Error | 服务器最新备份超 25h 未更新（控制机侧，退出码 2 写入 cron 日志，不经 alert-watch） | `pull-backup.sh` |

> 支付链路关键字（`payment_notify_*`）涉及资金，优先级最高。
> `geocode_discarded`（逆地理 10 次失败终态，每轨迹一次）为**仅日志检索**关键字——高频噪音不接 webhook；观测口径 `SELECT count(*) FROM auto_record_trajectories WHERE geocode_attempts >= 10`。
> 破坏性演练（Redis 故障注入等）仍不在常规清单；迁移预演与部署失败自动回滚见 §8/§5。

## 11. 外部平台配置对照（换机/轮换索引）

> 本节为**对照索引**：外部平台 ↔ 用途 ↔ 涉及变量/凭据 ↔ 配置位置。登记值与操作细节以「配置位置」列所指文档为准，此处不复述；控制机 `CFG_*` → 容器变量的渲染关系见 §2.11。

| 外部平台 | 用途 | 涉及变量/凭据 | 配置位置 |
|----------|------|---------------|----------|
| 微信公众平台（小程序后台） | 小程序登录凭证；虚拟支付回调配置；订阅消息模板（异常告警） | `WECHAT_APPID` / `WECHAT_SECRET`；`WECHAT_VIRTUAL_CALLBACK_TOKEN` / `WECHAT_VIRTUAL_CALLBACK_AES_KEY`（43 位，与后台「虚拟支付回调」配置一致）；`ABNORMAL_TEMPLATE_ID`（前端常量，`frontend/miniapp/miniprogram/config/index.ts`） | 控制机 `CFG_WECHAT_*`（§2.11）；回调排障与补发见 AGENTS.md「虚拟支付回调排障与补发」；模板值登记见 06 配置常量表 |
| 微信开放平台（移动应用） | App 微信登录/分享的包名与签名校验基准 | AppID `wx8cbf9a49aa861e50`、包名 `com.pathmemos.app`、签名 MD5 `0d44d1ce96b9595a2792025b0eeb241f` | 登记表见 AGENTS.md「开放平台移动应用登记信息」；签名由 `~/.android/papafeiji.keystore` 决定（App 端通知已裁撤，开放平台的订阅模板已不使用） |
| 多端应用（Donut）控制台 | App 内微信登录 `donut/code2verifyinfo` 换取用户标识（unionid 打通小程序账号） | `DONUT_APPID` / `DONUT_APPSECRET`（应用详情页获取——**非小程序、非移动应用凭据**） | 控制机 `CFG_DONUT_APPID`/`CFG_DONUT_APPSECRET`（§2.11）；未配置时 `/auth/login/app` 500 |
| 腾讯位置服务 | 后端逆地理编码（仅 geocoder）；前端地图/选点 | 后端 `TENCENT_MAP_KEY`（多 key 逗号分隔轮换池）；前端 `qmapAPIKey`（需同时勾选 WebServiceAPI 与 SDK 两类产品） | 后端经控制机 `CFG_MAP_KEY`（§2.11）；前端在 `frontend/miniapp/project.miniapp.json`；explore 配额注意事项见 AGENTS.md「qmapAPIKey 配额」 |
| Cloudflare | DNS 装配（A 记录指向生产）+ api/mcp Worker 自动部署 | `CLOUDFLARE_API_TOKEN`；Workers 域名 `api.pathmemos.com` / `mcp.pathmemos.com` | 控制机 env 导出（§2.11，历史键 `CFG_CLOUDFLARE_API_TOKEN` 兼容）；装配流程见 §4，Worker 运维见 `docs/mcp-worker-ops.md` |
| 阿里云 ECS | 生产应用服务器（cn-hangzhou）；SSH 不可用时经云助手应急执行 | 实例 `i-bp109m23dsgcnv98cr4p`；云助手 AccessKey | 应急命令与凭据配置见 AGENTS.md §四「阿里云 CLI 应急运维」 |
| 阿里云 OSS | 对象存储（图片/静态资源；`CDN_REFRESH_ENABLED` 形态下另需 RAM key 具 CDN 刷新权限） | `OSS_ACCESS_KEY_ID` / `OSS_ACCESS_KEY_SECRET` / `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_PUBLIC_URL` | §2.6/§2.11（部署侧五项成组必配）；`OSS_PUBLIC_URL` 空值回退行为见 §2.11 |
