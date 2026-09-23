# PP-02F AI 对话（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：无
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。行内 `路径` 为事实来源。

## 1. 背景与目标

- 服务对象：微信小程序登录用户（SSE 流式多轮对话）；微信公众号/服务号关注者（纯 AI 对话终端，`docs/ARCHITECTURE.md` `2.2`）。
- 解决的问题：让用户以自然语言回忆家庭日记内容；对上游大模型做统一封装、日配额控制、断线幂等与降级。
- 本域边界：
  - 负责：小程序 AI 对话接入（SSE）、公众号对话回调、上下文组装、上游调用、日配额扣减/退款、断线重连幂等、对话日志落库。
  - 不负责：日记与记忆的写入/查询（见 02b / 02h）、VIP 判定（见 02e，本域只消费 `vip.InfoProvider`）、MCP 协议与 API Key（见 02h）。
- 现状目标（由代码常量决定）：单条消息 ≤2500 code points、body ≤32KB；单次对话 ≤180s；非 VIP 10 次/自然日、VIP 100 次/自然日；同一轮重连不重复扣配额、不重复落库。

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | app(:8080) 与 sse(:8081) 双 HTTP Server；`/ai/chat` 只注册在 sseRouter | AI 流式长连接与普通 REST 超时/缓冲策略不同，需独立 Server（`backend/cmd/server/main.go`；Nginx `location /ai/chat` 反代 `papafeiji_sse`，`deploy/nginx/default.conf`） | 无 |
| D2 | `ai.Service` 同时服务小程序 SSE 与公众号；公众号用 `ChatWithPromptUsingConfig(..., WECHAT_MP_PROMPT, ...)` 自定义 prompt | 业务逻辑与传输解耦，公众号与小程序共享配额/幂等/上游 | 无 |
| D3 | 配额「先 Redis 闸门，后 DB 权威原子扣减；无有效回复必退款」 | Redis 仅作快速预检，DB 的 `WHERE used<quota` 才是权威；Redis 抖动 fail-open 不绕过配额 | 无 |
| D4 | 幂等键优先取客户端 `request_id`（每轮生成、重连复用），缺失时回退消息内容哈希 | 精确覆盖「断线重连」，避免重复调用上游/扣配额/落库（`backend/internal/ai/service.go` `aiTurnKeys`） | 无 |
| D5 | 上游为 OpenAI 兼容接口 `POST {baseUrl}/v1/chat/completions`，`stream:true` + `stream_options.include_usage:true`（流末尾 usage chunk 记入 `msg="ai chat usage"` 结构化日志：`user_id`/`prompt_tokens`/`completion_tokens`，尽力而为不落库，`scripts/ai-cost.sh` 汇总）；上游建流失败/非 200/中途流错误且无回复时输出限频告警 `alert=ai_upstream_error`（5 分钟一次） | 对接 DeepSeek 等兼容网关；thinking 由 `AI_THINKING_TYPE` 可选开启；成本与上游故障此前不可见（ADR-0015） | 无 |
| D6 | 上游流上下文用 `context.WithoutCancel` 派生，客户端断开后继续消费至 EOF 并写回放缓存 | 重连可零成本回放，避免重复扣费（`service.go`） | 无 |
| D7 | 小程序不使用标准 EventSource；前端用 `wx.request` + `enableChunked` 自解析 SSE | 微信小程序无 EventSource/ReadableStream（`frontend/.../utils/eventSource.ts`） | 无 |
| D8 | 对话日志异步落库，后台删除 **>90 天**的日志 | 控制表体积，日志仅用于调试 | 无 |
| D9 | 本域**未使用 Redis pub/sub** | 全仓库未发现 Publish/Subscribe 调用；app 与 sse 仅通过同一 Redis 的键（session/配额/幂等/缓存）协作 | 无 |
| D10 | AI 变现边界 = 每日配额（非 VIP 10 / VIP 100）+ 单次输入限制 + 基础限流，到此为止 | 不建 Token 余额、计费、精确成本核算体系；配额即全部 | 无 |
| D11 | SSE 连接治理：数据/心跳/结束/错误事件串行写入；空闲每 25s 发 `: heartbeat`；单用户并发 2、进程内 `SSE_MAX_CONNS`（默认 200，仅约束本 sse 进程）；超限开流前 429；客户端断开立即释放名额，上游仍消费到 EOF 写回放缓存 | 防慢连接/断连泄漏占用；单轮为独立 POST、受 `AIStreamTimeout=180s` 约束，无跨轮长连接 | 无 |

