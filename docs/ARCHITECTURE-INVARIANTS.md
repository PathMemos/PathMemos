# 架构不变量（Architecture Invariants）

> 版本：当前（以代码为唯一事实源）。
> 本文件记录当前架构必须遵守的**不变量**：PostgreSQL 为唯一事实来源、Redis 为辅助，配置全部来自环境变量。

## 1. 总体架构

```
微信小程序 / 公众号 / MCP 客户端
        │
      Nginx
        │
   Go API(:8080) + SSE(:8081)
   Auth / User / Family / Diary / File / Location / AI / VIP / Payment / MCP
        │
   ┌────┴─────┐
PostgreSQL   Redis（辅助）
唯一事实来源  会话 / 缓存 / 限流 / 去重节流标记 / 临时状态
        │
   Go ticker 后台任务（自动记录 / 关单 / 日志清理 / 轨迹清理 / 孤儿文件清理）
```

## 2. 架构不变量（不可违背）

> 仲裁基线：**核心业务正确性 > 性能优化**——缓存、锁、限流、去重标记等加速手段不得凌驾于 DB 正确性（本表 I1/I2 的总纲，适用于一切实现取舍）。

| # | 不变量 | 含义 |
|---|--------|------|
| I1 | **PostgreSQL 是唯一事实来源** | 业务正确性的最终依据只能是 DB（事务 / 唯一约束 / 状态机）；Redis 与进程内缓存不得成为最终状态 |
| I2 | **Redis 故障不导致数据错误** | Redis 仅保存 Session、缓存、限流计数、临时状态等非业务事实；不得造成数据丢失、重复支付。「重复业务写入」限定**资金（支付 / VIP 发放）与家庭结构写入**；AI 对话日志与配额计数在 Redis 故障窗口**允许重复**（损失上限 = 单用户单日配额量级，DB `IncrementAIDailyQuotaUsed` 的 `WHERE used<quota` 原子条件扣减是权威闸门限损，见 02f AI-2/AI-4）。**已知失败模式**：① Session 校验 fail-closed，Redis 故障 = 全部受保护接口 500（等同鉴权不可用），并非仅辅助降级；② AI 幂等 SETNX fail-open——`ai/service.go` `chatWithPrompt` 在 SETNX 报错时按未获取处理、走正常生成（不阻断对话，断线重连同 request_id 可能重复扣配额 / 落 `ai_dialog_logs`，见 02f AI-2）。恢复路径：Redis 恢复即自愈（/health/ready 对 Redis 故障返回 200、watchdog 不重启），无需人工干预 |
| I3 | **身份与归属只由服务端裁决** | 用户身份取自 session（Redis）；客户端传入的 userId / familyId / diaryId / fileId 等仅为参数，不得作为权限依据；资源归属一律按当前身份（context）+ DB 关系校验（规则清单见 `spec/03-api.md` §5） |
| I4 | **家庭数据按当前成员关系校验** | 共享视图与访问校验以 `current_family_id` 的当前成员为准；`diaries` / `diary_entries` 不存 `family_id` |
| I5 | **支付以可信回调 + DB 幂等为最终依据** | 支付成功以微信验签回调为准；`out_trade_no` 唯一 + 订单状态机在 DB 内保证只发一次货；Redis 仅辅助 |
| I6 | **后台任务必须幂等** | 任务重复执行、重启、锁丢失都不得产生重复数据；游标/去重键是优化，不是正确性前提 |
| I7 | **AI 上下文中的日记内容是不可信数据** | 用户日记文本只能作为数据拼入 prompt，不得被当作系统指令执行；可测断言见 02f AI-5「不可信数据」条 |
| I8 | **文件公开访问是产品设计** | 图片使用 UUID 定名 + 公开 URL；知道 URL 即可访问，不引入签名 URL / 鉴权代理 |
| I9 | **注销联动遵循 DB 约束 + 业务事务** | 结构性联动由 `ON DELETE CASCADE/SET NULL` 负责；业务性联动由事务负责；物理文件异步清理 |
| I10 | **错误响应词汇统一** | `code` 固定枚举（0000/4000/4010/4030/4040/4090/4130/4290/5001），语义码一律 `biz_code`，同一 biz_code 全端点同一 HTTP 状态。**例外**：SSE 端点（`/ai/chat`）流建立后 HTTP 恒 200，错误经 `event: error` 事件体下发，biz_code→HTTP 映射仅作用于事件体内的 `code` 字段；开流前的校验/限流/鉴权错误（400/413/429/401）仍走统一 envelope（ADR-0008；词汇表见 `spec/03-api.md` §3） |
| I11 | **锁不是正确性来源** | PG advisory lock / Redis SETNX 只减少并发重复执行；最终正确性由 DB 事务、唯一约束、状态条件更新保证（锁清单见 §8；配合 I1/I6） |

