# PP-05 开发计划与部署契约（L5）

> 层级：L5 开发计划与部署契约｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0003（多 Agent 隔离）、ADR-0004（app/sse 双容器）

---

## 1. 开发约定

| 项 | 约定 |
|---|---|
| 协作模式 | 多 Agent 并行：`git worktree` 分支隔离（`feat/agent-*`），独立验证后经 `deploy/deploy.sh --branch` 合入 `main`，互不阻塞 |
| 代码源头 | `origin`（私有 papafeiji-go）的 `main` 为唯一开发源头；`open`（公开 pathmemos）仅经 `scripts/release-open.sh` 发布 |
| 迭代节奏 | 每个迭代：Spec 评审（对应层制品对齐）→ 任务拆解（依赖与 `[P]` 并行标记）→ 实现（代码+测试+migration 同步回写 spec）→ 本地门禁 → 部署验证 → 收敛 |
| 评审节点 | Spec 评审（开工前）、代码评审（合并前）、部署验证（每次修改后） |
| 红线 | 只处理与当前任务直接相关的文件；禁止直接 push `origin main`；禁止绕过 worktree 直接改 `/opt/papafeiji` 源码 |

## 2. 外部依赖

| 依赖 | 用途 | 状态 |
|------|------|------|
| PostgreSQL 15 | 主数据库 | 已就绪（docker） |
| Redis 7 | Session / 缓存 / 限流 / 临时幂等（分布式锁已迁 PostgreSQL advisory lock，ADR-0005） | 已就绪（docker，`volatile-lru`） |
| 微信开放平台（`api.weixin.qq.com`） | 小程序登录、虚拟支付、模板消息、服务号消息 | 外部，运行必备 |
| 腾讯地图（`apis.map.qq.com`） | 逆地理编码、静态地图轨迹图 | 外部，运行必备 |
| DeepSeek（`api.deepseek.com`） | AI 对话 | 外部，运行必备（baseUrl/model 可经环境变量覆盖） |
| 阿里云 OSS | 图片与静态资源（本地存储可兜底） | 生产已启用 |
| Cloudflare Worker | MCP 公网入口 `mcp.pathmemos.com`；私有化路由 `api.pathmemos.com` | SaaS MCP 必需 |

## 3. 部署契约（`deploy/deploy.sh` 行为约定）

### 3.1 执行顺序（骨架）

1. 本地：环境变量校验（含可选代码 lint 检查，`--skip-checks` 跳过）。
2. 迁移前全库 `pg_dump` 备份（远端容器执行、拉回控制机 `/root/DeployOps/papafeiji-db-backups/`）。
3. 代码同步到远端（`rsync` 源码，`scp` 生成物）。
4. 快照远端将被覆盖的 `.env/nginx.conf/docker-compose.*/migrations`（供回滚）。
5. 远端：数据层（postgres/redis）未就绪时先启动（仅首次部署）。
6. 迁移：`migrate up`（先校验旧迁移版本 squash 守卫）。
7. 起业务层（app/sse/nginx）→ 健康门禁（healthcheck 必须 healthy）。
8. 安装 watchdog/reload cron → nginx 配置校验（`nginx -t`）+ `docker compose restart nginx`。
9. 全部成功才由脚本把分支合入 `main` 并 push。

### 3.2 失败处理分类

- **自动回滚**（`_rollback_and_exit`）：迁移失败、业务层启动失败、健康门禁未过 → 迁移回滚到部署前版本（`migrate goto <prev>`；部署前无迁移则逐条 `migrate down 1`）+ 恢复上一版镜像与被覆盖的运维配置（`.env`/nginx/override/migrations/compose 快照），以非零码退出（不重试健康检查，异常容器由 watchdog 兜底）。仅保当次部署。
- **数据级回滚**：用部署前 `pg_dump` 备份走 `scripts/restore-backup.sh`（04 §8.6）。
- **仅告警继续**：postgres 未运行导致备份跳过（允许继续）；备份产出校验失败则中止部署。
- **直接退出**：nginx 配置校验失败（先复位配置）。
- 「回退一步」只保当次部署，更早版本需 `git checkout <旧 commit>` 重新构建。

### 3.3 密钥注入边界

| 容器 | 可见密钥 | 理由 |
|------|---------|------|
| `app` / `sse` | 运行期实际读取项（`DATABASE_URL` / `REDIS_PASSWORD` / 微信 / OSS / AI / 地图 / `WORKER_SECRET` / `MCP_WORKER_SECRET`） | 显式白名单，避免整文件灌入 |
| `postgres` | 仅 `POSTGRES_*` | 建库账号 |
| `backup` | 仅 `DB_PASSWORD` | 备份任务连库 |
| `nginx` / `redis` | 无（redis 仅自身密码） | 不持有可伪造 token 的密钥 |

硬约束：`.env` 权限 600；密钥只存控制机 `/root/DeployOps/env/.env.papafeiji`（不入仓库）；部署输出不回显口令；`WORKER_SECRET` 为 SaaS 必填（空则启动校验失败），open 模式必须留空。

### 3.4 备份与恢复

- 备份：每次部署迁移前 `pg_dump -Fc` 到控制机 `/root/DeployOps/papafeiji-db-backups/`；部署机保留 `.prev` 快照。
- 连续备份：SaaS 生成 compose 内含 `backup` 容器（postgres:15-alpine），每 6 小时 `pg_dump` + uploads 打包到部署目录 `./backups`，默认保留 7 份（`BACKUP_KEEP` 可调）。
- 恢复：统一走 `scripts/restore-backup.sh`（支持 .dump/.sql.gz/.sql，恢复前自动安全备份并停启应用容器，需 `--yes`）。
- 迁移回滚仅限当次部署失败窗口：`_rollback_and_exit` 自动 `migrate goto <prev>`（部署前无迁移则逐条 down）；历史版本回退需 `git checkout <旧 commit>` 重新部署。数据级回滚统一走 `restore-backup.sh` + 部署前备份（04 §8.6、DEPLOYMENT §5）。

## 4. 本地门禁与工具

| 命令 | 作用 |
|------|------|
| `make lint-go` | golangci-lint（全新缓存目录） |
| `make lint-frontend` | 小程序 `tsc` + `eslint` |
| `make lint-worker` | api-worker / mcp-worker typecheck |
| `make check-sqlc-sync` | sqlc 生成代码与 SQL 列数一致 |
| `make test` | `go test -race ./backend/... -count=1 -timeout 300s` |
| `bash scripts/audit-patterns.sh` | 静态审计（信息性，恒 exit 0） |
| `scripts/spec-check.sh` | spec 门禁（§九） |