## 3. 核心流程（用户故事 + 时序）

### AI-1 小程序 AI 对话（SSE 流式）

作为小程序用户，我想和 AI 多轮对话回忆家庭日记。

验收标准：
- 前端向 `{getSSEBaseURL()}/ai/chat` 发送 POST JSON（`AIDrawer.ts`），Authorization 为 `Bearer <sessionId>`；私有化模式附加 `X-Private-Api-Key`。
- 后端先写 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、HTTP 200 并 flush（`ai/handler.go`）。
- 普通分片事件体为 `data: <line>`（多行内容逐行加前缀，事件间空行）；结束事件 `event: done`；错误事件 `event: error` + `{"code","biz_code","bizCode","message"}`（`bizCode` 为**过渡兼容字段**（代码注释：待新版本全量后移除），供旧客户端读取；`ai/upstream.go`）。
- 校验失败在开流前返回普通 JSON：消息 trim 为空 `message is required`、>2500 code points `message too long`、`request_id` 不匹配 `^[A-Za-z0-9_-]{8,64}$` → `invalid request_id`、JSON 非法 → `invalid request body` 均 400 `code=4000`；body >32KB → 413 `code=4130`。
- 全轮超时 `AIStreamTimeout=180s`。

### AI-2 断线重连幂等

验收标准：
- 键：`aiTurnKeys(userID, message, requestID)` 对 `userID + 换行符 + (requestID 或 message)` 取 SHA-256；`ai:turn:<hex>`（TTL 240s，处理中标记）、`ai:reply:<hex>`（TTL 10min，完整回复）。
- 命中 `ai:reply:<hex>`：按 `aiReplayChunkRunes=120` 字符/片回放，直接返回缓存，不扣配额、不落库。
- 未命中且 `SETNX ai:turn` 成功：正常生成；失败（已在途）：每 `aiTurnPollInterval=200ms` 轮询，等待 `ai:reply` 出现或 `ai:turn` 消失，窗口 `aiTurnKeyTTL=240s`（实际受外层 180s 请求上下文约束）。
- **等待超时或在途失败 → 直接返回 SSE error（`code=4290`、`biz_code=OPERATION_IN_PROGRESS`、`message="ai turn in progress, retry later"`），不回落生成、不扣配额、不落库**；残留标记由 TTL 兜底。
- 上游流以 EOF 完整结束且回复非空时，先写 `ai:reply` 再删 `ai:turn`（顺序固定）。
- 断言：同一 `request_id` 重发，`ai_daily_quota_usage.used` 不额外 +1，`ai_dialog_logs` 不额外新增行（含在途冲突分支）。
- 边界：不带 `request_id` 的调用方（公众号）在 10 分钟内主动重复发送同一消息会命中回放（`service.go` 注释，已接受）。

### AI-3 公众号对话