## 3. 模块取舍

**保留**：用户 / 家庭 / 日记 / 文件 / 自动轨迹 / Location / AI SSE / VIP / 虚拟支付 / MCP / PostgreSQL / Redis（辅助）/ Go ticker 后台任务 / Nginx + 本地文件公开访问。

**删除**：`sys_configs`（配置中心化过度设计，配置全部改环境变量）。

**说明**：
- **不存在 Admin HTTP 模块**：仓库只有 `backend/cmd/admin`（一次性运维 CLI：按 `TARGET_PHONE` 删除用户、`jobs status` 查询后台任务观测、`job run <name>` 触发后台任务）。该 CLI 体量极小且被运维使用，**保留**；不涉及任何后台路由/中间件/配置热更新。
- **不存在 JWT**：鉴权为服务端 session（Redis），无 token、无本地撤销缓存。
- **推送（push）是现有功能**（异常告警 / 新地点提醒 / 客服消息），当前在用。

## 4. 关键架构决策

| 决策 | 说明 |
|------|------|
| 不设独立 Admin 后台 | 仅保留 `cmd/admin` 运维 CLI（按手机号删用户、`jobs status`、`job run <name>`），无 HTTP 管理接口 |
| 配置全部走环境变量 | 无中心配置表 |
| Redis 为辅助组件 | 仅会话 / 缓存 / 限流 / 配额预检 / 临时幂等；见 I1、I2 |
| 分布式锁为 PostgreSQL | `db.AdvisoryLock`（会话级 advisory lock） |
| Health 拆分 | `/health/live`（进程）+ `/health/ready`（DB）；Redis 上报但不判死 |
| 推送能力保留 | 异常告警 / 新地点提醒 / 客服消息为在用功能 |
| 生产回滚策略 | expand → 兼容旧代码 → contract；不以 `migrate down` 为常规手段 |
| AI 日志保留 90 天 | 仅按时间清理，无每用户条数上限 |
| VIP 判定一律严格 | `expire_time > now()`；无宽限期概念 |
| `WORKER_SECRET` 生产必填 | `DEPLOYMENT_MODE=saas` 时启动校验（空则拒绝启动）；open 模式必须留空 |
| 客户端日志保留 30 天 | `cleanup_client_ops_logs` 后台任务；后台任务总数 10（含 `purge_deleted_objects`，ADR-0013） |
| `user_vip_claims` 保留 | trial / free 领取防重（`INSERT` + `EXISTS`） |

## 5. 配置来源

配置全部来自环境变量或代码常量：

| 行为配置 | 环境变量来源 |
|---------------------|----------|
| `ai_config.baseUrl` | `AI_BASE_URL` |
| `ai_config.model` | `AI_MODEL` |
| `ai_config.maxOutputTokens` | `AI_MAX_OUTPUT_TOKENS` |
| `ai_config.thinking.type` | `AI_THINKING_TYPE` |
| `ai_prompt` | `AI_PROMPT`（代码内默认值，可环境变量覆盖） |
| `sys_config.wechatMpPrompt` | `WECHAT_MP_PROMPT` |
| `sys_config.defaultCoverImage` / `defaultAvatarUrl` / `defaultTrajectoryIcon` | 环境变量 `DEFAULT_COVER_IMAGE` / `DEFAULT_AVATAR_URL` / `DEFAULT_TRAJECTORY_ICON` |
| 文件对外基址 | open 模式取 `API_HOST`；SaaS 模式经 `CFG_DOMAIN` 渲染为 `STORAGE_PUBLIC_BASE_URL`。`API_HOST` 两种模式均必填（`config.validate` 无条件校验；SaaS 供 MCP 公开入口回退使用） |

