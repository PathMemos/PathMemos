# 能力索引（L8）

> 全平台能力实时清单（按域分组）。新增/变更能力时同步本条（见 [`spec-standards.md`](spec-standards.md) §一、§九）。
> 行为细节以对应 L2 分册为准；本表只做**能力登记 + 测试映射**。
> 测试/验收口径：自动化仅**后端 Go 单测**；小程序/Worker/端到端为**人工验收**（L7 场景号）。存量前端 Jest / Worker vitest 为资产、非门禁。
> 状态：V2.1（以当前代码为唯一事实源）。

## 认证 / 账号（02a）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 微信登录（新用户自动注册） | `POST /auth/login` | auth/handler_test.go |
| 登出 | `POST /auth/logout` | auth 包单测 |
| 手机号绑定（原子日限 + IP 限流） | `POST /auth/phone/bind` | auth/handler_phone_test.go |
| 手机号解绑（保留 phone_bind_time） | `POST /auth/phone/unbind` | auth/handler_phone_test.go |
| 手机号查询（canModifyToday） | `GET /auth/phone` | auth/handler_phone_test.go |
| 登录后补绑邀请人 | `POST /auth/inviter` | — |
| 邀请奖励（注册期 / 补绑共用） | `applyInviteRewardsWithTx` | — |
| 用户资料 / 语言 | `GET /user/profile`、`PUT /user/nickname`、`PUT /user/lang` | — |
| 账号注销（session/文件清理） | `DELETE /auth/account` | — |
| 会话管理（Redis，滑动+绝对 TTL） | middleware/session.go | middleware/session_test.go |

## 日记（02b）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 时间线卡片列表（游标分页） | `GET /diary/info` | — |
| 按日期批量取卡 | `POST /diary/info/dates` | — |
| 日记详情（offset 分页 + count + memories） | `GET /diary/details` | diary/service_test.go |
| 手动条目增删改 | `POST/PUT/DELETE /diary/details` | — |
| 记忆（随手记）CRUD | `/diary/details/memory` | — |
| 手动封面设置 / 清除 | `PUT /diary/info` | — |
| 整日删除（仅本人当日） | `DELETE /diary/info` | — |
| 封面降级（manual>image>trajectory>default）/ 异步刷新 / 轮询 | diary/service.go、`GET /diary/cover-url` | — |
| 日记统计 | `GET /diary/stats` | — |

## 自动记录（02c）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 开关自动记录 | `PUT /auto-record/config` | — |
| 轨迹上报（服务端幂等，迁移 000005） | `POST /auto-record/trajectories` | 迁移/真实库验证 |
| 驻留点聚类 | autorecord/service.go | autorecord/merge_test.go |
| 逆地理编码 | `GET /location/reverse` | location/client_test.go |
| 常用地址替换 / 摘要 | autorecord、后台任务 | — |
| 自动成文（后台 + 首次即时，互斥锁） | `POST /diary/details/auto` | diary/auto_lock_test.go |
| 候选公平轮转 / 逆地理终态 | keyset 游标 `job:cursor:auto_record`；`geocode_attempts>=10` 终态排除 | autorecord/scheduler_test.go |
| 新地点提醒 / 异常告警 | 后台任务 | — |
| 轨迹清理（7 天） | 后台任务 | — |

## 家庭 / 邀请（02d）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 家庭信息 / 创建 / 解散 | `GET/POST/DELETE /family` | — |
| 邀请链接生成 / 加入 / 奖励（奖励窗口统一注册后 7 天；被邀请奖励按 openid 终身一次） | `POST /family/invite-link`、`/family/invite-link/join` | — |
| 退出 / 移除成员 | `POST /family/leave`、`DELETE /family/members/{userId}` | — |
| 个人邀请短码解析 / 列表 | `GET /invite/resolve`、`/invite/list` | invite/qrcode_test.go |
| 个人邀请二维码 | `POST /invite/qrcode` | — |

## VIP / 支付（02e）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| VIP 查询 | `GET /user/vip`、`GET /vip` | — |
| 新用户试用自动领取 | `POST /vip/new-user` | vip/service_test.go |
| 免费 VIP 领取 / 查重（trial/free 按微信主体 openid 终身一次，注销墓碑行防重注册重领；check 优先按 openid 判） | `GET /vip/free`、`POST /vip/free/claim`、`GET /vip/free/check` | vip/service_test.go |
| 虚拟支付下单（沙箱闸门） | `POST /payment/virtual/request` | payment/request_test.go |
| 回调验签解密 + receive_id 校验 | `POST /api/prod/payment/virtualPayNotify` | payment/notify_crypto_test.go |
| 幂等发货 / 已关闭订单补发 | payment/service.go | payment/handler_test.go |
| 订单状态 / 取消 / 关闭 | `GET /payment/virtual/status`、`POST /payment/virtual/cancel` | payment/status_test.go |
| VIP 补发（客服/运营） | `ExtendVIPDaysWithTx` | vip/service_test.go |

## AI 对话（02f）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 小程序 SSE 对话 | `POST /ai/chat`（:8081） | ai/replay_test.go |
| SSE 连接治理（心跳/并发上限/断连释放） | ai/handler.go；`SSE_MAX_CONNS`（默认 200）、单用户 2 | ai/sse_limiter_test.go |
| 断线重连幂等（request_id + reply 缓存） | ai/service.go | ai/replay_test.go |
| 公众号对话 | `/wx/callback` | 人工（L7 F7 步骤 6~10） |
| 每日配额扣减 / 退款 | ai/service.go | — |
| 上下文组装（背景截断 2 万 runes） | ai/service.go | ai/service_test.go |
| 前端 AI 抽屉 | components/AIDrawer | 人工（L7 F7；存量 AIDrawer.test.ts 非门禁） |