验收标准：
- 路由 `GET/POST /wx/callback`（app，公开）；GET 验签成功原样回显 `echostr`，失败 HTTP 403 + `fail`。
- POST：`msg_signature` 存在即走加密模式，必须校验加密签名（不回退明文签名），解密后 `appid` 必须等于 `WECHAT_MP_APPID`（配置非空时）；否则走明文 `signature` 校验。`ToUserName` 与 `WECHAT_MP_GHID` 不一致则返回 `success` 丢弃。
- 去重：text/voice 且 `MsgId` 非空时 Redis `wxmp:msgid:<MsgId>` SetNX TTL 60s；重复消息被动回复「正在思考，请稍候…」，避免触发重复 AI。
- text：立即被动回复「正在思考，请稍候…」，后台 goroutine 内先 `resolveUser`（未绑定则异步发引导）再调用 AI（超时 `AIStreamTimeout+30s`）；回复按 `wxmpKfTextByteLimit=2000` 字节拆分为客服消息（`SendKfMessage`）推送，发送前 `stripMarkdown`。
- 客服消息**全部分段均失败**时记录 `[ALERT] wx mp all kf segments failed` 日志（部分失败仅计数，不告警）。
- 其他兜底分支：非 text/voice/event 消息与空文本回「暂只支持文字和语音消息…」；AI 调用失败（非配额类）回「服务繁忙，请稍后再试。」；去重检查 Redis 故障时降级为继续处理（不丢弃消息）。
- voice：使用微信识别结果 `Recognition`；为空回「未能识别这段语音…」。
- 未绑定用户：被动回复仍是 `wxmpReplyThinking`「正在思考，请稍候…」，后台 `resolveUser` 失败后以客服消息 `wxmpReplyNoUser`「您尚未在小程序中登录…」异步推送，并异步推送小程序卡片（`WechatAppID` + `pages/index/index`）。
- 配额超限：客服消息回「您当天的对话额度已用完，请明天再试。」。
- event：`subscribe` 更新 `wx_mp_accounts.subscribed=true`、异步 `resolveUser`、异步推送小程序卡片、回欢迎语；`unsubscribe` 置 false，返回空回复。

### AI-4 配额扣减与退款

验收标准：
- 配额：`dailyQuotaNonVIP=10`、`dailyQuotaVIP=100`；VIP 判定来自 `vipService.GetVIPInfo(ctx,userID).IsVIP`。
- 日期：`timeutil.NowShanghai()` 的当天 0 点，`quota_date` 为 date。
- Redis 键 `ai:daily_chat:<userID>:<YYYY-MM-DD>`，TTL = 到次日 0 点（上海）的剩余秒数 + 60s；Lua `dailyQuotaLua` 原子 incr，首次设 expire，>limit 时 decr 并返回 0。
- Redis 报错 → 记 warn 后降级放行（fail-open），继续走 DB 权威。
- DB 扣减：`IncrementAIDailyQuotaUsed`（ON CONFLICT user/quota_date DO UPDATE used=used+1 WHERE used < quota）；未实际扣减时回补 Redis 并返回配额不足。
- 退款触发：上游建流失败、流取消/错误、超时且无回复、空回复。退款 Db `DecrementAIDailyQuotaUsed`（GREATEST(used-1,0)），最多 4 次尝试（首次 + 3 次退避 100/200/300ms，每次 5s 超时）。
- 退款成功后 Lua `refundDailyQuotaLua` 原子退 Redis；key 不存在时按 DB 返回值重建。
- 4 次仍失败 → 记录 `alert:ai_quota_refund_failed`（含 user_id/quota_date）；用户并发注销导致的 `pgx.ErrNoRows` 视为成功不告警。

### AI-5 上下文组装与落库

验收标准：
- 背景：`user.CurrentFamilyID` 为空返回空串；否则 Redis `ai:family_summary:<familyID>`（TTL `backgroundCacheTTL=30min`）命中即用；miss 时 `ListAIBackgroundEntries(familyID, maxBackgroundEntries=2000)`（家庭维度 LIMIT 2000；子查询每用户近 90 天最多 500 条，格式 `YYYYMMDD HH24时 | 地址 | 文本`），拼 `昵称:\n内容`，超 `maxBackgroundLen=20000` runes 截断（按上游窗口保守预算）；无记录写入并返回「暂无日记记录」。
- 不可信数据（I7 落地断言）：日记背景文本仅作为**引用资料**拼入 system 背景；背景中的指令性内容（如「忽略以上设定」类文本）不得改变系统行为——验收断言：背景含此类文本时，回复仍遵循系统 prompt 且不执行该指令。
- 跨域失效：family/diary/autorecord 变更时删除 `ai:family_summary:<familyID>`（`family/service.go`、`autorecord/service.go`、`diary/service.go`）。
- 近期上下文：`ListRecentDialogLogs(userID, 20)`（created_at > now()-7 days，DESC）→ `buildMessages` 从新到旧累计，单条计入 rune 数，累计 >2000 runes 或 ≥20 条停止，合并为一条 system「近期对话上下文（最近 7 天）：」。
- messages 顺序：system(系统 prompt + 背景/占位替换) → 可选 system(「当前提问者是：<nickname>…」) → 可选 system(近期上下文) → user。顺序不得改变（DeepSeek 前缀缓存，`upstream.go` 注释）。
- 占位符：`{nowDate}` → `YYYY年MM月DD日`（上海）；`{userNickname}` → 「当前用户」；`{userDiaryDetails}`/`{familyDiaryDetails}` → 背景；systemPrompt 为空时用「你是一个日记助手。」。
- 语言：`user.Lang=="en"` 时 system 首部加英文强制指令且 user 内容前置 `(Answer in English) `；`"zh-Hant"` 时加繁中指令。
- 落库：回复完成后 `safe.Go` 异步、10s 超时、`db.WithTx` 事务写 `ai_dialog_logs`：先 user 行，回复非空再 assistant 行；两条均含同一 `created_at`。