> 配置唯一来源为 `config.go` 的环境变量读取；仓库无中心配置表。

## 6. 验收标准

除 `go build ./...` 外，至少通过：`go test ./...`、`go vet ./...`、`make lint-go`、`make lint-frontend`、`make lint-worker`、`make check-sqlc-sync`；前端 Jest（`make test-frontend`）与 Worker vitest 为存量资产、非门禁（spec-standards §五）。

CI（`.github/workflows/ci.yml` 三 job）实际门禁：**backend** = golangci-lint（默认集含 govet，等效 `go vet`）+ `go test -race ./backend/...` + `make check-sqlc-sync` + `scripts/check_mcp_static_sync.py`（MCP 静态方法双源对账）+ `scripts/tests/test-check-conn-budget.sh`（连接预算脚本自测）+ `scripts/spec-check.sh`；**frontend** = `npm ci` + `npm run lint`（tsc + eslint）；**worker** = api-worker / mcp-worker `npm ci` + `npm run typecheck`。

关键业务场景（**人工验收**，见 `docs/spec/07-acceptance-flows.md` 的场景编号）：

| 域 | 必测 | flow |
|----|------|------|
| 用户 | 登录 / 重复登录 / 注销 / 注销后旧 session 失效 | F1 / F9 |
| 家庭 | 创建 / 加入 / 退出 / 移除 / 解散 / owner 迁移 / 用户注销 | F5 |
| 日记 | 创建 / 修改 / 删除 / 跨家庭访问拒绝 / 图片关联 / 日期聚合 | F2 / F4 |
| 文件 | 正常上传 / 超 10MB / 超 50MB / 非法类型 / 删除 / 重复引用 | F11 |
| AI | 正常 SSE / 客户端断开 / 上游超时 / 上游错误 / 超上下文上限 / SSE 并发超限 429（单用户第 3 连接）/ 在途冲突 OPERATION_IN_PROGRESS / 退款失败告警 `alert:ai_quota_refund_failed` | F7 |
| 支付 | 正常回调 / 重复回调 / 错误签名 / 订单关闭后回调 / 用户注销后回调 | F6 |
| 自动记录 | 重复执行 / 任务重启 / 轨迹点重复 / 逆地理失败 | F3 |
| MCP | Key 生成/查询/轮换；经 Worker 读写日记/回忆；源站内部端点拒绝公网直连 | F8 |
| 私有化接入 | Worker 注册握手、按 API Key 路由、错误码 | F12 |
| 不变量 | Redis 宕机不影响 DB 正确性；服务面为鉴权 fail-closed——受保护接口全 500（I2 口径，非仅限流/缓存降级）；后台任务重复执行不产生重复数据 | 人工破坏性演练年度一次（07 §1.1/§4），结果写入当次发布说明 |

## 7. 架构边界

