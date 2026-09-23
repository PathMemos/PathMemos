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
| Redis 7 | Session / 缓存 / 限流 / 临时幂等（分布式锁已迁 PostgreSQL advisory lock，ADR-0005） | 已就绪（docker，`noeviction` + AOF everysec，`--maxmemory 307mb`） |
| 微信开放平台（`api.weixin.qq.com`） | 小程序登录、虚拟支付、模板消息、服务号消息 | 外部，运行必备 |
| 腾讯地图（`apis.map.qq.com`） | 逆地理编码、静态地图轨迹图 | 外部，运行必备 |
| DeepSeek（`api.deepseek.com`） | AI 对话 | 外部，运行必备（baseUrl/model 可经环境变量覆盖） |
| 阿里云 OSS | 图片与静态资源（本地存储可兜底） | 生产已启用 |
| Cloudflare Worker | MCP 公网入口 `mcp.pathmemos.com`；私有化路由 `api.pathmemos.com` | SaaS MCP 必需 |

## 3. 部署契约（`deploy/deploy.sh` 行为约定）

### 3.1 执行顺序（骨架）

1. 本地：环境变量校验（含可选代码 lint 检查，`--skip-checks` 跳过）；配置 `CLOUDFLARE_API_TOKEN` 时经 Cloudflare API 装配 API 域名与官网域名(+www) 的 A 记录（指向 REMOTE_HOST、DNS-only；已有记录指向不一致默认仅警告，`CFG_CF_DNS_OVERWRITE=1` 才改写；zone 不在账号内或接口失败仅警告不阻断）。
2. 迁移前全库 `pg_dump` 备份（远端容器执行、拉回控制机 `/root/DeployOps/papafeiji-db-backups/`）。
3. 代码同步到远端（`rsync` 源码，`scp` 生成物）。
4. 快照远端将被覆盖的 `.env/nginx.conf/docker-compose.*/migrations`（供回滚）。
5. 远端：数据层（postgres/redis）未就绪时先启动（仅首次部署）。
6. 迁移：`migrate up`（先校验旧迁移版本 squash 守卫；检测到残留旧版本时可用 `--auto-align-schema-version` 在自动备份后对齐 `schema_migrations` 到基线，否则提示手工 `UPDATE` 后重跑）。
7. 起业务层（app/sse）→ 安装 4 个 cron（watchdog 每分钟 / alert-cron 每 2 分钟 / alert-p95 每 15 分钟 / cert-check 每小时，带 marker 去重）→ 健康门禁（app/sse healthcheck 必须 healthy）。
8. nginx 分层切换（ADR-0015，零停机）：未运行才启动（首次部署在健康门禁通过后启动，需 nginx 先在 80 端口服务 certbot webroot 验证）→ 配置相对 `.prev` 变化才经一次性容器 `nginx -t` 校验后 force-recreate → 总是 `nginx -s reload`（重解析 upstream DNS + 排空旧连接）。
9. 全量发布按兼容性顺序：后端（向后兼容）通过健康门禁后，先自动部署 Cloudflare Worker（api-worker/mcp-worker：`--var` 把回源地址对齐到本次 `CFG_DOMAIN`、`secret put` 同步共享密钥，`--skip-workers` 跳过），再上传小程序；Worker 或小程序失败均不回退后端。
10. 全部成功才由脚本把分支合入 `main` 并 push。

### 3.2 失败处理分类

- **自动回滚**（`_rollback_and_exit`）：迁移失败、业务层启动失败、健康门禁未过 → 迁移回滚到部署前版本（`migrate goto <prev>`；部署前无迁移则逐条 `migrate down 1`）+ 恢复上一版镜像与被覆盖的运维配置（`.env`/nginx/override/migrations/compose 快照），以非零码退出（不重试健康检查，异常容器由 watchdog 兜底）。仅保当次部署。
- **数据级回滚**：用部署前 `pg_dump` 备份走 `scripts/restore-backup.sh`（04 §8 第 6 条）。
- **备份门禁**：仅全新环境（无既有数据卷）允许 postgres 未运行跳过备份；存量库拿不到可验证备份必须中止；备份产出校验失败一律中止部署。
- **nginx 配置校验 / 重启失败**：调用 `_rollback_and_exit`，与自动回滚同路径（回滚迁移 + 恢复 `.prev` 快照并重建旧容器）。
- 「回退一步」只保当次部署，更早版本需 `git checkout <旧 commit>` 重新构建。
- **Worker/小程序上传失败**：不回退后端（后端向后兼容、服务可用），非零码退出（不合 main、不发小程序）；Worker 部署后拨测 `mcp.pathmemos.com/health`，失败仅警告（边缘传播需数十秒）。

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
- 连续备份：SaaS 生成 compose 内含 `backup` 容器（postgres:15-alpine），每 1 小时（`BACKUP_INTERVAL` 默认 3600）`pg_dump` + uploads 打包到部署目录 `./backups`，默认保留 48 份（`BACKUP_KEEP` 可调；RPO ≤ 1h）；临时文件成功后原子替换，失败记录日志且不退出，成功写 `backups/.last_success` 心跳（watchdog 检查超 2× 间隔输出 `alert=backup_stale`），db/uploads 失败输出 `alert=backup_failed`/`alert=backup_uploads_failed`。
- 恢复：统一走 `scripts/restore-backup.sh`（支持 .dump/.sql.gz/.sql，恢复前自动安全备份并停启应用容器，需 `--yes`）。
- 迁移回滚仅限当次部署失败窗口：`_rollback_and_exit` 自动 `migrate goto <prev>`（部署前无迁移则逐条 down）；历史版本回退需 `git checkout <旧 commit>` 重新部署。数据级回滚统一走 `restore-backup.sh` + 部署前备份（04 §8 第 6 条、DEPLOYMENT §5）。

## 4. 本地门禁与工具

| 命令 | 作用 |
|------|------|
| `make lint-go` | golangci-lint（全新缓存目录） |
| `make lint-frontend` | 小程序 `tsc` + `eslint` |
| `make lint-worker` | api-worker / mcp-worker typecheck |
| `make check-sqlc-sync` | sqlc 生成代码与 SQL 列数一致 |
| `make test` | `go test -race ./backend/... -count=1 -timeout 300s` |
| `bash scripts/tests/test-check-conn-budget.sh` | 数据库连接预算脚本测试 |
| `bash scripts/audit-patterns.sh` | 静态审计（信息性，恒 exit 0） |
| `scripts/spec-check.sh` | spec 门禁（§九） |

> CI（`.github/workflows/ci.yml`）执行上述全部门禁：backend（lint/test/sqlc/MCP 对账/连接预算/spec-check/audit-patterns）+ frontend lint + worker typecheck。