### AI-6 上游异常与超时

验收标准：
- 上游 HTTP 非 200 → 报错（`upstream status N`）并退配额。
- 上游 payload 含非空 `error.message` → 报错；JSON 解析连续失败 >3 次 → 报错；每成功解析一次计数清零。
- SSE 行仅识别 `data: ` 前缀，忽略空行与其他行；`[DONE]` → io.EOF；空 chunk 跳过；`delta.reasoning_content` 不输出。
- 180s 超时：若已累计部分回复则保存并正常返回；否则退配额并下发 `event:error` `code=5001` `message=timeout`。
- 客户端断开：`onChunk` 失败置 `clientGone`，继续消费上游到 EOF（供回放缓存），不再向已断开客户端写。

## 4. 数据模型

### 4.1 表

| 表 | 关键字段 / 约束 | 说明 |
|----|----------------|------|
| `ai_daily_quota_usage` | `user_id` + `quota_date` 复合主键；`used int default 0`；`created_at/updated_at`；索引 `idx_ai_daily_quota_usage_date(quota_date)`；`user_id` **无外键**（与 04-database §3.15 一致） | 每用户每日已用次数（上海自然日）；注销由应用层事务清理（02a A-8），非级联 |
| `ai_dialog_logs` | `id` 主键；`user_id` FK CASCADE；`role CHECK(user/assistant/system)`；`content text`；`created_at`；索引 `(user_id, created_at)`、`(created_at)` | 对话日志；后台删除 >90 天 |
| `users` | `id/nickname/lang/current_family_id` | 读取身份、语言、当前家庭 |
| `wx_mp_accounts` | `mp_openid/unionid/user_id/subscribed/subscribe_time/last_interact_time` | 公众号绑定；本域更新订阅状态与互动时间 |
| `family_members` / `diaries` / `diary_entries` / `users` | — | 背景构建只读（`ai.sql` `ListAIBackgroundEntries`） |

来源：`backend/migrations/000001_baseline.up.sql`（`api_keys` 之外的 AI 相关表均在该基线内）。

### 4.2 Redis 键

| 键 | TTL | 用途 | 位置 |
|----|-----|------|------|
| `ai:daily_chat:<userID>:<YYYY-MM-DD>` | 到次日 0 点（上海）+60s | 配额前置闸门计数 | `ai/service.go` |
| `ai:turn:<sha256>` | 240s | 同一轮「处理中」标记 | `ai/service.go` |
| `ai:reply:<sha256>` | 10min | 完整回复回放缓存 | `ai/service.go` |
| `ai:family_summary:<familyID>` | 30min | 家庭日记背景缓存 | `ai/service.go` |
| `wxmp:msgid:<MsgId>` | 60s | 公众号消息去重 | `wxmp/handler.go` |
| `wxmp:thumb_media_id` | 48h | 小程序卡片缩略图 media_id | `wxmp/handler.go` |
| `wxmp:profile:refresh:<openID>` | 1h | 微信资料异步刷新节流 | `wxmp/handler.go` |
| `wechat:mp:access_token:<appID>` | 110min | 公众号 access_token 缓存 | `wxmp/client.go` |

## 5. API 契约

