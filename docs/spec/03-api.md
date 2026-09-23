# PP-03 接口契约（L3）

> 层级：L3 接口契约｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0001（MCP 剥离到 Cloudflare Worker）、ADR-0004（app/sse 双容器）
> 说明：本文件按当前代码实现整理，描述当前接口契约。章节遵循 docs/spec-standards.md 第二节 L3 必含小节（认证与请求头 / 统一响应结构 / 错误码词汇表 / 分页 / 状态码映射 / 超时与限流 / 数据归属校验 / 幂等约定 / 按域分组全登记 + 通用契约模板）。
>
> **实现锚点**（仅定位用途，标注当前代码位置）：
> - 路由挂载与中间件顺序：backend/cmd/server/main.go
> - 各域注册方法：backend/internal/*/handler.go 的 Register / RegisterPublic / RegisterProtected；backend/internal/mcp/rpc.go 的 RegisterInternal / RegisterPublic
> - 中间件：backend/internal/middleware/{session,open_auth,worker,response,ratelimit,keylimit,body,accesslog}.go
> - 错误码常量：backend/pkg/errors/codes.go；域内哨兵错误：backend/internal/{auth,family,file,vip,diary}/*.go
> - 配置/环境变量：backend/internal/config/config.go

## 1. 认证与请求头

### 1.1 请求头一览

| 头 | 值形态 | 由谁校验 | 适用路由 | 源文件 |
|----|--------|---------|---------|--------|
| Authorization | Bearer + sessionId | SessionMiddleware（SaaS）或 OpenAuthMiddleware（open） | 所有受保护路由、SSE /ai/chat、MCP 数据端点（此处 Bearer 为 API Key） | middleware/session.go、middleware/open_auth.go、mcp/server.go |
| X-Private-Api-Key | 明文 OPEN_API_KEY | OpenAuthMiddleware，且**仅当完全无 Authorization 头时** | open 模式受保护路由（Worker 转发 MCP/curl 通路） | middleware/open_auth.go |
| X-Worker-Secret | 明文 WORKER_SECRET | WorkerAuth（HTTP/SSE 顶层） | 经 Worker 中转的 HTTP 流量 | middleware/worker.go |
| X-Worker-Secret | 明文 MCP_WORKER_SECRET | mcp.workerSecretAuth | SaaS 模式 /internal/mcp/* | mcp/rpc.go |
| X-Forwarded-Host | 主机名 | WorkerAuth 的触发条件（存在才校验 X-Worker-Secret） | 同上 | middleware/worker.go |
| X-Request-ID | 字符串 | LoggerMiddleware 优先复用并写入响应 request_id | 全部 | middleware/response.go、middleware/logger.go |
| Content-Type | application/json 或 multipart/form-data | 各 handler | 视路由 | 各 handler |

### 1.2 三套鉴权机制

**A. 会话鉴权（SaaS 默认，open 模式带 Authorization 时）**

- Authorization: Bearer sessionId → Redis session:<id> → 注入 user_id + session_id 到 context（middleware/session.go:279-301）。
- 会话 TTL：滑动过期 30 天（sessionExpiry）、绝对过期 90 天（sessionAbsoluteExpiry）；每次成功读取刷新滑动 TTL（session.go:23-29,160-178）。
- 缺失 session → 401 + code="4010" + biz_code="SESSION_INVALID" + message="missing session"；不存在/过期 → 401 + code="4010" + biz_code="SESSION_INVALID" + "invalid or expired session"；Redis 错误 → 500 + code="5001" + "failed to check session"。
- SaaS 模式的受保护分组使用 SessionMiddleware；open 模式使用 OpenAuthMiddleware（main.go:240-245,318-322）。

**B. 开源版 API Key 鉴权（仅 open 模式，OpenAuthMiddleware）**

- 两条**互斥**通路（middleware/open_auth.go:21-26）：
  1. 有 Authorization 头 → 只做 session 鉴权；session 不存在直接 401，**不**回落到 API Key。
  2. 无 Authorization 头且带 X-Private-Api-Key → 只接受与种子 OPEN_API_KEY 的 SHA-256 相等的 key（lookupUserByAPIKey，open_auth.go:89-105），成功后注入 sessionID = "apikey:<userID>"。
- 用户经 /mcp/key 创建的 MCP Key **不能**升级为完整 REST 凭证（仅种子 OPEN_API_KEY 被接受）。
- 连续失败达 3 次才 sleep 500ms 并清零计数（暴力穷举节流，open_auth.go:31-32,77-82）。
- 均失败 → 401 + code="4010" + biz_code="SESSION_INVALID" + "unauthorized"。

**C. Worker 中转一致性校验（不是源站防线）**

- WorkerAuth(expected, skipPrefixes...)（middleware/worker.go:16-46）：
  - expected（WORKER_SECRET）为空 → 仅 `DEPLOYMENT_MODE=open` 允许（开源直连语义）；**saas 模式下 WORKER_SECRET 为启动必填**，空则 `config.validate` 失败、进程拒绝启动；expected 为空时不校验也不消费 `X-Worker-Secret`/`X-Forwarded-Host`（入站伪造这两个头无效果）；
  - 路径命中 skip 前缀 → 直接放行；
  - **无 X-Forwarded-Host** → 直接放行（允许 SaaS 小程序直连 pro.papafeiji.cn）；
  - 有 X-Forwarded-Host → 校验 X-Worker-Secret，缺失/不匹配返回 403（纯文本 forbidden，非统一 envelope）。
- HTTP apiRouter 的 skip 前缀：/api/prod/payment/virtualPayNotify、/wx/callback、/internal/mcp（main.go:207）；health 端点不挂载 WorkerAuth；SSE 路由的 WorkerAuth 无 skip 前缀，但 health 注册在分组之外（main.go:309-317）。
- SaaS /internal/mcp/* 由 mcp.workerSecretAuth 独立校验 MCP_WORKER_SECRET：saas 模式启动即强制非空（`config.validate`），运行时未配置 → 500 + server misconfigured；不匹配 → 403 + forbidden（mcp/rpc.go）。

### 1.3 部署模式差异（DEPLOYMENT_MODE=saas|open，默认 saas）

| 维度 | saas（默认） | open |
|------|----------------|--------|
| 受保护分组中间件 | SessionMiddleware | OpenAuthMiddleware |
| MCP 协议端点 | 仅 /internal/mcp/*（X-Worker-Secret，RegisterInternal） | 仅公开 /mcp/*（RegisterPublic，IP 限流 60/min） |
| /mcp/key* 管理 | 会话分组内 | 会话分组内；以 apikey: 身份调用会被 403 拒绝（mcp/handler.go:169-175） |
| 启动 migration / seed | 由 deploy.sh 控制 | 启动时 migration.Run + bootstrap.SeedOpenBackend（main.go:112,135） |
| /system/config features | payment/wxmp 按 env 计算 | payment=false、wxmp=false |

## 2. 统一响应结构

源文件：backend/internal/middleware/response.go（responseEnvelope，第 15-24 行）。

字段定义：

| 字段 | JSON 键 | 成功 | 失败（JSONError） | 说明 |
|------|---------|------|--------------------|------|
| Code | code | "0000" | 传入的 code | **固定枚举**（§3.1：0000/4000/4010/4030/4040/4090/4130/4290/5001）；语义标识符不得作为 code |
| BizCode | biz_code | 不出现 | 可选出现 | **语义码唯一载体**（如 SESSION_INVALID、RATE_LIMITED、NOT_VIP）；同一 biz_code 全端点同一 HTTP 状态 |
| Message | message | "ok" | 具体文案 | |
| Data | data | 业务数据 | 不出现（omitempty） | 空 map 会输出，nil 省略 |
| Extra | extra | 不出现 | 不出现 | 仅 GET /diary/details（经 JSONWithExtraAndCount）下发非空 extra |
| Count | count | 不出现 | 不出现 | JSONWithPagination（GET /diary/info）与 JSONWithExtraAndCount（GET /diary/details）使用 |
| NextCursor | nextCursor | 不出现 | 不出现 | 同上 |
| RequestID | request_id | 始终出现 | 始终出现 | 优先复用请求头 X-Request-ID，否则 middleware.GetReqID，最后生成 UUID（response.go:50-62） |

六个构造器：

| 函数 | 字段组合 | 使用者 |
|------|---------|--------|
| JSON(w,r,status,data) | code=0000, message=ok, data | 绝大多数成功响应 |
| JSONWithPagination(w,r,status,data,nextCursor,count) | + count、nextCursor | GET /diary/info |
| JSONWithExtra(w,r,status,data,extra) | + extra | JSON() 的底层实现（extra=nil） |
| JSONWithExtraAndCount(w,r,status,data,extra,count) | + extra、count | GET /diary/details |
| JSONError(w,r,status,code,message,bizCode...) | code/message；biz_code 可选 | 所有错误 |
| JSONBizError(w,r,bizCode,message) | code/HTTP 状态由 biz_code 唯一推导；biz_code | 仅持有 biz_code 的错误分支 |

### 2.1 非 envelope 响应（必须单独处理）

| 端点/场景 | 响应体 | 源文件 |
|-----------|--------|--------|
| 微信支付回调 GET/POST /api/prod/payment/virtualPayNotify | 固定 {"ErrCode":0,"ErrMsg":"success"}（200）；瞬时故障 {"ErrCode":-1,"ErrMsg":"internal error"}（500） | payment/handler.go:372,288,330 |
| 微信公众号回调 GET /wx/callback | 纯文本 echostr（200）或 fail（403） | wxmp/handler.go:94-114 |
| 微信公众号回调 POST /wx/callback | XML 回复或纯文本 success（200） | wxmp/handler.go:203-248,444-454 |
| SSE POST /ai/chat | text/event-stream，见 2.2 | ai/handler.go、ai/upstream.go |
| MCP 数据端点 /mcp/*、/internal/mcp/* | JSON-RPC 端点 `/mcp`、`/internal/mcp/rpc` 错误为纯文本（`mcp/server.go`）；REST 端点 `diary`/`memories` 返回统一 envelope（401/400/413/429/500 走 `middleware.JSON*`，`mcp/handler.go`） | mcp/server.go、mcp/handler.go |
| POST /file/upload 超时 | http.TimeoutHandler 返回 {"code":"5001","message":"upload timeout"}（非统一 envelope 包：是 message 不是 msg，且无 request_id） | main.go:273 |
| WorkerAuth / workerSecretAuth 拒绝 | 纯文本 forbidden（403） | middleware/worker.go:39-41、mcp/rpc.go:52-55 |

### 2.2 SSE 事件流（POST /ai/chat）

响应头：Content-Type: text/event-stream、Cache-Control: no-cache、Connection: keep-alive；HTTP 状态 200，先 WriteHeader(200) 再 Flush（ai/handler.go:89-93）。

| 事件 | 载荷 | 源文件 |
|------|------|--------|
| 数据块 | 每个 chunk 按行输出 data: <line> + 换行，末尾空行 | ai/upstream.go:244-256 |
| 心跳 | 每 25s 一条 SSE 注释行 `: heartbeat`（无 event/data 语义，客户端忽略） | ai/handler.go |
| 结束 | event: done + data 空行 | ai/upstream.go:258-264 |
| 错误 | event: error，data 为 {"code":"<枚举>","biz_code":"<语义码?>","bizCode":"<语义码?>","message":"<msg>"}（`bizCode` 为与 `biz_code` 同时下发、供旧客户端读取的兼容字段） | ai/upstream.go:266-282 |

错误事件取值：配额超限 code=4290、biz_code=AI_DAILY_QUOTA_EXCEEDED、message="daily ai chat quota exceeded"；在途冲突（同 request_id 处理中，等待超时或在途失败，**不回落生成**）code=4290、biz_code=OPERATION_IN_PROGRESS、message="ai turn in progress, retry later"；超时 code=5001、message="timeout"；上游错误 code=5001、message="upstream error"（ai/handler.go）。

## 3. 错误码词汇表

**词汇表规范源：本文 §3.1/§3.2（ADR-0008）；`backend/pkg/errors/codes.go` 的常量与下表一一对应（常量列为该文件中的 Go 常量名）。**

### 3.1 code 固定枚举（终态，ADR-0008）

| 常量 | 值 | HTTP | 语义 |
|------|----|------|------|
| CodeSuccess | 0000 | 200 | 成功（所有成功响应写死） |
| CodeBadRequest | 4000 | 400 | 请求参数非法 |
| CodeUnauthorized | 4010 | 401 | 未认证 |
| CodeForbidden | 4030 | 403 | 越权 / 能力禁用 |
| CodeNotFound | 4040 | 404 | 资源不存在 |
| CodeConflict | 4090 | 409 | 状态冲突（已领取 / 已占用） |
| CodeRequestEntityTooLarge | 4130 | 413 | JSON body 超上限 |
| CodeTooManyRequests | 4290 | 429 | 限流 / 配额 / 操作进行中 |
| CodeInternalError | 5001 | 500 | 内部错误 |

规则：`code` 只允许本表枚举；语义标识符一律放 `biz_code`（§3.2）；新增枚举需先改 `backend/pkg/errors/codes.go` 并回写本表。

### 3.2 biz_code 语义码词汇表（终态）

统一下发形态：`code = 对应枚举` + `biz_code = 语义码`；HTTP 状态由 `errors.HTTPStatus(bizCode)` 唯一决定。

| biz_code | Go 常量 | HTTP | code | 说明 / 发出位置 |
|----------|---------|------|------|-----------------|
| SESSION_INVALID | `BizSessionInvalid` | 401 | 4010 | Session/OpenAuth 中间件：缺失或失效会话（仅作为 biz_code 下发） |
| RATE_LIMITED | `BizRateLimited` | 429 | 4290 | IP/Key 限流（ratelimit.go、keylimit.go）、逆地理/位置配额、AI IP/并发限流（AI 日配额单独下发 `AI_DAILY_QUOTA_EXCEEDED`） |
| NOT_VIP | `BizNotVip` | 403 | 4030 | 非 VIP 调用 VIP 能力（autorecord、diary auto） |
| PHONE_ALREADY_BOUND | `BizPhoneAlreadyBound` | 409 | 4090 | 手机号已被绑定（auth） |
| FAMILY_NOT_FOUND | `BizFamilyNotFound` | 404 | 4040 | 邀请链接目标家庭不存在 |
| TARGET_IS_PERSONAL_FAMILY | `BizTargetIsPersonalFamily` | 403 | 4030 | 加入目标是个人家庭 |
| OWNER_CANNOT_LEAVE_FAMILY | `BizOwnerCannotLeaveFamily` | 403 | 4030 | owner 退出家庭 |
| CANNOT_REMOVE_SELF / CANNOT_REMOVE_OWNER | `BizCannotRemoveSelf` / `BizCannotRemoveOwner` | 403 | 4030 | 移除成员校验 |
| ALREADY_IN_FAMILY / FAMILY_FULL | `BizAlreadyInFamily` / `BizFamilyFull` | 409 | 4090 | 创建 / 加入家庭 |
| FREE_VIP_ALREADY_CLAIMED / TRIAL_VIP_ALREADY_CLAIMED | `BizFreeVipAlreadyClaimed` / `BizTrialVipAlreadyClaimed` | 409 | 4090 | VIP 领取防重 |
| ORDER_NOT_FOUND | `BizOrderNotFound` | 404 | 4040 | 订单不存在 / 非本人 |
| OPERATION_IN_PROGRESS | `BizOperationInProgress` | 429 | 4290 | 锁冲突 / 并发注销 / AI 在途 |
| INVALID_FILE_TYPE / FILE_SIZE_EXCEEDED | `BizInvalidFileType` / `BizFileSizeExceeded` | 400 | 4000 | 上传类型 / 大小校验 |
| USER_IMAGE_STORAGE_LIMIT_EXCEEDED | `BizUserImageStorageLimitExceeded` | 400 | 4000 | 图片存储配额 |
| TEXT_TOO_LONG / INVALID_COLOR_FORMAT / INVALID_COORDINATES | `BizTextTooLong` / `BizInvalidColorFormat` / `BizInvalidCoordinates` | 400 | 4000 | 日记条目校验、AI 消息过长（`message too long`，见 02f；仅作为 biz_code 下发） |
| AI_DAILY_QUOTA_EXCEEDED | `BizAIDailyQuotaExceeded` | 429（SSE 体内） | 4290 | AI 日配额（SSE error 事件） |

### 3.3 域内 Go 哨兵错误（不是 code，映射为 message 或 Code*）

| 包 | 哨兵错误 | 出参 | 源文件 |
|----|---------|------|--------|
| auth | ErrWechatInvalidCode、ErrWechatService | 登录/绑手机号错误分支 | auth/errors.go |
| family | ErrAlreadyInFamily、ErrFamilyNotFound、ErrTargetIsPersonalFamily、ErrAlreadyInTargetFamily、ErrNotInNormalFamily、ErrOwnerCannotLeave、ErrFamilyFull、ErrCannotRemoveSelf、ErrCannotRemoveOwner、ErrNotOwner、ErrTargetNotInFamily、ErrCannotDissolvePersonal、ErrOperationInProgress | 家庭/邀请 handler switch | family/errors.go |
| file | ErrNotFileOwner、ErrSystemFileDelete、ErrFileNotFound、ErrFileInUse | 删除文件错误分支 | file/errors.go |
| vip | ErrFreeVIPAlreadyClaimed、ErrTrialVIPAlreadyClaimed、ErrInvalidVIP | 领取 VIP 错误分支 | vip/errors.go |
| diary | ErrEntryNotFound、ErrFamilyMismatch、ErrMemberNotFound、ErrPermissionDenied、ErrCoverUpdateInProgress、ErrDailyReverseQuotaExceeded、errTextTooLong、errInvalidColor、errInvalidCoords、errTooManyImages 等 | 日记 handler | diary/service.go、diary/handler.go |
| ai | ErrAIDailyQuotaExceeded、errAIChatTimeout、errAITurnInProgress | SSE 错误分支（errAITurnInProgress → SSE error 事件 code=4290 + biz_code=OPERATION_IN_PROGRESS，message `ai turn in progress, retry later`） | ai/service.go |

### 3.4 状态码映射

errors.HTTPStatus(bizCode)：

| HTTP | 命中业务码 |
|------|-----------|
| 404 | FAMILY_NOT_FOUND、ORDER_NOT_FOUND |
| 401 | SESSION_INVALID |
| 403 | TARGET_IS_PERSONAL_FAMILY、OWNER_CANNOT_LEAVE_FAMILY、CANNOT_REMOVE_SELF、CANNOT_REMOVE_OWNER、NOT_VIP |
| 409 | PHONE_ALREADY_BOUND、ALREADY_IN_FAMILY、FAMILY_FULL、FREE_VIP_ALREADY_CLAIMED、TRIAL_VIP_ALREADY_CLAIMED |
| 429 | OPERATION_IN_PROGRESS、RATE_LIMITED、AI_DAILY_QUOTA_EXCEEDED |
| 400 | 其余（default） |

> 约束：所有 handler 统一经 `errors.HTTPStatus(bizCode)` 取 HTTP 状态；同一 biz_code 全端点同一 HTTP 状态（`errors.HTTPStatus` 唯一映射）。

## 4. 分页

| 端点 | 方式 | 参数 | 响应字段 | 源文件 |
|------|------|------|---------|--------|
| GET /diary/info | 游标（日期） | cursorDate（默认 9999-12-31，格式 YYYY-MM-DD，非法 400）、size（默认 10；<1 重置为 10，>20 夹取至 20） | data（卡片数组）、count=len(cards)、nextCursor | diary/handler.go:63-92 |
| GET /diary/details | 页码 | page（默认 1，小于 1 归 1）、size（默认 20；<1 重置为 20，>100 夹取至 100）、memberUserId 可选 | data + extra + count（服务层给出分页元信息） | diary/handler.go:209-256 |
| POST /diary/info/dates | 批量 | body dates[]，最多 31 个 | data 为卡片数组，无分页 | diary/handler.go:94-137 |
| MCP GET /mcp/diary、GET /mcp/memories | limit 条数 | limit 默认 500，最大 1000（limit=0 合法返回空）；日期默认最近 90 天，跨度上限 180 天 | {"data":...,"truncated":bool} | mcp/handler.go:398-410,412-443 |

## 5. 数据归属校验约定

- 受保护 handler 的用户身份**只从 context 取**（middleware.UserID(ctx)），不从 URL/body 取用户 ID（middleware/session.go:31-36）。
- 数据归属在 handler/service 层校验：
  - 文件：files.created_by 必须等于当前用户，或双方 current_family_id 相同（file/handler.go:465-543）；系统文件按 metadata.family_id 与用户 current_family_id 比对。
  - 日记：虚拟 ID 形如 family:<familyId>:date:<YYYY-MM-DD>，familyId 必须等于当前用户 current_family_id，否则 403（diary/service.go:130-136,505-511）。
  - 支付订单：orders.user_id 必须等于当前用户，否则按 ORDER_NOT_FOUND 返回（payment/handler.go:144-147,183-186）。
- MCP/API Key 端点：Bearer key 的 SHA-256 命中 api_keys.key_hash，以该行 user_id 作为数据归属；expires_at 仅存储、不作为有效性条件（mcp/server.go:70-82、mcp/handler.go:42,490-493）。
- Authorization 与会话不匹配时不会静默降级为默认用户（open 模式 session 失效直接 401，open_auth.go:47-55）。

## 6. 超时与限流

### 6.1 服务器超时（`backend/cmd/server/main.go` `httpServer`/`sseServer`）

| Server | 监听默认 | ReadTimeout | ReadHeaderTimeout | WriteTimeout | IdleTimeout |
|--------|---------|-------------|-------------------|--------------|-------------|
| HTTP（app） | 127.0.0.1:8080 | 10 分钟 | 10 秒 | 310 秒 | 60 秒 |
| SSE（sse） | 127.0.0.1:8081 | 0（不限制，防掐断 SSE） | 10 秒 | 0 | 60 秒 |

- 优雅关闭：SIGINT/SIGTERM → shuttingDown=true（`/health/ready` 返回 503）→ 停止后台任务 → 30s 超时的 Shutdown。
- POST /file/upload 额外包 http.TimeoutHandler(5*time.Minute)，超时体见 2.1（main.go:273）。
- AI 流超时 `AIStreamTimeout` = 180s（`ai/handler.go`）；上游 HTTP 客户端超时 5 分钟（`config.SSEHTTPClient()`）、通用客户端 30 秒（`config.HTTPClient()`）。

### 6.2 body 大小上限（middleware.ReadJSONBody）

| 端点 | 上限 | 源文件 |
|------|------|--------|
| 用户设置类（/user/*） | 8 KB | user/handler.go:30 |
| VIP 领取 | 8 KB | vip/handler.go:16 |
| 登录/绑手机号/绑定邀请人/注销/支付 request/cancel/auto-record config/push | 4 KB | 各 handler |
| 日记条目/记忆/封面、家庭 join、邀请 qrcode | 64 KB | diary/handler.go、family/handler.go、invite/handler.go |
| AI 对话 POST /ai/chat | 32 KB | ai/handler.go:28 |
| MCP 数据端点 | 64 KB | mcp/handler.go:36 |
| POST /auto-record/trajectories | 64 KB | autorecord/handler.go:23 |
| POST /ops/client-log | 64 KB | opslog/handler.go:22 |
| POST /api/prod/payment/virtualPayNotify | 64 KB（handler 内 LimitReader，非统一中间件） | payment/handler.go:223 |
| POST /file/upload | 单文件 10 MB / 单请求 50 MB / 最多 9 个 | file/handler.go:35-37 |
| 默认兜底（maxBytes 小于等于 0） | 4096 B | middleware/body.go:13 |

> 超限统一语义：JSON body 超上限 → **HTTP 413 + `code="4130"`**；multipart 单文件/总量业务超限仍为 400 + `code="4000"`（+ `biz_code=FILE_SIZE_EXCEEDED`）。

### 6.3 限流与配额

| 范围 | 阈值 | 实现 | 源文件 |
|------|------|------|--------|
| GET /health/live、/health/ready | 30 次/分钟/IP | 内存滑动窗口（同容器内 app 与 sse 两个 Server 共享同一 limiter 实例，健康端点合并共享 30 次/分钟/IP 预算；双容器部署时两容器各自独立计数，每容器 30/min/IP） | main.go:200-203,309-311 |
| 公开路由组（login、支付回调、wx 回调、/invite/resolve 另加、/system/config） | 60 次/分钟/IP | 同上 | main.go:209-211 |
| GET /invite/resolve | 60 次/小时/IP | 独立 limiter | invite/handler.go |
| DELETE /auth/account | 5 次/小时/IP | 独立 limiter | main.go:251 |
| POST /auth/phone/bind | 10 次/分钟/IP | 独立 limiter | main.go:260 |
| SSE POST /ai/chat | 30 次/分钟/IP | 独立 limiter | main.go:314 |
| SSE 并发连接 | 单用户 2、进程内 `SSE_MAX_CONNS`（默认 200） | `chatLimiter`（package 级，仅 sse 进程） | ai/handler.go |
| POST /file/upload | 60 次/分钟/IP | 独立 limiter（防海量小文件耗尽 files 行/inode；先于 5 分钟 TimeoutHandler 执行） | main.go:268-275 |
| MCP 数据端点（open 公开 /mcp/*） | 60 次/分钟/IP（外层）+ 30 次/分钟/API Key（内层） | IP limiter + KeyRateLimiter | mcp/rpc.go:29-37、mcp/handler.go:59-70 |
| AI 每日配额 | 非 VIP 10 次/天、VIP 100 次/天 | Redis ai:daily_chat:<userId>:<date> Lua 原子计数 | ai/handler.go:25-27、ai/service.go:259-272,429-467 |
| 逆地理编码 | 200 次/用户/天 | Redis location:reverse:<userId>:<date>，fail-closed | location/quota.go:14-51 |
| 限流算法 | 滑动窗口，maxBuckets=10000（1.2 倍触发驱逐） | slidingWindowLimiter | middleware/ratelimit.go:26-97 |
| 可信代理 | TRUSTED_PROXY_CIDR（配置后才解析 X-Forwarded-For/X-Real-IP） | | middleware/ratelimit.go:149-191 |

> 限流/配额命中统一返回 429 + `code="4290"` + `biz_code="RATE_LIMITED"`。

### 6.4 其他阈值

- MCP：默认返回 500 条、上限 1000，日期跨度上限 180 天（mcp/handler.go:32-38）。
- 自动记录轨迹：单批最多 50 点（autorecord/handler.go:22）。
- 客户端日志：单次最多 200 条（超限保留前 200 条）；缺 `t`/`type` 或 `type` 超 128 字节的事件**整条丢弃**；单条 detail 超 1024B 时**丢弃该条事件的 detail 字段**（事件保留，仅入 t/type，不做部分截断；opslog/handler.go）。
- 日记条目：正文不超过 10000 code point、颜色 #RGB/#RRGGBB/#RRGGBBAA、地址不超过 500、图片不超过 9（diary/handler.go:571-632）。
- 记忆：标题 1~50 字、正文不超过 10000 字（diary/handler.go:388-425、mcp/handler.go:540-560）。

## 7. 幂等约定

| 场景 | 幂等键 / 机制 | 行为 | 源文件 |
|------|--------------|------|--------|
| 支付回调发货 | orders.transaction_id 唯一索引 + orders.state（pending/closed→paid） | 重复回调不重复发货；closed 被支付补记并发货；金额>0 一律发货（不一致仅告警）；瞬时故障返回非 2xx 触发微信重试，业务性拒绝返回固定成功 | payment/handler.go、payment/service.go |
| 关闭订单 | CloseOrder 条件更新（仅 pending 且属主） | 已关闭/已支付/并发关闭均返回 200 | payment/handler.go:107-161 |
| AI 对话 | 客户端 request_id（格式 ^[A-Za-z0-9_-]{8,64}$）；无则消息哈希 | 重连复用同一 request_id 命中回复缓存，不重复扣配额/落库 | ai/handler.go:31-32,78-81、ai/service.go:77-83,193-214 |
| MCP POST /mcp/key | 用户唯一（api_keys UNIQUE(user_id)） | 已存在时返回既有 key（get-or-create） | mcp/handler.go:177-225 |
| POST /mcp/key/rotate | 事务内先删后建 | 每次换发新 key | mcp/handler.go:274-315 |
| 家庭加入 | family_members 唯一约束 + errAlreadyInTargetFamily | 已在目标家庭返回 200 | family/handler.go:238-239 |
| 绑定邀请人 | 事务内行锁 GetUserByIDForUpdate + 幂等检查 | 已绑定幂等成功；注册超过 7 天静默成功不绑定 | auth/handler.go:378-399 |
| 微信公众号消息 | msgID Redis SETNX（60s TTL） | 重复消息返回占位文案，不重复触发 AI | wxmp/handler.go:41,207-219,250-263 |
| 免费/试用 VIP 领取 | user_vip_claims UNIQUE(user_id,vip_id)、user_vips UNIQUE(user_id) | 重复领取返回 409 业务码 | vip/handler.go、迁移唯一约束 |
| 文件上传失败回滚 | 请求内已提交 part 的顺序回滚（记录/物理文件/配额） | 避免半成功 | file/handler.go:110-208 |

## 8. 全量端点登记

> 统计：main.go + 各 handler 注册的端点（含 HTTP 与 SSE 的 health 端点、以及 SaaS/open 互斥的两套 MCP 路由）。以下按域登记。

### 8.1 运维 / 健康

| 方法 | 路径 | 鉴权 | 域 | 说明 |
|------|------|------|----|------|
| GET | /health/live | 无（IP 30/min） | 运维 | 存活探针：进程正常即 200，不检查 DB/Redis |
| GET | /health/ready | 无（IP 30/min） | 运维 | 就绪探针：DB 不可用 → 503；Redis 故障不判死，body 上报 `status: degraded` + `redis: down`（正常为 `redis: up`） |
| GET | /health | 无（IP 30/min） | 运维 | 兼容别名，等价 `/health/ready`；SSE Server 另有同名健康检查 |

### 8.2 认证与账号（auth）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /auth/login | 公开（IP 60/min） | body {code,inviter?}；微信 jscode2session → 查找/创建用户（含个人家庭、试用 VIP、邀请奖励、邀请码）→ 返回 {sessionId,newUser,userInfo} |
| POST | /auth/logout | 登录态 | 删除当前 session |
| GET | /auth/phone | 登录态 | 返回 {phoneNumber,canModifyToday}；canModifyToday 基于上海时区当日是否已绑过 |
| POST | /auth/phone/bind | 登录态 + IP 限流 10/min | body {code}；每日最多绑定一次（服务端日限，原子条件更新），微信换号后写 phone_bind_time；手机号唯一冲突 409 语义 |
| POST | /auth/phone/unbind | 登录态 | body {code}（不消费微信接口）；未绑定时 400 phone not bound |
| POST | /auth/inviter | 登录态 | body {inviter}；补绑邀请人，事务内行锁+幂等；自邀/不存在 400；注册超过 7 天静默成功 |
| DELETE | /auth/account | 登录态 + IP 5/h | body {confirmName} 必须等于当前昵称；先删当前 session 再事务删数据，后台异步清理 session/文件 |

### 8.3 用户（user）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /user/profile | 登录态 | 用户资料 + VIP + 公众号订阅状态 |
| PUT | /user/avatar | 登录态 | body {fileId}；文件需属主且 file_type=image；更新后异步生成地图标记头像 |
| PUT | /user/nickname | 登录态 | body {nickName}；经 validator.ValidateNickname |
| PUT | /user/lang | 登录态 | body {lang}；仅 zh/zh-Hant/en |
| GET | /user/vip | 登录态 | 返回 {isVip,expireTime} |
| GET | /user/common-addresses | 登录态 | 返回 {addresses:[{name,lat,lon,count}]} |
| POST | /user/common-addresses/refresh | 登录态 | 触发 SummarizeUserCommonAddresses 后返回列表 |
| PUT | /user/common-addresses/{name} | 登录态 | body {newName}；源行 FOR UPDATE，重名合并+删源、renameDiaryEntriesAddress 分批（1000/批）同步日记地址；newName 不超过 100 code point |

### 8.4 家庭（family）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /family | 登录态 | 返回 {familyId,ownerId,isPersonal,members[]} |
| POST | /family | 登录态 | 从个人家庭升级/创建共享家庭，返回 {familyId}；已在家庭 409 语义（本端点不抢家庭锁，无 429 分支） |
| POST | /family/invite-link | 登录态 | 个人家庭会先创建共享家庭；返回 {linkId} |
| POST | /family/invite-link/join | 登录态 | body {linkId}；已在目标家庭 200 幂等；家庭不存在/满/个人家庭按业务码返回 |
| POST | /family/leave | 登录态 | 退出共享家庭；owner 不可退出 |
| DELETE | /family/members/{userId} | 登录态 | 移除成员；不能移除自己/owner，非 owner 403 |
| DELETE | /family | 登录态 | 解散共享家庭；个人家庭/非 owner 403 |

### 8.5 邀请（invite）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /invite/resolve | 公开（IP 60/h） | query code（8 位白名单字符集）→ {userId}；非法 400、未找到 404 |
| GET | /invite/list | 登录态 | 当前用户邀请列表 {list:[{userId,nickName,avatarUrl,joined}]} |
| POST | /invite/qrcode | 登录态 | body 可空 {raw?}；返回 {url}（小程序码/原始码） |

### 8.6 日记（diary）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /diary/info | 登录态 | 日记卡片列表，游标分页（见第 4 节） |
| GET | /diary/stats | 登录态 | 日记统计 |
| POST | /diary/info/dates | 登录态 | body {dates[]}（最多 31），按日期返回卡片 |
| PUT | /diary/info | 登录态 | body {id,coverImage?}；更新家庭某日封面（锁内评估降级，冲突 OPERATION_IN_PROGRESS） |
| DELETE | /diary/info | 登录态 | query id（虚拟 ID）；删除该家庭某日日记；非本家庭 403 |
| GET | /diary/details | 登录态 | query diaryId（虚拟 ID）、memberUserId?、page、size；返回 data+count+extra |
| POST | /diary/details | 登录态 | body 条目字段（见 6.4），返回 {id, card?} |
| PUT | /diary/details | 登录态 | body 含 id，更新条目 |
| DELETE | /diary/details | 登录态 | query id，删除条目 |
| POST | /diary/details/auto | 登录态 + VIP | body {lat,lon}；逆地理（受 200/日配额）自动成文；非 VIP NOT_VIP |
| POST | /diary/details/memory | 登录态 | body {title,content,recordTime}，新建回忆；标题 1~50、正文不超过 10000 |
| PUT | /diary/details/memory | 登录态 | body {id,title,content,recordTime} |
| DELETE | /diary/details/memory | 登录态 | query id |
| GET | /diary/cover-url | 登录态 | query familyId,recordDate；需为当前家庭成员；返回 {coverImg} |

### 8.7 自动记录（autorecord）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /auto-record/config | 登录态 | 非 VIP 恒返回 {enabled:false}；VIP 返回用户 auto_record_enabled；读用户 DB 失败时降级返回 200 {enabled:false}（非 500） |
| PUT | /auto-record/config | 登录态 | body {enabled}；enabled=true 需 VIP，否则 NOT_VIP |
| PUT | /auto-record/active | 登录态 | 记录活跃心跳：仅更新 last_active_at（不触碰 abnormal_alert_sent_at；异常告警的抑制由候选查询附带 last_active_at 条件间接实现，见 02c AR-11.7） |
| POST | /auto-record/trajectories | 登录态 + VIP | body {points:[{lat,lon,recordedAt}]}（最多 50）；坐标范围校验；写 auto_record_trajectories |

### 8.8 文件（file）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /file/upload | 登录态 | multipart/form-data，字段名 files；单文件不超过 10MB、总量不超过 50MB、最多 9 个；扩展名白名单 jpg/jpeg/png/gif/webp；VIP 存储配额更大（普通 1GiB、VIP 5GiB，USER_IMAGE_STORAGE_LIMIT_BYTES*）；HTTP 层 5 分钟 TimeoutHandler |
| DELETE | /file/{fileId} | 登录态 | 删除文件；非属主 403、系统文件 403、被引用 400、不存在 404 |
| GET | /file/download/{fileId} | 登录态 | 返回 {url}；属主或同 current_family_id；系统文件按 metadata.family_id 校验 |

### 8.9 VIP（vip）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /vip | 登录态 | 在售付费 VIP 商品列表（含 prices） |
| GET | /vip/free | 登录态 | 免费 VIP 列表 [{id,name}] |
| POST | /vip/free/claim | 登录态 | body {vipId}；已领取 409 语义 FREE_VIP_ALREADY_CLAIMED |
| GET | /vip/free/check | 登录态 | query vipId，返回 {claimed} |
| POST | /vip/new-user | 登录态 | 领取新用户试用 VIP；已领取 TRIAL_VIP_ALREADY_CLAIMED |

### 8.10 支付（payment）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /payment/virtual/request | 登录态 | body {vipId,env}（env 仅 0/1；env=1 需 PAYMENT_ALLOW_SANDBOX=1，否则 403）；创建微信虚拟支付订单 |
| POST | /payment/virtual/cancel | 登录态 | body {outTradeNo}；仅关闭自己 pending 订单，重复/已关闭幂等 200 |
| GET | /payment/virtual/status | 登录态 | query outTradeNo；返回 {state}；非属主按 ORDER_NOT_FOUND |
| GET | /api/prod/payment/virtualPayNotify | 微信签名（公开组） | 服务器地址验证：校验 signature/timestamp/nonce 后原样返回 echostr |
| POST | /api/prod/payment/virtualPayNotify | 微信加密验签（公开组） | 安全模式回调：msg_signature 验签 + AES 解密发货；明文模式被拒；瞬时故障返回 500 触发重试 |

### 8.11 推送（push）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /subscribe/record | 登录态 | body {templateId,scene,accept}；仅支持 scene=ABNORMAL_ALERT，否则 400 |

### 8.12 位置（location）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /location/reverse | 登录态 | query latitude,longitude,pois?；坐标校验；200/用户/日配额；返回 address/detailAddress/landmark/areaCode/areaName/pois |

### 8.13 AI 对话（ai，SSE Server :8081）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /ai/chat | 登录态 + IP 30/min | body {message（不超过 2500 code point），request_id?}；SSE 流式返回（见 2.2）；日配额非 VIP 10、VIP 100 |

### 8.14 MCP / 开放接口（mcp）

**模式 A：open 公开端点（/mcp，外层 IP 60/min，内层 API Key）**

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /mcp | Authorization: Bearer apiKey | MCP Streamable HTTP（JSON-RPC：initialize/tools/list/tools/call/prompts/*）；202 表示无 id 的通知 |
| GET | /mcp/diary | API Key | query start_date,end_date,limit；返回 {data,truncated} |
| POST | /mcp/memories | API Key | body {record_time,title,content}；返回 {memory_id} |
| GET | /mcp/memories | API Key | 同日记查询，合并回忆与日记 |

**模式 B：saas 内部端点（/internal/mcp，X-Worker-Secret=MCP_WORKER_SECRET + API Key）**

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /internal/mcp/rpc | X-Worker-Secret + API Key | 同 /mcp |
| GET | /internal/mcp/diary | 同上 | 同 /mcp/diary |
| POST | /internal/mcp/memories | 同上 | 同 /mcp/memories |
| GET | /internal/mcp/memories | 同上 | 同 /mcp/memories |

**API Key 管理（登录态分组内，两种模式均注册）**

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /mcp/key | 登录态 | 创建或返回既有 key；响应 {apiKey,apiUrl,memoryUrl,expiresAt,mcpConfig,authUrl?}；apikey: 身份 403 |
| GET | /mcp/key | 登录态 | 无 key 时 {hasKey:false} |
| DELETE | /mcp/key | 登录态 | 删除当前用户 key |
| POST | /mcp/key/rotate | 登录态 | 事务内删旧建新并返回新 key |

### 8.15 客户端运维日志（opslog）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | /ops/client-log | 登录态 | body {events[],device?,appVersion?}；最多 200 条事件；缺 `t`/`type`、`type` 超 128 字节的事件被静默丢弃；单条 detail 超 1024B 时仅整体丢弃 detail 字段（不截断保留，见 §6.4）；过滤后为空不落库直接 200；写 client_ops_logs |

### 8.16 公众号回调（wxmp）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /wx/callback | 微信服务器签名 | 校验 signature/timestamp/nonce，成功返回 echostr |
| POST | /wx/callback | 明文签名或加密 msg_signature | 解明文/密文 → event（订阅/退订）、text/voice（异步 AI 客服消息分段推送）；其他类型返回固定文案；to_user_name 需匹配 WECHAT_MP_GHID（若配置） |

### 8.17 系统配置（system）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /system/config | 公开（IP 60/min） | **只读能力探测（capability probe），非配置中心、无运行时配置管理**。返回 {mode,features}；ai=AI_API_KEY 非空、mcp=MCP_ENABLED（默认 true，可用 MCP_ENABLED=0 关）、freeVip 由 `FREE_VIP_ENABLED` 控制（默认 true，置 0 关闭免费入口无需发版）；open 强制 payment=false,wxmp=false；saas 的 payment 需三项微信虚拟支付 env 齐全、wxmp=WECHAT_MSG_TOKEN 非空（system/handler.go） |

### 8.18 后台任务（非 HTTP，供 API 行为解释）

`jobs.Runner.Start`（jobs/runner.go），所有任务用 PostgreSQL advisory lock（`lock:background:{task}`）互斥、任务自身幂等，间隔可用 env 覆盖（默认值来自 config.go）；`KnownJobNames` 共 10 项，Redis 记录 `job:last_success` / `job:last_failure` / `job:fail_streak`，`watchJobHealth` 每分钟做失联检测（>3× 周期输出 `job_stale`）并消费 `job:trigger:<name>` 人工补跑：

| 任务 | 间隔（默认） | 对应 API/表 |
|------|-------------|-------------|
| 自动记录处理 | 5 分钟 | 消费 /auto-record/trajectories 写入的轨迹 |
| 异常提醒检查 | 5 分钟 | 关联 /subscribe/record、users.abnormal_alert_sent_at |
| 订单超时关闭 | 1 分钟 | 关闭 24h 未支付 orders |
| AI 对话日志清理 | 24 小时 | 删除 ai_dialog_logs 中 >90 天记录（无每用户条数上限） |
| 轨迹数据清理 | 6 小时 | auto_record_trajectories |
| 孤立文件清理 | 7 天 | files/物理存储 |
| 孤立轨迹图清理 | 7 天 | 轨迹地图文件 |
| 客户端日志清理 | 24 小时 | 删除 client_ops_logs 中 >30 天记录 |
| 常用地址汇总 | 每日 03:00 | /user/common-addresses/refresh 同一底层 |
| 已删对象 CDN 缓存刷新 | 24 小时（`JOB_INTERVAL_PURGE_DELETED_OBJECTS`；`CDN_REFRESH_ENABLED=1` 才启用） | 消费 Redis `purge:oss:pending` 队列，分批调阿里云 CDN 刷新（ADR-0013） |

## 9. 通用契约模板

新增端点时按下表填满，缺一不可（对应 spec-check 提示级「路由 ↔ L3 覆盖」）：

| 项 | 内容 |
|----|------|
| 方法 + 路径 | 例：POST /xxx/yyy |
| 鉴权 | 公开 / 登录态（Session）/ open 额外 X-Private-Api-Key / Worker X-Worker-Secret / 微信签名 |
| 请求 | body/query 字段、类型、必填、上限 |
| 成功响应 | data 结构（统一 envelope，code=0000） |
| 错误 | 触发的 code / biz_code / HTTP 状态 |
| 数据归属 | 从 context 取 user_id；跨用户/家庭的校验点 |
| 幂等 | 幂等键与重复请求语义 |
| 限流/配额 | 命中的 limiter 或 Redis 配额键 |
| 超时 | 路由级或 server 级 |
| 源文件 | handler 与注册位置 |


