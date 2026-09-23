# PP-02H MCP 与开源版接入（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：无
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。行内 `路径` 为事实来源。

## 1. 背景与目标

- 服务对象：使用 Cursor / Kimi 等 MCP 客户端的外部 AI 工具；以及自行部署开源版（`DEPLOYMENT_MODE=open`）的用户。
- 解决的问题：让外部 AI 工具用 API Key 读写用户的日记与记忆；SaaS 侧隐藏源站、由 Cloudflare Worker 充当唯一公网 MCP 入口；开源版可直连自部署后端。
- 本域边界：
  - 负责：API Key 生命周期（创建/查询/删除/轮换）、MCP Streamable HTTP 协议与两个工具、MCP REST 三件套、Worker 公网入口（`mcp.pathmemos.com`）、开源版公开 `/mcp/*`、api-worker 私有化路由（`api.pathmemos.com`）。
  - 不负责：日记/记忆的业务语义（见 02b 与记忆相关分册）、AI 对话（见 02f）、VIP（见 02e）、Cloudflare 账号与 KV 的创建（见 `docs/mcp-worker-ops.md`）。

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | MCP 采用 Streamable HTTP（spec 2024-11-05），每次请求为无状态 POST，无持久连接/无 in-memory session | 降低源站连接与协议解析开销，免 session 清理（`backend/internal/mcp/server.go`） | 无 |
| D2 | SaaS 唯一公网入口为 Cloudflare Worker `mcp.pathmemos.com`；源站仅暴露 `/internal/mcp/*`（`X-Worker-Secret` 保护） | 隐藏源站 IP、获得边缘接入（`docs/ARCHITECTURE.md` `2.3`） | 无 |
| D3 | 开源版（`DEPLOYMENT_MODE=open`）源站直接注册公开 `/mcp`、`/mcp/diary`、`/mcp/memories`；`/internal/mcp/*` 仅在 SaaS 模式注册 | 不依赖 Worker 也能用（`main.go:235`；`mcp/rpc.go` `RegisterPublic`） | 无 |
| D4 | API Key 同时存明文（`api_keys.api_key`，产品要求永久展示）与 SHA-256（`key_hash`，认证用）；永不过期（`expires_at=9999-12-31T23:59:59Z` 占位） | 满足「永久展示」需求；认证只查 hash，不校验过期（`mcp/handler.go` `apiKeyNeverExpires`） | ADR-0007 |
| D5 | Worker 边缘直接响应静态方法（`tools/list`、`prompts/list`、`prompts/get`），动态方法回源；GET 查询在 Worker 边缘缓存 | 减少回源与握手延迟（`mcp-worker/src/index.ts`） | 无 |
| D6 | GET 缓存键叠加「记忆写版本」，`store_memory` 成功后换版本，使该 Key 全部查询缓存整体失效 | 让近期查询也可安全缓存，消除 store 后 30 分钟脏读（`mcp-worker/src/index.ts`） | 无 |
| D7 | 小程序私有化后端由 api-worker 按 `X-Private-Api-Key` 查 KV 路由到用户自部署后端 | 用 API Key 作路由键，避免 session 刷新导致路由失效（`api-worker/src/index.ts`） | 无 |
| D8 | 请求/响应体上限 64KB；超预算结果截断并返回 `truncated` | 防大文本查询内存峰值与超时（`mcp/handler.go`、`mcp/server.go`） | 无 |
| D9 | API Key 身份（`apikey:` 前缀）禁止调用 `/mcp/key*` 管理接口 | 开源版默认用户的 key 即 `OPEN_API_KEY`，Rotate/Delete 会弄挂 Worker 鉴权（`mcp/handler.go` `rejectAPIKeyIdentity`） | 无 |

## 3. 核心流程（用户故事 + 时序）

### MCP-1 小程序管理 API Key

验收标准：
- `GET /mcp/key`：无 key 返回 `{"hasKey":false}`（`code=0000`）；有 key 返回 `apiKey/apiUrl/memoryUrl/expiresAt/mcpConfig`，并附 `hasKey:true`。
- `POST /mcp/key`：get-or-create 幂等；已有 key 时返回已存在 key（依赖 `CreateAPIKey ON CONFLICT (user_id) DO NOTHING` + 冲突后回查），不换发。
- `POST /mcp/key/rotate`：事务内先 `DeleteAPIKeyByUser` 再 `CreateAPIKey`，原子换发；失败整体回滚、旧 key 保留。
- `DELETE /mcp/key`：按 user 删除；无 key 也返回成功空对象。
- 四个端点均在 session 鉴权路由组内；若身份为 `apikey:<userID>`（开源版 `X-Private-Api-Key` 通路）→ HTTP 403 `code=4030`，message「open 模式默认身份的 key 即 OPEN_API_KEY，不可管理；key 管理需登录会话」。
- key 生成：`util.NewRandomToken(32)`；`expiresAt` 固定 `9999-12-31T23:59:59Z`（RFC3339 输出）。
- 返回的 `mcpConfig` 固定结构：`{"mcpServers":{"memory":{"url":"<公开入口>/mcp","headers":{"Authorization":"Bearer <key>"}}}}`；`apiUrl` 指向 GET `/mcp/diary`，`memoryUrl` 指向 POST `/mcp/memories`；配置了 `MCP_PUBLIC_URL` 时三者均指向该入口，并额外返回 `authUrl=<base>/auth`。