- 后台任务：Go ticker + 幂等 + PostgreSQL advisory lock 互斥，未使用外部任务队列 / 调度服务。无积压指标；Redis 记录每任务 `job:last_success` / `job:last_failure` / `job:fail_streak`，`watchJobHealth` 每分钟做失联检测（`now - last_success > 3× 周期` 输出 `job_stale`），人工补跑 `papafeiji-admin job run <name>` 写 `job:trigger:<name>` 由 app/sse 双容器的 `watchJobHealth` 竞争消费（`GETDEL` 原子取走 + 任务 advisory lock 去重，单次执行）；另有 slog，慢于 80% `maxDuration` 记 warn。
- 文件：UUID 定名 + 公开 URL；未使用签名 URL / 鉴权代理。
- MCP：一用户一 API Key + 基础工具；未引入多 Key / OAuth / scope。
- 缓存：Redis 单层；未使用进程内二级缓存。
- 限流：进程内内存滑动窗口（`middleware/ratelimit.go`），单实例有效；多实例部署时各实例独立计数。部署面另有 nginx 层限流（API 20/upload 10/web 25 r/s + limit_req 429 / limit_conn 503）与 fail2ban 主机封禁（见 `DEPLOYMENT.md` §3）；本条仅指后端应用层的实现边界。
- 数据库连接：Postgres `max_connections=200`；`app`/`sse` 各创建请求池（`DB_MAX_CONNS`/`DB_MIN_CONNS`，默认 50/10）与后台池（`DB_BG_MAX_CONNS`/`DB_BG_MIN_CONNS`，默认 10/2）两个独立 pgx pool；`deploy/docker-compose.override.yml` 注入 75/5 与 10/2。`scripts/check-conn-budget.sh` 以 `2×(DB_MAX_CONNS+DB_BG_MAX_CONNS) ≤ 170`（当前 170）作为部署硬门槛。
- HTTP 超时：app `ReadTimeout=10min`、`WriteTimeout=310s`、`ReadHeaderTimeout=10s`；SSE `ReadTimeout=WriteTimeout=0`。

## 8. 分布式锁（PostgreSQL advisory lock）

Redis 只保留 **会话 / 缓存 / 限流 / 配额预检 / 临时幂等辅助**；分布式锁统一为 PostgreSQL advisory lock。

实现：`backend/internal/db/advisory_lock.go` 的 `db.AdvisoryLock` 使用 PostgreSQL **会话级 advisory lock**（`pg_try_advisory_lock(hashtextextended(key, 0))`；持锁占用一个连接，进程退出即自动释放）。服务层通过 `db.Locker` 接口注入，单元测试注入内存假实现（如 diary 包 `memLocker`）。

| 锁键 | 位置 | 现方案 |
|------|------|--------|
| `lock:family:{familyID}` | `family/service.go` | `db.AdvisoryLock` + `uq_family_members_user_id` 唯一约束兜底 |
| `lock:delete_account:{userID}` | `family/service.go` | `db.AdvisoryLock` |
| `lock:auto_record:{userID}` | `autorecord/service.go`、`diary/service.go` | `db.AdvisoryLock` + 轨迹唯一索引 / `UpsertDiary` 幂等兜底 |
| `lock:covers:{familyID}:{date}` | `diary/service.go` | `db.AdvisoryLock`（短临界区） |
| `lock:background:{task}`（10 个） | `jobs/runner.go` | `db.AdvisoryLock`；任务自身幂等 |
| `invite:qrcode:gen:{userID}` | `invite/qrcode.go` | `db.AdvisoryLock` |
| 迁移启动互斥（常量 key `0x6d6967726174696f`） | `internal/migration/migrate.go` | 基础设施锁：迁移期 `pg_advisory_lock` 会话级互斥（防双实例并发跑迁移），不属业务锁 |

> 说明：advisory lock 无 TTL（连接断开自动释放），`Locker` 接口不含 TTL/续期语义；正确性仍以 DB 唯一约束/事务/幂等为最终依据（见 I1/I6/I11）。
>
> 另：`lock:covers:refresh:{familyID}:{date}` 为 Redis SETNX 30s **去重节流标记**（非互斥锁，保留在 Redis；故障仅失去节流，符合 I2），封面互斥由 `lock:covers:{familyID}:{date}` advisory lock 承担。

## 9. 已接受风险与「不改」清单
- 续费后自动恢复后台定位采集，无需再确认（VIP 过期停采→续费恢复=既有授权延续；重弹确认打扰大多数未开自动记录的续费用户）
- 日记图片物理删除 best-effort 失败时，对象停留于曾公开且长缓存的 URL（残余暴露面已接受；不加自动重试——失败小概率、内容为已删除日记残留）
- `AI_MAX_OUTPUT_TOKENS` 未配置时单轮 AI 输出成本敞口**有界但不由本服务控制**：上界 ≈ 上游生成速率 × 180s 流超时 × 日配额（10/100 次），量级小；不设代码默认值（保守默认可能截断推理类模型计入 completion 的 thinking 长回答，02f D10「配额即全部」变现边界指调用次数）——SaaS 部署建议必配，开源版随上游模型默认

