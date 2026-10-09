# PP-05 开发与协作约定（L5）

> 层级：L5 开发与协作约定｜版本：当前（以代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0003（分支隔离与部署互斥）、ADR-0004（app/sse 双容器）

---

## 1. 开发约定

| 项 | 约定 |
|---|---|
| 代码源头 | `origin`（私有仓库，曾用名 papafeiji-go）的 `main` 为唯一开发源头；`open`（公开 pathmemos）仅经 `scripts/release-open.sh` 发布 |
| 分支纪律 | 功能开发在独立分支（`git worktree` 隔离可选，机制见 ADR-0003）；完成本地门禁后经 `deploy/deploy.sh --branch <分支>` 部署，健康门禁通过自动合入 `main` |
| 评审节点 | 代码评审（合并前）、部署验证（每次部署后健康检查） |
| 红线 | 只处理与当前任务直接相关的文件；禁止直接 push `origin main`；禁止绕过 deploy.sh 直接改服务器 `/opt/papafeiji` 源码 |

## 2. 外部依赖

运行时外部依赖（PostgreSQL / Redis / 微信各端 / 腾讯地图 / AI 上游 / 阿里云 OSS / Cloudflare Worker）的清单、配置变量与运行参数以 [`DEPLOYMENT.md`](../DEPLOYMENT.md)（§1 容器与镜像、§2 环境变量字典）为唯一权威源，本分册不复述（防双写漂移）；依赖版本钉扎与升级策略见 ADR-0014。

## 3. 部署契约

`deploy.sh` 的部署行为契约——执行顺序、健康门禁、失败回滚与半成功终态、自动合入 main、密钥注入边界、备份与恢复（RPO 两层口径）、cron 装配、Worker 部署链——以 [`DEPLOYMENT.md`](../DEPLOYMENT.md)（§4 部署与自动合入、§5 备份与恢复、§8 构建/校验工具）为唯一权威源，本分册不复述（防双写漂移）。

## 4. 本地门禁与工具

| 命令 | 作用 |
|------|------|
| `make lint-go` | golangci-lint（全新缓存目录） |
| `make lint-frontend` | 小程序 `tsc` + `eslint` |
| `make lint-worker` | api-worker / mcp-worker typecheck |
| `make check-sqlc-sync` | sqlc 生成代码与 SQL 列数一致 |
| `make test` | `go test -race ./backend/... -count=1 -timeout 300s` |
| `bash scripts/tests/test-check-conn-budget.sh` | 数据库连接预算脚本测试 |
| `scripts/spec-check.sh` | 文档一致性门禁 |

> CI（`.github/workflows/ci.yml`）执行上述全部门禁：backend（lint/test/sqlc/MCP 对账/连接预算/spec-check）+ frontend lint + worker typecheck。