统一响应结构（`backend/internal/middleware/response.go`）：`{ code, biz_code?, message, data?, extra?, count?, nextCursor?, request_id }`；成功 `code="0000"/message="ok"`。错误码词汇见 03-api §3；对应常量在 `backend/pkg/errors/codes.go`（本域相关：`4000`、`4010`、`4030`、`4290`、`5001`、`AI_DAILY_QUOTA_EXCEEDED`、`RATE_LIMITED`、`SESSION_INVALID`）。

### 5.1 端点登记

| 方法 | 路径 | 载体 | 鉴权 | 说明 |
|------|------|------|------|------|
| POST | `/ai/chat` | sse:8081（Nginx 反代） | SaaS：Session；open：Session 或 `X-Private-Api-Key` | AI 对话 SSE；另有 `30/min/IP` 应用层限流 |
| GET | `/wx/callback` | app:8080 | 微信签名 `WECHAT_MSG_TOKEN` | 服务器验证，回显 `echostr`；失败 403 `fail` |
| POST | `/wx/callback` | app:8080 | 微信签名或加密 `msg_signature` | 公众号消息回调 |
| GET | `/system/config` | app:8080 | 公开 | 返回 `mode`、`features.ai`、`features.mcp` 等，前端用于显隐入口（`backend/internal/system/handler.go`） |

> `/ai/chat` 不存在于 app:8080；请求需经 Nginx 路由到 sse:8081。SaaS 直连 `pro.papafeiji.cn`，私有化模式经 `api.pathmemos.com`（api-worker 对 `/ai/chat` 采用流式转发、无 60s 超时，`api-worker/src/index.ts`）。

### 5.2 POST /ai/chat 请求体

| 字段 | 类型 | 必填 | 约束 |
|------|------|------|------|
| `message` | string | 是 | trim 后非空；≤2500 code points |
| `request_id` | string | 否 | 8–64 位 `[A-Za-z0-9_-]`；同一轮重连复用 |

> 请求体字段仅 `message`（必填）与 `request_id`（可选）；多轮历史由后端 `ListRecentDialogLogs`（近 7 天、最多 20 条）组装（见 AI-5）。

### 5.3 POST /ai/chat 响应（SSE）

| event | data | 说明 |
|-------|------|------|
| （无 event 名，默认 message） | 文本分片（多行各自 `data: ` 前缀） | 逐片推送；每事件以空行结束 |
| `done` | 空 | 流正常结束 |
| `error` | `{"code":"...","biz_code":"...","message":"..."}` | 业务/上游错误；HTTP 状态已固定为 200 |

错误映射（SSE error 体 `{code, biz_code, message}`；HTTP 层错误为统一 envelope）：

| 场景 | HTTP | code | biz_code | message |
|------|------|------|----------|---------|
| body >32KB | 413 | `4130` | — | `request body too large` |
| body JSON 非法 | 400 | `4000` | — | `invalid request body` |
| message 为空 | 400 | `4000` | — | `message is required` |
| message 过长 | 400 | `4000` | `TEXT_TOO_LONG` | `message too long` |
| request_id 非法 | 400 | `4000` | — | `invalid request_id` |
| SSE 不支持（理论分支） | 500 | `5001` | — | `streaming not supported` |
| 配额超限 | 200(SSE) | `4290` | `AI_DAILY_QUOTA_EXCEEDED` | `daily ai chat quota exceeded` |
| 在途冲突（等待超时/在途失败，不回落生成） | 200(SSE) | `4290` | `OPERATION_IN_PROGRESS` | `ai turn in progress, retry later` |
| 超时 | 200(SSE) | `5001` | — | `timeout` |
| 上游错误 | 200(SSE) | `5001` | — | `upstream error` |
| IP 限流 | 429 | `4290` | `RATE_LIMITED` | `too many requests` |
| SSE 并发超限（单用户 >2 或全局 >`SSE_MAX_CONNS`） | 429 | `4290` | `RATE_LIMITED` | `too many concurrent ai chat connections` |
| 未认证（SaaS） | 401 | `4010` | `SESSION_INVALID` | 由 Session 中间件给出 |
| 未认证（open） | 401 | `4010` | `SESSION_INVALID` | 由 OpenAuth 中间件给出 |

### 5.4 公众号被动回复