以下为**有意的产品/架构取舍**，已由 ADR 或 L2 决策锁定；改变这些取舍须新增 ADR 或在对应分册更新决策。

| 项 | 取舍 | 出处 |
|----|------|------|
| 图片公开可访问 | UUID 定名 + 公开 URL，不引入签名 URL / 鉴权代理 | I8 |
| 双套 schema_migrations 记账（open 启动器 TEXT PK 版 vs golang-migrate BIGINT+dirty 版）同名不同构 | 不加防呆预检：触发前提是生产库被显式误配 `DEPLOYMENT_MODE=open`（部署链固定注入 saas 运行值），概率极低且后果是启动失败而非数据损坏，加预检属过度防御 | 04 §8 |
| MCP API Key 明文存储 | 明文供永久展示 + SHA-256 认证；泄露风险已缓解（一键/rotate/注销级联/限流） | ADR-0007 |
| 开源版内置微信 AppID/Secret | AES-GCM 轻量混淆，非保密机制 | ADR-0009 |
| 家庭邀请链接长期有效、无撤销 | 小家庭熟人场景，成本收益不匹配 | ADR-0010 |
| owner 携整家合并不可逆 | 接受钓鱼风险，不做二次确认 | ADR-0012 |
| 支付回调金额不作为漏发闸门 | 金额 >0 即发货；与标价不一致仅告警 | ADR-0011 |
| 已删图片边缘缓存有限收敛窗口（**仅 CDN 部署形态（私有）适用；SaaS 为 OSS 直连无边缘层**） | 定期批量 purge，不做同步清理 | ADR-0013 |
| `users.session_key` 明文存储 | 虚拟支付签名所需 | 02e / 04 |
| 详情列表 offset 分页并发边界 | 单日量小、暂不改 cursor（复查触发：用户报告重复/漏条目，或季度人工抽样 diaries-entries 单日条目数分布） | 02b D-3 |
| 注销后数秒 session 残留 | 依赖 TTL 自然过期 | 02a |
| 注销重注册一律不再发放邀请奖励：openid 墓碑命中即整体跳过——本人 +3 与邀请人 +7 均不发、也不建邀请行，注册/补绑/加入主流程照常成功；预检使唯一冲突不再触发已中止事务的 25P02 连锁 500 | 被邀请人侧 +3 自 000008 起 openid 墓碑终身一次；邀请人侧同步收口为不发 | 02d F-5 / 02a A-6 |
| 邀请人月度奖励计数存于 `user_invites`(inviter_id=自己)，邀请人注销后 `inviter_id` 置 NULL（000011 SET NULL）计数清零、月度上限可跨账号周期重置 | 同上，自抑制更强 | 04 §3.18 / 02a A-6 |
| 注销 × 在途已付款订单：支付成功后、回调到达前用户注销（注销事务将订单 `user_id` 置空），回调到达后无论 pending→paid 还是 closed→paid 补记，均只更新订单状态、不激活 VIP、无退款路径；重注册（同微信新账号）亦不补发（订单与 openid 无关联，墓碑机制不覆盖） | 秒级窗口 × 用户自主注销；订单状态最终收敛，资金侧接受 | 02e VP-10 / ADR-0011 |
| MCP Key 限流分桶键取自未验证 Bearer 原文：随机 Bearer 每请求新桶、绕过 30/min/Key 层；剩余防线=open 模式外层 60/min/IP、SaaS 直连被 `workerSecretAuth` 403、经 Worker 边缘 120/min/IP（代理与静态分支同限）封顶随机 Key 放大；残余影响为每请求一次 DB 查询（受 IP 层限额约束） | 容忍 hacker——无数据可读、无放大；Cloudflare 平台限流为可选加固 | 02h §6.1 |

> 说明：本清单只收录**已经过决策**的取舍。