### MCP-2 SaaS MCP 客户端接入（Worker 转发）

验收标准：
- 客户端访问 `https://mcp.pathmemos.com/mcp`，鉴权优先 `Authorization: Bearer <apiKey>`，其次 `?t=<token>` 查 KV `mcp_token:<token>`。
- 两者都无 → HTTP 401 文本 `Unauthorized`。
- `/mcp` 仅接受 POST；非 POST 返回 HTTP 405 + JSON-RPC 错误 `{"jsonrpc":"2.0","id":null,"error":{"code":-32601,"message":"Method Not Allowed: only POST is supported"}}`。注意 405 判定**先于鉴权**：未带凭据的非 POST 请求返回的是 405 而非 401。
- 静态方法在 Worker 边缘响应：`tools/list`、`prompts/list`、`prompts/get`（内容须与 Go 后端 `server.go` 同步）；响应带 `X-Worker-Cache: STATIC`。这三处边缘响应的内容为 Go 后端 `server.go` 的副本，两处同步由 `scripts/check_mcp_static_sync.py` 机械对账 tools/prompts 的 name+description 与 prompt 正文首行（spec-check 提示级第 11 项，CI 同步执行）；正文全文仍为人工维护。静态方法响应前须回源校验 API Key（`GET /internal/mcp/diary?limit=0` + `X-Worker-Secret`，10s 超时，fail-closed）；校验结果正缓存 60s（Cache API `__keycheck/<sha256前16位>`）。
- 其余回源 `{BACKEND_URL}/internal/mcp/rpc`，转发头为**白名单重建**：仅 `Content-Type`（缺省补 application/json）、`Authorization: Bearer <apiKey>`、`X-Worker-Secret: <MCP_WORKER_SECRET>`、`X-Request-ID`、`Accept`，客户端其余请求头（Cookie、User-Agent 等）不透传；源站超时默认 `60s`，可经 `MCP_UPSTREAM_TIMEOUT_MS` 调整（限 5s~300s，非法回退 60s；超长流式响应被 60s 截断时调大）。
- 源站 `serveStreamable` 校验 `X-Worker-Secret`（`workerSecretAuth`，常量时间比较；未配置返回 500——防御分支，启动校验后不可达）、限流、Bearer API Key（查 `key_hash`，认证忽略 `expires_at`）。

### MCP-3 绑定页 /auth

验收标准：
- `GET /auth` 返回 HTML 表单（粘贴 API Key）。
- `POST /auth/bind`：按 IP 限流 10/min（超限 429 HTML）；缺 key 返回 400 HTML。
- 用该 key 调 `GET {BACKEND_URL}/internal/mcp/diary?limit=1`（带 `X-Worker-Secret`，10s 超时）校验：`401` → HTML「API Key 无效或已过期」401；非 2xx → 502；网络失败 → 502。
- 校验成功：`token = crypto.randomUUID()` 写入 KV `mcp_token:<token>` → `apiKey`，TTL `30 天`；HTML 展示 `{origin}/mcp?t=<token>` 与 MCP 配置示例。

### MCP-4 工具调用 store_memory / query_memories