| 模式 | 报文 |
|------|------|
| 明文 | `ReplyTextXML`（`<xml>…<MsgType>text</MsgType><Content>…`） |
| 加密 | `EncryptReplyXML`（`<Encrypt>/<MsgSignature>/<TimeStamp>/<Nonce>`，AES-256-CBC + PKCS7 按 32 字节对齐，`wxmp/crypto.go`） |

## 6. 关键实现约束

### 6.1 阈值（改了就出问题）

| 项 | 值 | 位置 |
|----|-----|------|
| 单条消息上限 | 2500 code points | `ai/handler.go` `maxMessageCodePoints`；`ai/service.go` 复核 |
| 请求 body 上限 | 32KB | `ai/handler.go` `maxChatBodySize` |
| SSE 流超时 | 180s | `ai/handler.go` `AIStreamTimeout` |
| 非 VIP / VIP 日配额 | 10 / 100 | `ai/handler.go` |
| 配额 Redis TTL | 到次日 0 点（上海）+60s | `service.go` `dailyQuotaCacheTTLFor` |
| 退款重试 | 最多 4 次尝试（首 + 退避 100/200/300ms） | `service.go` `refundDailyQuota` |
| 同轮处理中标记 TTL | 240s | `service.go` `aiTurnKeyTTL` |
| 回复回放缓存 TTL | 10min | `service.go` `aiReplyTTL` |
| 在途轮询间隔 | 200ms | `service.go` `aiTurnPollInterval` |
| 回放分片 | 120 runes | `service.go` `aiReplayChunkRunes` |
| 背景缓存 TTL | 30min | `ai/handler.go` `backgroundCacheTTL` |
| 背景内容上限 | 20000 runes | `service.go` `maxBackgroundLen` |
| 背景查询 | 家庭 LIMIT 2000；子查询每用户 500 条 / 近 90 天 | `ai.sql` `ListAIBackgroundEntries` |
| 历史上下文 | ≤2000 runes 且 ≤20 条；查询近 7 天 | `upstream.go` `buildMessages`；`ai.sql` `ListRecentDialogLogs` |
| 上游解析容错 | JSON 失败累计 >3 次报错 | `upstream.go` |
| 上游 HTTP 客户端上限 | 5min（安全上限；实际生效上限为 `AIStreamTimeout=180s`；该 client 仅 `ai/upstream.go` 流式调用使用）；Scanner 缓冲上限 2MB | `config.SSEHTTPClient()`、`upstream.go` |
| 对话日志保留 | 删除 >90 天；批 1000 | `jobs/runner.go` `runCleanupAILogs`；`ai.sql` |
| 清理任务间隔 | 默认 24h（ticker 首次在 interval 后触发），PG advisory lock `lock:background:cleanup_ai_logs` | `jobs/runner.go`；`JOB_INTERVAL_CLEANUP_AI_LOGS` |
| 前端输入上限 | 2500 字符（与后端 `maxMessageCodePoints` 统一） | `AIDrawer.ts`；`ai/handler.go` |
| 前端 SSE 缓冲 | 64KB；`wx.request` timeout 200s；传输类失败自动重连 1 次 | `utils/eventSource.ts` |

### 6.2 锁 / 幂等 / 事务 / 降级

- 幂等：`request_id` → `ai:turn` / `ai:reply`（`6.1` 与 AI-2）。
- 配额一致性：Redis 为预检、DB 为权威；Redis 异常 fail-open；DB 扣减失败或未扣减时回补 Redis。
- 事务：仅对话日志落库用 `db.WithTx`；`saveLog` 在同一事务内写 user/assistant 两行。
- 降级：Redis 读背景/回放/闸门失败均不阻断对话；缓存写失败仅失去缓存能力。
- 后台任务统一用 `internal/pkg/safe.Go`，panic 记录日志。
- 跨进程：app 与 sse 仅共享 Redis 数据（session、配额、幂等键、缓存），**无 pub/sub**。

## 7. 前端接入