## 文件 / 推送（02g）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 图片上传（白名单 / 配额 / 限流） | `POST /file/upload` | — |
| 文件下载 / 删除引用计数 | `GET /file/download/{fileId}`、`DELETE /file/{fileId}` | file/storage_test.go |
| 头像更新 + 地图 marker | `PUT /user/avatar` | — |
| 孤儿文件清理 | 后台任务 | — |
| 公众号验证 / 去重 / 客服消息 | `/wx/callback` | 人工（L7 F7） |
| 异常告警推送 / 新地点提醒 | 后台任务 | — |
| 订阅记录 | `POST /subscribe/record` | — |
| 已删图片边缘缓存批量收敛（ADR-0013） | `CDN_REFRESH_ENABLED=1` 时：删除 OSS 对象记录 `purge:oss:pending`，`purge_deleted_objects` 任务（默认 24h）分批调阿里云 CDN 刷新 | purge 包单测 |
| 前端上传部分失败仅补传失败项 | utils/http.ts | 人工（L7 F2；存量 uploadFile.test.ts 非门禁） |
| 支付失败/取消关单 | utils/pay.ts | 人工（L7 F6；存量 pay.test.ts 非门禁） |

## MCP / 开放（02h）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| API Key 管理（生成/查询/删除/轮换） | `/mcp/key*` | — |
| SaaS MCP 接入（Worker 转发，静态方法回源校验） | mcp.pathmemos.com | 人工（L7 F8；存量 mcp-worker vitest 非门禁） |
| 绑定页 `/auth` | Worker | 人工（L7 F8 步骤 3） |
| 工具调用 store_memory / query_memories | `/internal/mcp/rpc` | — |
| REST 三件套 | `/internal/mcp/diary`、`/internal/mcp/memories` | — |
| 开源版直连 `/mcp/*` | `DEPLOYMENT_MODE=open` | 人工（L7 F12） |
| api-worker 私有化路由 + SSRF 黑名单 | api.pathmemos.com | 人工（L7 F12；存量 api-worker vitest 非门禁） |
| Worker GET 缓存（TTL 分档） | mcp-worker/src/lib.ts | 同上 |
| 记忆写版本（写后 bump 缓存版本） | mcp-worker/src/index.ts | — |

## 系统 / 运维（05）

| 能力 | 入口 / 实现 | 测试/验收 |
|------|------------|------------|
| 健康检查（live / ready / 兼容别名 `/health`） | `GET /health/live`、`GET /health/ready`、`GET /health` | — |
| 能力探测 | `GET /system/config` | — |
| 限流（IP / Key / 会话配额，统一 429 `code=4290`+`biz_code=RATE_LIMITED`） | middleware | — |
| 访问日志（`route`/`duration_ms`）+ 客户端运维日志 | middleware、`POST /ops/client-log`（保留 30 天）；`scripts/accesslog-p95.sh` 统计分位 | — |
| 后台任务（自动记录/告警/关单/清理/AI 日志/客户端日志，共 10 项（含 purge_deleted_objects，ADR-0013），PG advisory lock 互斥；auto_record 同日去重集合化、成文失败保留轨迹下轮重试） | jobs/runner.go | — |
| 后台任务观测 / 失联检测 / 人工补跑 | Redis `job:last_success\|last_failure\|fail_streak:{task}`；`job_stale`（>3× 周期）；`papafeiji-admin jobs status` / `job run <name>` | jobs/runner_test.go |
| 部署契约（零停机分层切换） | deploy/deploy.sh | — |
| Cloudflare 入口自动装配（API/官网域名 A 记录 DNS-only 装配 + api/mcp Worker 自动部署与 secret 同源同步，`--skip-workers` 可跳过） | deploy/deploy.sh | — |
| OSS 连接自检（五项成组校验、bucket 缺失自动创建、默认静态资源上传） | deploy/deploy.sh | — |
| 官网静态站（`papafeiji.cn`/`www`，Astro 构建产物随部署发布，nginx 按 Host 与 API 共用端口；证书 certbot 独立 lineage；构建失败仅警告跳过、不阻断后端部署） | `website/`、deploy/deploy.sh、deploy/nginx/*.conf | — |
| 告警通道（随部署装配：alert-cron 每 2 分钟 / alert-p95 每 15 分钟 / cert-check 每小时 / 控制机 uptime-check / watchdog 容器 CPU 异常 `papafeiji_cpu_high`；`CFG_ALERT_WEBHOOK_URL` 强烈建议必配） | scripts/alert-cron.sh、alert-p95.sh、deploy/cert-check.sh、scripts/uptime-check.sh、deploy/watchdog.sh（ADR-0015） | — |
| L7 滥用防护（nginx 限流 + 单 IP 连接上限 + fail2ban 限流打穿自动封禁） | deploy/nginx/default.conf、deploy/deploy.sh 远端 fail2ban jail 装配 | — |
| 慢 SQL 榜单 / 迁移预演 / AI 成本观测 | scripts/sql-top.sh（pg_stat_statements，迁移 000007）、scripts/rehearse-migration.sh、scripts/ai-cost.sh（`msg="ai chat usage"` 日志） | — |