验收标准：
- `initialize`：接受 `2024-11-05` 及以后版本并回显；空版本按默认 `2024-11-05`；低于该版本 → `-32602 unsupported protocol version`。结果含 `capabilities.tools={}`、`capabilities.prompts={}`、`serverInfo={name:"memory-mcp",version:"1.0.0"}`。
- `notifications/initialized` 返回 202 Accepted（无响应体）；无 `id` 的通知类请求（notifications/*、tools/call、prompts/*、未知方法）返回 202；**例外**：`initialize` 与 `tools/list` 无 `id` 时仍返回 200 + result。
- `tools/list` 返回两项：`store_memory`、`query_memories`（schema 见 5.4 节）。
- `tools/call` `store_memory`：校验 `record_time`（RFC3339，必填）、`title`（trim 后 1–50 runes）、`content`（trim 后 1–10000 runes）；失败 `-32602`；DB 失败 `-32603 保存失败，请重试`；成功文本「已保存。ID: <memoryID>」。
- `tools/call` `query_memories`：默认近 90 天、limit 默认 500；负值/0 归一为 500；上限 1000；日期跨度 >180 天 → `-32602`；成功返回格式化文本 + `truncated` 布尔；超预算时文本末尾附「[注意] 结果数量过多…」。工具参数 `limit≤0` 归一为默认 500；REST 端 `limit=0` 合法（返回空结果）。
- 响应超 64KB：JSON-RPC 直接返回 `-32603 response too large`；REST 端走预算截断并返回 `truncated`，两种策略并存。
- `prompts/list` 返回 `memory-sync` 与 `memory-digest`（`arguments: []`）；`prompts/get` 返回对应 description 与 user 文本（常量 `memorySyncPrompt`/`memoryDigestPrompt`）；未知 name → `-32602 prompt not found`。
- 未知 method 且有 id → `-32601 method not found`；仅 `tools/call`/REST 会访问 DB。

### MCP-5 MCP REST 三件套

验收标准：
- `GET /internal/mcp/diary` 与 `GET /internal/mcp/memories`：先限流（`KeyRateLimiter 30/min`：分桶键取自 Authorization 头原文——无 Bearer 前缀时按 `anon:<IP>`，有 Bearer 前缀时按 `sha256(Bearer 原文)`；**分桶键未验证**，随机 Bearer 每请求新桶可绕过本层，防线与已接受风险见 `ARCHITECTURE-INVARIANTS.md` §9），再 Bearer API Key 认证（失败 401），再解析日期与 limit。
- `GET /diary` 按日聚合输出 `[{recordDate, entryCount, entries:[{time,location,content}]}]` + `truncated`。
- `GET /memories` 合并 memories 与 diary_entries，按时间倒序，输出 `[{created_at,title,content,location}]` + `truncated`。
- `POST /memories`：Bearer + 限流；body ≤64KB（超出 HTTP 413 `code=4130`）；校验 `record_time`、`title`、`content`；失败 400 `code=4000` + 中文 message；成功 `code=0000` + `{"memory_id": "..."}`，并在同一事务 `CreateMemory` + `UpsertDiary`，成功后尽力失效 MCP 查询缓存。
- 限流超限 → HTTP 429 `code=4290` + `biz_code=RATE_LIMITED` + `too many requests`（在鉴权之前判断）。
- MCP 工具调用与 REST 写入均不消耗 AI 对话配额；写入频率由 `30/min` KeyRateLimiter 约束。

### MCP-6 开源版直连 `/mcp/*`

验收标准：
- `DEPLOYMENT_MODE=open` 时后端注册 `POST /mcp`、`GET /mcp/diary`、`POST /mcp/memories`、`GET /mcp/memories`，外层套 `60/min/IP` 限流（`IPRateLimiter`），内层仍为 `30/min` KeyRateLimiter + API Key 认证，形成双层限流。
- `/internal/mcp/*` 仅在 SaaS 模式注册；该模式仅挂载公开 `/mcp/*`。
- `/mcp/key*` 管理端点仍要求登录 session（`OpenAuthMiddleware` 同时支持 session 与种子 `OPEN_API_KEY`，但 `apikey:` 身份被 `rejectAPIKeyIdentity` 拒绝）。

### MCP-7 api-worker 私有化路由（`api.pathmemos.com`）

验收标准：
- 注册：`POST /worker/register` body `{url, apiKey}`；`sessionId`（Bearer）非空为前置（空 → 401 `4010`）；POST/DELETE 按 `CF-Connecting-IP` 限流 10/min（超出 429 `4290`）。
- URL 必须 `https://` 且为独立域名/根路径（带子路径、query、hash → 400 `4001`）；`apiKey` 长度 <16 → 400 `4000`。
- 目标主机过 SSRF 黑名单：拒绝 localhost/`.local`/`.internal` 后缀、私网/链路本地/保留 IPv4 段与 IPv6 字面量（`api-worker/src/lib.ts` `isDisallowedBackendHost`），命中 → 400 `4001 backend host not allowed`。
- 握手探测：`GET {url}/system/config` 必须 `data.mode=="open"`（否则 400 `4001`）；`GET {url}/vip` 不能返回 401（401 → 400 `4003`）；探测网络失败或响应非 JSON → 400 `4002`；探测超时各 10s。
- 通过后 KV 写 `backend_api_key:<sha256hex(apiKey)>` → `{"type":"private","url":"..."}`（不存明文 key）；KV 写/删失败返回 502 `code=5001 storage temporarily unavailable, retry`（不裸抛 500）。
- 注销：`DELETE /worker/register` body `{apiKey}` → 删除对应 KV，返回 `{"code":"0000","message":"ok","deleted":true}`；body 缺失或 `apiKey` 为空/非字符串时不删除任何 KV，返回 `{"code":"0000","message":"no apiKey provided","deleted":false}`，调用方据 `deleted` 可区分（api-worker/src/index.ts）。
- `/worker/register` 以 session 非空为前置校验，实际可信性由「持有 API Key」与握手探测共同保证。
- KV 写入采用最终一致：注册/注销后约 60s 内边缘仍可能命中旧路由。
- 路由：非公开回调路径且带 `X-Private-Api-Key`：`ENABLE_PRIVATE_BACKEND=="false"` → 403 `4031 private backend routing is disabled`；KV 命中且 `type=="private"` → 转发到该 url；KV 未命中或 `type!="private"` → 403 `4031 private backend not registered for this api key`（不静默回退 SaaS）；KV 读取抛异常 → 503 `5030`。
- 公开回调按**前缀匹配**（`pathname.startsWith`）强制回 SaaS：`/wx/callback`、`/api/prod/payment/virtualPayNotify` 均忽略 `X-Private-Api-Key`；因此 `/wx/callbackX`、`/wx/callback/任意后缀` 等前缀相同的路径同样回 SaaS（已知偏宽边界，保持现状）。
- 转发：仅当目标为 `SAAS_BACKEND_URL` 时附加 `X-Worker-Secret`；目标为私有后端时**删除**客户端传入的 `X-Worker-Secret`（SaaS 密钥只发官方后端）；`X-Forwarded-Host` 始终以真实入站主机覆盖（客户端伪造值不透传）；其余请求头仍原样透传（含 session Bearer，私有后端须自行校验）；GET/HEAD 转发时剥离 body；超时默认 60s（`/file/upload`、`/file/download` 为 360s；`/ai/chat` 不设超时）；不可达 → 502 `5020`；响应头加 `X-Forwarded-By: papafeiji-api-worker`；转发日志对私有目标只记录 `shortHash(targetUrl)`，不落明文域名。

### MCP-8 Worker GET 缓存与记忆写版本

验收标准：
- 仅 GET `/mcp/diary`、`/mcp/memories` 可缓存；缓存键为「剔除 `t` 参数后的 URL + `_<sha256前16位(apiKey)>_<memVersion>`」。
- TTL：无日期参数或查询范围含最近 3 天 → 1 分钟；纯历史 → 5 分钟（小程序写入不经 Worker，边缘缓存无法跨路径失效，故收窄陈旧窗口）。日期以 `new Date(s + "T00:00:00Z")` 按 UTC 解析，源站按上海时区，跨时区边界时两侧 TTL 档位可能相差一档。
- `store_memory` 的 `tools/call` 或 `POST /mcp/memories` 返回 2xx 后，写新的 `memVersion`（UUID，Cache-Control max-age=3600）到 `caches.default`，key 为 `{BACKEND_URL}/__memver/<sha256前16位(apiKey)>`。
- 命中缓存响应加 `X-Worker-Cache: HIT`；所有响应加 `X-Request-ID`（缺省由 Worker 生成 UUID）。
- 缓存对象大小：源站结果 >64KB 时不写源站 Redis 缓存（源站侧判断），Worker 侧缓存由 `cacheable && status==200` 决定。
- `tools/call` body 非 JSON/解析失败时**保守按写处理**（bump 写版本，宁多失效不脏读）；KV 写失败不裸抛：绑定页 `/auth/bind` 返回 502 可重试提示（mcp-worker）。

## 4. 数据模型

### 4.1 表（`backend/migrations/000001_baseline.up.sql`）

| 表 | 关键字段 / 约束 | 说明 |
|----|----------------|------|
| `api_keys` | `id` 主键；`user_id` NOT NULL 且 `UNIQUE`、FK `users ON DELETE CASCADE`；`key_hash` NOT NULL 且 `UNIQUE INDEX idx_api_keys_key_hash`；`api_key` NOT NULL（明文）；`expires_at` NOT NULL；`created_at`；索引 `idx_api_keys_expires_at` | 一用户至多一 key；明文供展示，hash 供认证 |
| `memories` | `id` 主键；`user_id` FK CASCADE；`record_time timestamptz NOT NULL`；`record_date date NOT NULL`；`title text NOT NULL`；`content text NOT NULL`；`created_at`；覆盖索引 `idx_memories_query_covering(user_id, record_date DESC, record_time DESC) INCLUDE (title, content)` | 手动保存的记忆；写入同时 upsert 当日 diary 行 |
| `diaries` | `(user_id, record_date)` 唯一（`UpsertDiary` ON CONFLICT DO NOTHING） | 记忆写入时确保当日 diary 行存在 |
| `diary_entries` / `users` | — | MCP 查询日记条目与昵称 |

### 4.2 Cloudflare KV / Cache

| 位置 | key | value | TTL |
|------|-----|-------|-----|
| mcp-worker KV | `mcp_token:<token>` | API Key 明文 | 30 天 |
| mcp-worker Cache API | `{BACKEND_URL}/__keycheck/<sha256前16位>` | API Key 边缘校验正缓存 | 60s |
| api-worker KV | `backend_api_key:<sha256hex(apiKey)>` | `{"type":"private","url":"https://..."}` | 无（持久，直到 DELETE） |
| mcp-worker Cache API | `{BACKEND_URL}/__memver/<sha256前16位>` | 记忆写版本 UUID | 3600s |
| mcp-worker Cache API | 查询 URL + 身份后缀（见 MCP-8） | 上游 JSON 响应 | 60s（含近 3 天） / 300s（纯历史） |

### 4.3 源站 Redis 缓存

| 键 | TTL | 说明 |
|----|-----|------|
| `mcp:diary:<userID>:<start>:<end>:<limit>` | 30min（`mcpCacheTTL`） | GET diary 结果（JSON 字节），>64KB 不写 |
| `mcp:mem:<userID>:<start>:<end>:<limit>` | 30min | GET memories 结果（JSON 字节），>64KB 不写 |

失效：`InvalidateUserCache` 以 `SCAN`（count 100）删除 `mcp:diary:<userID>:*` 与 `mcp:mem:<userID>:*`；由 `createMemoryForUser` 成功后尽力调用（Redis 不可用由 TTL 兜底）。

## 5. API 契约

统一响应结构 `{code,biz_code?,message,data?,request_id}`（`middleware/response.go`）；错误码见 `backend/pkg/errors/codes.go`（相关：`0000`、`4000`、`4010`、`4030`、`5001`、`RATE_LIMITED`、`SESSION_INVALID`）。

API Key 数据边界：Key 只能访问其属主用户本身有权限访问的数据（归属 = `api_keys.user_id`，规则见 03-api §5），默认不跨用户、不跨家庭；不建立 OAuth/Scope 体系。

### 5.1 源站端点登记

| 方法 | 路径 | 注册位置 | 鉴权 | 说明 |
|------|------|----------|------|------|
| POST | `/mcp/key` | session 路由组（`mcp/handler.go` `Register`） | Session | 创建（get-or-create）API Key；`apikey:` 身份 403 |
| GET | `/mcp/key` | 同上 | Session | 查询；无 key `{"hasKey":false}` |
| DELETE | `/mcp/key` | 同上 | Session | 删除当前用户 key |
| POST | `/mcp/key/rotate` | 同上 | Session | 原子换发 |
| POST | `/internal/mcp/rpc` | `RegisterInternal`（SaaS） | `X-Worker-Secret` + `Authorization: Bearer <apiKey>` | MCP Streamable HTTP |
| GET | `/internal/mcp/diary` | 同上 | 同上 | 日记查询 REST |
| POST | `/internal/mcp/memories` | 同上 | 同上 | 创建记忆 REST |
| GET | `/internal/mcp/memories` | 同上 | 同上 | 记忆查询 REST |
| POST | `/mcp` | `RegisterPublic`（open） | API Key（Bearer） | MCP Streamable HTTP |
| GET | `/mcp/diary` | 同上 | API Key | 日记查询 REST |
| POST | `/mcp/memories` | 同上 | API Key | 创建记忆 REST |
| GET | `/mcp/memories` | 同上 | API Key | 记忆查询 REST |

> 路由挂载：`apiRouter`（`WorkerAuth(cfg.WorkerSecret, "/api/prod/payment/virtualPayNotify", "/wx/callback", "/internal/mcp")`）。注意 `/internal/mcp` 在 WorkerAuth 的 skip 前缀内，即不由 `WORKER_SECRET` 校验，而由 `workerSecretAuth` 用 `MCP_WORKER_SECRET` 保护（`main.go:207`、`mcp/rpc.go`）。

### 5.2 Worker 公网端点（mcp-worker，`mcp.pathmemos.com`）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/health` | 公开（不校验 HTTP 方法，任意方法同行为） | 回源 `/internal/mcp/diary?limit=0` + `X-Worker-Secret`，期望 401 → `{status:"ok",upstream:"reachable"}`；否则 503 `degraded` |
| GET | `/auth` | 公开 | HTML 绑定表单 |
| POST | `/auth/bind` | 公开 + 10/min/IP | 校验 API Key 并生成 KV token |
| POST | `/mcp` | Bearer 或 `?t=` | 静态方法边缘响应，其余回源 `/internal/mcp/rpc` |
| GET | `/mcp/diary` | Bearer 或 `?t=` | 回源 `/internal/mcp/diary`，含边缘缓存 |
| POST | `/mcp/memories` | Bearer 或 `?t=` | 回源 `/internal/mcp/memories` |
| GET | `/mcp/memories` | Bearer 或 `?t=` | 回源 `/internal/mcp/memories`，含边缘缓存 |
| 其他 | 任意 | — | 404 `Not Found` |

### 5.3 api-worker 端点（`api.pathmemos.com`）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/worker/register` | Bearer session 非空 + 10/min/IP | 注册私有后端；返回 `{"code":"0000","message":"ok"}` |
| DELETE | `/worker/register` | 同上 | 注销；返回 `{"code":"0000","message":"ok","deleted":true}`；缺 body / 空 `apiKey` 返回 `{"code":"0000","message":"no apiKey provided","deleted":false}` |
| 其他 | 任意 | Bearer/`X-Private-Api-Key` | 按上文路由转发；无法识别的私有 key 组合返回 4031 |

api-worker 错误码：`4000`（url/apiKey 不合法）、`4001`（URL 形状/非 open 后端）、`4002`（目标不可达）、`4003`（apiKey 被后端拒绝）、`4010`（缺 session）、`4031`（私有路由停用/未注册）、`4050`（方法不允许）、`4290`（注册限流）、`5020`（转发不可达）、`5030`（KV 读取失败）。

> 上述错误码是 **api-worker 自有词汇**，与源站 `code` 固定枚举（03-api §3.1）相互独立，勿混用或改写源站枚举。

### 5.4 MCP JSON-RPC 方法契约

| 方法 | 入参 | 结果 |
|------|------|------|
| `initialize` | `{protocolVersion}` | `{protocolVersion, capabilities:{tools:{},prompts:{}}, serverInfo:{name:"memory-mcp",version:"1.0.0"}}` |
| `notifications/initialized` | — | HTTP 202（无 body） |
| `tools/list` | — | `{tools:[store_memory, query_memories]}` |
| `tools/call` | `{name, arguments}` | 见 5.5 节 |
| `prompts/list` | — | `{prompts:[memory-sync, memory-digest]}` |
| `prompts/get` | `{name}` | `{description, messages:[{role:"user",content:{type:"text",text}}]}` |
| 未知且有 id | — | `error.code=-32601 "method not found"` |

工具 schema：

| 工具 | 参数 | 约束 |
|------|------|------|
| `store_memory` | `record_time`(string,ISO8601)、`title`(string)、`content`(string) | 三者必填；title ≤50 字、content 1–10000 字 |
| `query_memories` | `start_date`(YYYY-MM-DD)、`end_date`(YYYY-MM-DD)、`limit`(int) | 默认近 90 天；limit 默认 500、上限 1000 |

### 5.5 REST 响应体（`data` 部分）

| 端点 | 成功 data | 错误 |
|------|-----------|------|
| POST `/…/memories` | `{"memory_id":"<uuid>"}` | 400 `4000`（校验）、413 `4130`（>64KB）、401 `4010`、429 `4290`+`RATE_LIMITED`、500 `5001` |
| GET `/…/diary` | `{"data":[{"recordDate","entryCount","entries":[{"time":"YYYY年MM月DD日HH时","location","content"}]}],"truncated":bool}` | 400 `4000`（日期格式/跨度）、401、429、500 |
| GET `/…/memories` | `{"data":[{"created_at","title","content","location"}],"truncated":bool}` | 同上 |

## 6. 关键实现约束

### 6.1 阈值（改了就出问题）

| 项 | 值 | 位置 |
|----|-----|------|
| 请求/响应体上限 | 64KB（`maxMcpMessageSize = 64 << 10`） | `mcp/handler.go`、`mcp/server.go` |
| 响应字节预算 | `maxMcpMessageSize*4/5`（≈52.4KB） | `mcp/handler.go` |
| 分页默认 / 上限 | 500 / 1000（`defaultMcpPageSize` / `maxMcpEntries`） | `mcp/handler.go` |
| 默认回溯天数 | 90 天（`mcpDiaryDefaultDaysBack`） | `mcp/handler.go`、`mcp/server.go` |
| 日期跨度上限 | 180 天（`maxMcpDateRangeDays`） | 同上 |
| API Key 限流 | 30/min，maxBuckets 5000，key=sha256(apiKey) 或 `anon:<IP>` | `mcp/handler.go` `NewKeyRateLimiter` |
| open 公开 IP 限流 | 60/min | `mcp/rpc.go` `RegisterPublic` |
| 源站 Redis 缓存 TTL | 30min | `mcp/handler.go` `mcpCacheTTL` |
| API Key 永久有效期 | `9999-12-31T23:59:59Z` | `mcp/handler.go` `apiKeyNeverExpires` |
| key hash | SHA-256 hex | `mcp/handler.go` `hashKey`；`bootstrap/open.go` |
| Worker 缓存 TTL | 含近 3 天（或无日期参数）1min；纯历史 5min | `mcp-worker/src/index.ts` |
| Worker 记忆写版本 TTL | 3600s | `mcp-worker/src/index.ts` |
| 绑定 token TTL | 30 天 | `mcp-worker/src/index.ts` |
| /auth/bind 限流 | 10/min/IP，跟踪上限 1000 IP | `mcp-worker/src/index.ts` |
| api-worker 注册限流 | 10/min/IP，跟踪上限 10000 IP | `api-worker/src/index.ts` |
| Worker 上游超时 | 代理 60s；绑定校验 10s；/health 探测 5s；静态方法 key 校验回源 10s | `mcp-worker/src/index.ts` |
| api-worker 转发超时 | 默认 60s；`/file/upload`、`/file/download` 360s；`/ai/chat` 不设超时 | `api-worker/src/lib.ts` |
| serverInfo | `memory-mcp` / `1.0.0` | `mcp/server.go` |
| 最低协议版本 | `2024-11-05` | `mcp/server.go` |

> Worker 侧 `AbortSignal.timeout` 会**同时约束响应体传输**（api-worker/src/lib.ts 注释同款语义）：mcp-worker 代理的 60s 上游超时意味着超过 60s 的流式响应会被截断。当前源站 MCP 为无状态短 JSON 响应，正常不受影响；引入长流式 MCP 响应时必须像 api-worker 的 `/ai/chat` 一样对该类路径豁免超时。

### 6.2 认证 / 幂等 / 事务 / 失效

- 每次请求独立查 `api_keys.key_hash`，不缓存 key 状态，不校验 `expires_at`。
- **open 后端忽略伪造 Worker 头**（D3 规格）：open 模式（`WORKER_SECRET` 留空）时 `WorkerAuth` 因 expected 为空**整体短路**——入站 `X-Worker-Secret`/`X-Forwarded-Host` 被忽略、不产生 403；鉴权只认 session Bearer 与种子 `X-Private-Api-Key`。私有化链路中 api-worker 会删除客户端传入的 `X-Worker-Secret`并覆盖 `X-Forwarded-Host`，open 后端依本条语义亦不消费这两个头（`middleware/worker.go` `WorkerAuth` expected=="" 分支）。
- `workerSecretAuth` 用 `crypto/subtle.ConstantTimeCompare` 比较 `X-Worker-Secret`；saas 模式 `MCP_WORKER_SECRET` 为空则 `config.validate` 启动失败；源站保留空值返回 500 的防御分支（启动校验通过后不可达）。
- `createMemoryForUser` 单事务写 `memories` + `UpsertDiary`；`record_date` = `record_time` 转上海时区后的 Y/M/D，但以 UTC 零点存入（`time.Date(..., time.UTC)`）。
- 缓存失效：`InvalidateUserCache` 用 `context.WithoutCancel` 执行，避免客户端断开中断清理。
- 触发器注意：`api_keys.user_id` 有 UNIQUE，`CreateKey` 依赖冲突后回查实现幂等。

## 7. 前端接入

| 位置 | 说明 |
|------|------|
| `frontend/miniapp/miniprogram/pages/sub/Mcp/Mcp.ts` + `.wxml` | MCP 接入页：`GET /mcp/key` 初始化，`POST /mcp/key` 生成，`POST /mcp/key/rotate` 换发（带确认弹窗），复制 key/配置/绑定地址；3 个 Tab（`mcp`/`connector`/`http`），无 `authUrl` 时隐藏 connector 并回退 Tab |
| `frontend/miniapp/miniprogram/pages/User/User.ts` | `toMcp()` 跳转 `/pages/sub/Mcp/Mcp` |
| `frontend/miniapp/miniprogram/components/MemoryEdit/MemoryEdit.ts` | `goMemoryConfig()` 跳转同页，用于记忆配置引导 |
| `frontend/miniapp/miniprogram/pages/sub/BackendConfig/BackendConfig.ts` | 私有化后端设置：向 `{WORKER_BASE_URL}/worker/register` POST/DELETE（请求头仅 `Authorization`，不含 `X-Private-Api-Key`）；仅直连 Worker 的探针请求 `GET /system/config` 带 `X-Private-Api-Key`；映射 Worker 错误码文案 |
| `frontend/miniapp/miniprogram/app.json` | 页面注册于 `subpackages`（`Mcp/Mcp`） |
| `/system/config` `features.mcp` | 由 `MCP_ENABLED` 控制（默认 true），前端可用于显隐入口；后端路由本身不因该开关禁用 |

前端注意：`getBaseURL()` 返回 SaaS 站点（直连）或私有模式下的 `WORKER_BASE_URL`；MCP 页的 `apiUrl`/`memoryUrl` 优先用后端返回值，缺失时才回退本地拼接。

## 8. 运维与任务

### 8.1 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `DEPLOYMENT_MODE` | `saas` | `open` 时注册公开 `/mcp/*`；`/internal/mcp/*` 仅在 `saas` 模式注册 |
| `MCP_WORKER_SECRET` | 空 | 源站 `/internal/mcp/*` 的 `X-Worker-Secret`；SaaS 必填（空则启动失败），open 留空 |
| `MCP_PUBLIC_URL` | 空 | 对外公开入口（如 `https://mcp.pathmemos.com`）；空则回退 `APIHost`，且不返回 `authUrl` |
| `MCP_ENABLED` | `true` | 仅影响 `/system/config features.mcp` |
| `OPEN_API_KEY` | 空 | 开源版种子 key（`bootstrap/open.go`）；变更时先事务换 key 再 flush 全部 session |
| `API_HOST` | 空 | `publicBaseURL` 回退来源；**所有模式**启动必填（config.validate 无条件下校验；open 模式 `sysconfig.go` 另有补充校验） |
| `HTTP_BIND`/`HTTP_PORT` | `127.0.0.1`/`8080` | `apiBaseURL` 回退拼接（默认 scheme 恒为 https） |

### 8.2 Worker 配置与部署（`docs/mcp-worker-ops.md`）

- `mcp-worker/wrangler.toml`：name `papafeiji-mcp`，route/custom_domain `mcp.pathmemos.com`，vars `BACKEND_URL=https://pro.papafeiji.cn`，KV binding `KV`（id `a2230a743e1b408ea1aa5b0abf4f3b0a`）；`MCP_WORKER_SECRET` 用 `wrangler secret put` 注入。
- 已知取舍：api-worker KV 私有路由映射（`backend_api_key:<sha256>`）生命周期独立于源站 `api_keys`——源站 rotate/删除/注销不清理 KV；旧键映射残留无数据风险（回源 401），仅 KV 条目残留，按需经 `DELETE /worker/register` 手动清理。
- `api-worker/wrangler.toml`：name `papafeiji-api`，custom_domain `api.pathmemos.com`，vars `SAAS_BACKEND_URL=https://pro.papafeiji.cn`、`ENABLE_PRIVATE_BACKEND="true"`，KV id `05b183e932a34cc9b8b4c1ef3d55d0ea`；`WORKER_SECRET` 同样用 secret 注入。
- `api-worker/src/lib.ts`：从 `index.ts` 抽出的纯函数与共享逻辑（`CF-Connecting-IP` 解析、`/worker/register` 写操作内存限流 10 次/分/IP、跟踪 IP 上限 10000）；有存量 vitest 单测（2026-09 验收策略下为资产、非门禁）。
- 部署：`cd mcp-worker && npx wrangler deploy`、`cd api-worker && npx wrangler deploy`；验证 `curl https://mcp.pathmemos.com/health`。
- 回滚：`npx wrangler deployments list` + `npx wrangler rollback <version-id>`。
- 平台层限流需在 Cloudflare Dashboard 手动配置（`/mcp*` 60/min/IP、`/auth*` 10/min/IP），不在 wrangler.toml。

### 8.3 任务

- 无 MCP 专属后台任务（stateless）；`Handler.Stop()` 释放限流器（当前实现无后台 goroutine，空操作）与公开 IP 限流器。
- 源站缓存一致性依赖写后主动失效 + 30min TTL；Worker 缓存依赖记忆写版本 + 1/5min TTL（MCP-8）。
- 数据清理：`api_keys` 仅由用户显式删除/换发（`DeleteExpiredAPIKeys` SQL 存在但全仓库无调用，key 永不过期）。
- KV 失败模式：mcp-worker `/auth/bind` 的 `KV.put` 与 api-worker 注册/注销的 `KV.put`/`KV.delete` 失败均被捕获并返回 502 `code=5001`（可重试，不裸抛 500）；仅 api-worker 的 KV **读**有 503 `5030` 兜底。