| 位置 | 说明 |
|------|------|
| `frontend/miniapp/miniprogram/components/AIDrawer/AIDrawer.ts` | 对话抽屉：发送、SSE 生命周期、消息裁剪（`MAX_MESSAGE_COUNT=50`）、配额弹窗、重试、日记卡片回填 |
| `frontend/miniapp/miniprogram/utils/eventSource.ts` | `wx.request`+`enableChunked` SSE 解析；`MAX_BUFFER_SIZE=64KB`；传输失败自动重连 1 次并清空半截输出；识别 `event: done/error` |
| `frontend/miniapp/miniprogram/utils/request.ts` | 登录与 session 获取；发送前 `request.login` |
| `config/index.ts` | `getSSEBaseURL()` 返回 `getBaseURL()`：develop → `http://localhost:8080`；private → `https://api.pathmemos.com`；否则 `https://pro.papafeiji.cn` |

关键交互约束：
- 每次发送生成 `request_id = Date.now() + "_" + 随机串`（`AIDrawer.ts`），`eventSource` 重连复用同一 `data` 对象（同一 request_id）。
- 错误 `biz_code=="AI_DAILY_QUOTA_EXCEEDED"`（code=4290）→ 展示配额弹窗并跳转 `/pages/sub/Vip/Vip`。
- AI 文本中 `$$YYYYMMDD$$` 日期标记会被抽取出并调用 `POST /diary/info/dates` 拉取日记卡片；正文中该标记被移除并转 Markdown HTML。
- 页面隐藏/卸载中止 SSE（`_abortSSE`），`_sending` 由 onclose/onerror 复位。

## 8. 运维与任务

### 8.1 环境变量（`backend/internal/config/config.go`）

| 变量 | 默认 | 说明 |
|------|------|------|
| `AI_API_KEY` | 无（必填，缺省启动校验失败） | 上游 `Authorization: Bearer` |
| `AI_BASE_URL` / `AI_MODEL` | 无（必填，缺省启动校验失败） | AI 上游地址与模型（唯一来源） |
| `DEPLOYMENT_MODE` | `saas` | `saas` / `open` |
| `WORKER_SECRET` | 空 | sseRouter 与 apiRouter 的 `WorkerAuth` 共享密钥；SaaS 必填（空则启动校验失败），open 模式必须留空 |
| `REDIS_ADDR` / `REDIS_PASSWORD` | — | 配额/幂等/缓存/session |
| `HTTP_BIND`/`HTTP_PORT` | `127.0.0.1`/`8080` | app |
| `SSE_BIND`/`SSE_PORT` | `127.0.0.1`/`8081` | sse |
| `SSE_MAX_CONNS` | `200` | 本进程 SSE 并发连接上限（单用户固定 2） |
| `WECHAT_MP_APPID` / `WECHAT_MP_SECRET` / `WECHAT_MP_GHID` | 空 | 公众号；未配置时 `IsConfigured()=false` 不刷新资料 |
| `WECHAT_MSG_TOKEN` / `WECHAT_ENCODING_AES_KEY` | 空 | 回调验签 / 加密模式 |

### 8.2 AI 配置（环境变量）

| 变量 | 映射 |
|------|------|
| `AI_BASE_URL` / `AI_MODEL` | 上游地址 / 模型（唯一来源） |
| `AI_MAX_OUTPUT_TOKENS` | `max_tokens`（>0 才发送） |
| `AI_THINKING_TYPE` | `thinking.type`（非空才发送） |
| `AI_PROMPT` | 小程序系统 prompt（代码内默认值，可环境变量覆盖） |
| `WECHAT_MP_PROMPT` | 公众号 prompt；空则回退 `AI_PROMPT` |

### 8.3 任务与入口

- 后台任务：`runCleanupAILogs`（`jobs/runner.go`），间隔 `JOB_INTERVAL_CLEANUP_AI_LOGS`（默认 24h）。
- Nginx：`location /ai/chat` → `papafeiji_sse`，`proxy_buffering off`、`proxy_read_timeout 300s`、限流 `api burst=50`（`deploy/nginx/default.conf`；开源版同结构 `nginx.open.conf`）。
- api-worker：`/ai/chat` 采用流式转发、可超过 60s（避免截断 SSE）；其余请求默认 60s（`/file/upload`、`/file/download` 360s，完整超时表见 02h §6.1）。
- sse Server：`ReadTimeout=0`、`WriteTimeout=0`（否则长流被 deadline 掐断），`IdleTimeout=60s`（`main.go`）。
