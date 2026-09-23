# PP-02G 文件、头像与推送（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0002（图片存储使用阿里云 OSS）、ADR-0013（已删除图片边缘缓存定期批量收敛）
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。

## 1. 背景与目标

- 解决什么问题：统一图片/文件的保存、下载、引用计数与配额；由用户头像生成轨迹图 marker；通过微信服务号模板消息、小程序订阅消息与客服消息触达用户。
- 服务谁：小程序登录用户（文件/头像）、VIP 且开启自动记录的用户（推送）、公众号订阅用户（AI 客服消息）。
- 本域边界：
  - 属于本域：文件上传/下载/删除、本地与 OSS 存储、图片存储配额、头像更新与 avatar marker 生成、孤儿文件清理、微信推送（异常告警/新地点提醒）、公众号回调与客服消息、access_token 缓存。
  - 不属于本域：日记条目与封面业务（PP-02B）、AI 对话引擎与配额（AI 分册）、VIP 判定（VIP 分册）、自动记录定位采集（自动记录分册）、家庭与邀请（家庭分册）。
- 关键实现位置：backend/internal/file/handler.go、storage.go、oss.go、cleanup.go、errors.go；backend/internal/avatar/service.go；backend/internal/push/service.go、mp.go、mini.go、token.go、handler.go、common.go、models.go；backend/internal/wxmp/handler.go、client.go、crypto.go；backend/internal/db/sqlc/file.sql、user.sql、wx_mp_account.sql、auto_record.sql；backend/migrations/000001_baseline.up.sql。

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | 图片使用固定公开 URL；OSS Bucket 公开读 | 降低存储与签名复杂度；URL 含 UUID 不可枚举 | ADR-0002 |
| D2 | 本地存储为兜底，配置齐全时走 OSS；URL 由 storage.URL(path, storageType) 统一构造 | 开源版/未配 OSS 时零依赖可用 | ADR-0002 |
| D3 | 用户上传只校验 Content-Type 前缀 image/ 与扩展名白名单 | 简化实现策略 | 无 |
| D4 | 配额用条件原子 SQL（IncrementUserImageStorage，超限 0 行），避免 check-then-act 竞态 | 并发上传不超卖 | 无 |
| D5 | 头像 marker 生成不加分布式锁；连续换头像最坏产生孤儿 marker，由清理任务回收 | 简单优先，极低概率 | 无 |
| D6 | 异常告警按自然日去重，**先记账后发送**（at-most-once，宁可漏报不重报） | 防止重复打扰；持续异常次日可再触发 | 无 |
| D7 | 异常告警同时尝试服务号与小程序通道（各自独立）；新地点提醒仅服务号 | 产品决策 | 无 |
| D8 | 微信 access_token 优先读 Redis（key wechat:mp:access_token:{appID}，TTL 110 分钟），未命中调 stable_token | 避免 token 失效竞态 | 无 |
| D9 | 公众号加密模式使用 AES-256-CBC + PKCS7，**按 32 字节对齐填充**（非 AES 块大小 16） | 腾讯官方实现要求，按 16 对齐微信无法解密 | 无 |
| D10 | 错误日志中脱敏 access_token（sanitizeTokenFromError / util.SanitizeURLError） | 防止密钥泄漏到日志 | 无 |
| D11 | 物理文件删除一律在 DB 事务提交之后 | 避免 DB 回滚后文件已丢失 | 无 |
| D12 | 物理删除允许与 DB 删除异步：用户删除在事务提交后删物理文件（失败由孤儿清理兜底）；周期清理完成前，公开桶上的文件 URL 仍可访问 | 容忍清理窗口；不引入签名 URL / 图片鉴权（I8） | 无 |
| D13 | **已删除图片的边缘缓存收敛（ADR-0013）**：OSS 写入带 `max-age=31536000, immutable`，物理删除后 URL 在 CDN/客户端缓存最长一年仍可命中——已删除图片必须最终不可达，采用**定期批量**缓存清理（purge）收敛，不做逐次同步清理、不改 immutable 写入策略 | 读性能与回源成本优先；接受有限收敛窗口 | ADR-0013 |

## 3. 核心流程（用户故事 + 时序）

### F-1 用户图片上传

作为用户，我在新增条目时上传照片。

**时序**：POST /file/upload（multipart，字段名 files，受 session 保护 + 60 次/分钟/IP 限流（防海量小文件耗尽 files 行/inode），外层 http.TimeoutHandler 5 分钟）→ 预检查存储配额 → ContentLength>50MB 拒绝 → Content-Type 必须 multipart/form-data → MaxBytesReader(MaxTotalSize+1MB) → MultipartReader 逐 part：非 files 字段跳过；文件数 >9 拒绝（第 10 个 files part）；handleUploadPart 校验扩展名/Content-Type → 写临时文件并逐块计数（>10MB 拒绝）→ 总大小 >50MB 拒绝 → 本地 Save 或 OSS 上传 → db.WithTx：CreateFile + 条件 IncrementUserImageStorage（0 行 → 配额错误）→ 返回 {files:[{fileId,url}]}；任一 part 失败回滚本请求已提交的文件/配额。

**AC**：
- 扩展名不在 {.jpg,.jpeg,.png,.gif,.webp} 或 Content-Type 不以 image/ 开头 → HTTP 400 code=4000 + biz_code=INVALID_FILE_TYPE。
- 单个 part >10MB 或单请求总量 >50MB → HTTP 400 code=4000 + biz_code=FILE_SIZE_EXCEEDED。
- 第 10 个 files part → HTTP 400 code=4000（MaxFiles=9）。
- 配额超限 → HTTP 400 code=4000 且 biz_code=USER_IMAGE_STORAGE_LIMIT_EXCEEDED。
- 成功响应含 files[0].fileId 与 files[0].url；upload 超时（>5 分钟）由 TimeoutHandler 返回原始 JSON {"code":"5001","message":"upload timeout"}（非统一 envelope 包）。

### F-2 图片存储配额

- 普通用户上限 USER_IMAGE_STORAGE_LIMIT_BYTES（默认 1GiB），VIP 上限 USER_IMAGE_STORAGE_LIMIT_BYTES_VIP（默认 5GiB），配置为 0 表示不限制。
- 上传前 checkStorageLimit：used >= limit 直接拒绝；limit<=0 跳过。
- 实际扣减在事务内 IncrementUserImageStorage（条件更新）；删除文件、删除孤儿文件时 DecrementUserImageStorage（GREATEST 保底不为负）。

**AC**：并发上传使总量超过 limit 时，超出的请求返回 biz_code=USER_IMAGE_STORAGE_LIMIT_EXCEEDED；users.image_storage_bytes 永不为负。

### F-3 文件下载（取 URL）

**时序**：GET /file/download/{fileId} → fileId 为空 400 → GetFileByID（不存在 404，DB 错误 500）→ system 文件：解析 metadata.family_id，当前用户 current_family_id 不匹配或缺失 → 403；非 system：CreatedBy 为空 → 403，非本人则查双方 current_family_id 是否相同，不同 → 403 → storage.URL 失败 → 500 → 返回 {url}。

**AC**：跨家庭用户下载他人图片 → 403 code=4030；不存在 → 404 code=4040；URL 构造失败 → 500 code=5001。

### F-4 文件删除（引用计数）

**时序**：DELETE /file/{fileId} → DeleteFile：db.WithTx 内 GetFileByIDForUpdate（行锁，不存在 ErrFileNotFound）→ system 文件 → ErrSystemFileDelete → CreatedBy 为空或非本人 → ErrNotFileOwner → GetFileReferences，若 used_by_cover 或 used_by_entry 或 avatar_user_count>0 → ErrFileInUse → DeleteFile 记录 + 若 file_type=image 扣减配额 → 提交后物理删除。

**AC**：被条目/封面/头像引用的文件删除返回 400 code=4000（file is still in use）；删除 system 文件 403；删除他人文件 403；物理删除发生在事务提交后。

### F-5 头像更新与 avatar marker 生成

作为用户，我在设置页选择微信头像。

**时序**：前端先 POST /file/upload?type=avatar 上传（file_type=image，计入配额），再 PUT /user/avatar body {fileId}（≤8KB）→ 校验 fileId 非空、文件存在（404）、file_type=image（400）、属于本人（403）→ storage.URL 取头像 URL → UpdateUserAvatar（users.avatar + avatar_file_id）→ 旧 avatar_file_id 不同则 DeletePhysicalIfUnreferenced → safe.Go 30s 内 avatarService.GenerateMarker：avatarURL 为空直接返回；下载头像（仅公开 HTTPS、Content-Type 含 image、≤5MB、重定向也须公开 HTTPS 且 ≤10 跳、30s 超时）；解码尺寸 >4096 报错；缩放到 75×75 并加 2px 白边生成 PNG；SaveSystemWithName 存文件（system 类型，name {uuid}.png）；读旧 marker → UpsertUserAvatarMarker → 旧 marker 物理删除（safe.Go 10s，best-effort）；返回 marker URL。

**AC**：非本人图片 403 code=4030；非 image 400 code=4000；头像 URL 非公开 HTTPS 时 marker 生成失败仅记日志、不影响头像更新接口；user_avatar_markers 每用户一行（PK user_id）。

### F-6 存储与 URL 构造

- 本地路径：Save 生成 {YYYY/MM}/{uuid}{ext}，落盘 baseDir（STORAGE_LOCAL_PATH，默认 /opt/pathmemos/uploads）。
- OSS 用户上传 key：uploads/{YYYY/MM}/{uuid}{ext}；SaveSystemWithName 的 OSS key：uploads/{YYYY/MM}/{fileName}{ext}。
- storage.URL：storageType=oss 走 oss.URL（baseURL+key）；否则要求 baseURL 非空（FileBaseURL），返回 baseURL + /uploads/ + 逐段 PathEscape 的 path；path 为空返回空串且无 error。
- OSS 上传设置 Content-Type（按扩展名）与 Cache-Control public, max-age=31536000, immutable。
- 删除路径做安全检查：Clean、非空、filepath.IsLocal、结果必须在 baseDir 内；本地不存在不报错；按 storageType 分流 OSS/本地。

**AC**：path 为空时 URL 返回空串且无 error；baseURL 未配置时本地 URL 返回 error（不静默产生相对路径）。

### F-7 孤儿文件清理（后台）

**时序**：runCleanupOrphanFiles（默认每 7 天）：先 ClearAvatarReferencesToOrphanFiles（清理 avatar_file_id 指向不存在文件的用户引用）→ 分页 ScanOrphanFiles（file_type=image、created_at 早于 1 天、无条目/头像/封面引用、id 游标，每批 1000）→ 逐条物理删除成功后删 DB 记录并扣减配额。runCleanupOrphanTrajMaps（默认每 7 天）：分页 ScanOldSystemFiles（file_type=system、created_at 与 updated_at 均早于 7 天、无封面引用、无 user_avatar_markers.marker_path 匹配）→ 先物理删除成功者再 BatchDeleteFiles。

**AC**：被条目/封面/头像引用的文件不会被清理；物理删除失败保留 DB 记录待下轮重试。

### P-1 公众号服务器验证与回调入口

**时序**：GET/POST /wx/callback（公开，WorkerAuth 豁免）→ GET：取 signature/timestamp/nonce/echostr，按 token 排序后 sha1 校验，通过 200 原样返回 echostr，失败 403 body fail；其他方法 405。POST：body 限制 64KB（超限/读失败/解析失败返回纯文本 success）；query 带 msg_signature 视为加密模式，校验 msg_signature（失败 403 fail），AES 解密并校验尾部 appID == WECHAT_MP_APPID（不符返回 success），否则用明文 signature 校验（失败 403 fail）；ToUserName 必须等于 WECHAT_MP_GHID，否则返回 success。

**AC**：签名错误 403 fail；body >64KB 返回 200 success；加密模式不会回退明文；ToUserName 不匹配返回 success。

### P-2 公众号消息处理与去重

- 文本/语音且 MsgId 非空：Redis SETNX wxmp:msgid:{msgId} TTL 60s，重复则被动回复「正在思考，请稍候…」并结束（避免微信重试触发重复 AI）；TTL 到期后同一 MsgId 会被再次处理。Redis 错误时降级为继续处理（不丢消息）。
- event=subscribe：异步 resolveUser + 同步更新 wx_mp_accounts.subscribed=true + 异步发送小程序卡片，被动回复欢迎语；event=unsubscribe：subscribed=false，回复空。
- 文本：**先立即被动回复「正在思考」占位**（满足微信 5s 窗口），后台 goroutine 内 resolveUser；失败 → 客服消息发「尚未登录…」+ 异步小程序卡片；成功 → 异步 AI。
- 语音：Recognition 为空 → 回复语音识别失败提示；否则按文本处理。
- 其他类型：回复「暂只支持文字和语音消息」。

**AC**：同一 MsgId 在 60s 内重复投递不触发第二次 AI；Redis 不可用时消息仍被处理。

### P-3 公众号 AI 客服消息

**时序**：safe.Go（超时 ai.AIStreamTimeout+30s）→ 读取配置（失败发「服务繁忙」）→ 以 `WECHAT_MP_PROMPT`（为空回退 `AI_PROMPT`）调用 aiService.ChatWithPromptUsingConfig 流式回调 → 累积字节 ≥2000 时 stripMarkdown 后 splitWeChatText 分段调用 SendKfMessage；全部段失败记录告警日志 `[ALERT] wx mp all kf segments failed`；AI 出错时先 flush 剩余，再发「服务繁忙」或（配额耗尽）「当天额度已用完」。

**AC**：单条客服消息 ≤2000 字节；全部段失败产生可 grep 的 ALERT 日志；失败不影响 /wx/callback 的 HTTP 响应。

### P-4 异常告警推送（服务号 + 小程序）

**时序**：后台 runAbnormalAlertCheck（默认每 5 分钟，时间窗口 8:00–22:00）→ 分页 ListAbnormalAlertCandidates（auto_record_enabled=true、VIP **严格有效** `expire_time > now()`、abnormal_alert_sent_at 为空或早于 1 小时前、last_active_at 为空或早于 60 分钟前）→ 对每个用户 SendAbnormalAlert：时间窗口校验 → 用户必须开启自动记录且是 VIP → 取 wx_mp_accounts（subscribed 且 mp_openid 非空=服务号可用；abnormal_subscribe_accepted 且 users.open_id 非空=小程序可用）→ 无可渠道直接返回 → MarkAbnormalAlertSent（原子，0 行=今日已发，跳过）→ 并发发送两条通道（30s 超时，safe.Go）→ handleMPError/handleMiniError 处理微信错误码。

**AC**：8:00 前/22:00 后不推送；非 VIP 不推送；当日已标记过不重复推送；标记成功但发送失败不回滚标记。

**微信错误码处理**：43004（服务号取消订阅）→ wx_mp_accounts.subscribed=false；43101（小程序拒绝订阅）→ users.abnormal_subscribe_accepted=false；47003（模板数据非法）→ error 日志；45009（限频）→ warn 日志。错误码用正则 code=(\d+) 从错误串提取（postWechatMessage 生成 action send error: code=%d msg=%s）。

### P-5 新地点提醒（仅服务号）

**时序**：日记 CreateAutoEntry 成功后 safe.Go 调 SendNewPlaceAlert → 时间窗口 6:00–22:59（h<6 或 h>=23 跳过）→ 取 wx_mp_accounts，必须 subscribed 且 mp_openid 非空，否则跳过 → 取 diary_entries（placeName 优先 address，其次 detail_address）与 diaries（recordDate）→ 构造 pagePath=pages/NoteDetail/NoteDetail?baseInfo=<urlencode 的 JSON{id,recordDate,dateName,coverImg}> → 调 mp.SendNewPlaceAlert（date 格式 2006年01月02日、time 格式 15:04，地点名截断 20 runes）→ 失败 handleMPError 并返回 error（不重试）。

**AC**：无服务号通道不发送；地点名超过 20 runes 截断并以 … 结尾；每个新地点只尝试一次。

> 约定：`baseInfo` 的 `dateName`/`coverImg` 当前恒为空，由前端 `NoteItem`/`NoteDetail` 用本地数据覆盖；后端不补齐。

### P-6 订阅记录

**时序**：POST /subscribe/record body {templateId?, scene, accept}（≤4096 字节）→ scene 必须为 ABNORMAL_ALERT，否则 400 unsupported scene → RecordSubscribe 更新 users.abnormal_subscribe_accepted=accept → {}。userID 从 session 上下文取，不采信 body。

**AC**：scene != ABNORMAL_ALERT → 400 code=4000；body >4096 → **413 code=4130**；成功写入 abnormal_subscribe_accepted。

### P-7 access_token 与微信客户端

**时序**：GetAccessToken：appID/secret 未配置报错；Redis key wechat:mp:access_token:{appID} 命中直接返回；否则 POST https://api.weixin.qq.com/cgi-bin/stable_token（JSON grant_type/appid/secret），响应体读 8KB，errcode!=0 或 token 为空报错，成功后 best-effort 写缓存 TTL 110 分钟。

**FetchUserInfo**：openID 非空；GET /cgi-bin/user/info，8KB，subscribe==1 → true。**UploadTempMedia**：mediaType/data 非空；multipart 上传；thumb 返回 thumb_media_id，其他返回 media_id。**SendMiniProgramPage**：openID/appID/thumbMediaID 非空；空 pagePath 默认 pages/index/index，空 title 默认 打开小程序；errcode!=0 报错。**SendKfMessage**：openID 非空；errcode!=0 报错。

**AC**：缓存命中不请求微信；未配置 appID/secret 返回 error 不 panic；所有微信 API 响应体读取上限 8KB。

## 4. 数据模型

| 表 | 关键字段 | 说明 |
|----|---------|------|
| files | id text PK、created_by text FK→users ON DELETE SET NULL、path text、name text、suffix text、size_bytes bigint DEFAULT 0、file_type text DEFAULT image、metadata jsonb、storage_type text DEFAULT local、created_at/updated_at | CHECK file_type ∈ {image,system}、storage_type ∈ {local,oss}、name ≤255、path ≤255、suffix ≤32；索引 file_type+created_at、created_by、metadata gin |
| user_avatar_markers | user_id text PK FK→users ON DELETE CASCADE、marker_path text NOT NULL、storage_type text DEFAULT local、updated_at | CHECK storage_type ∈ {local,oss}；每用户一行 |
| users | id、open_id、avatar、avatar_file_id FK→files ON DELETE SET NULL、image_storage_bytes bigint DEFAULT 0、auto_record_enabled、abnormal_subscribe_accepted、abnormal_alert_sent_at、last_active_at、current_family_id | CHECK image_storage_bytes >= 0；avatar 与 avatar_file_id 用于头像 |
| wx_mp_accounts | id PK、user_id FK→users ON DELETE CASCADE、mp_openid UNIQUE、unionid（部分唯一索引）、nickname、avatar、subscribed DEFAULT true、subscribe_time、last_interact_time、created_at/updated_at | 服务号绑定关系；推送前置数据 |
| client_ops_logs | id PK、user_id FK→users ON DELETE CASCADE、device、app_version、events jsonb、client_sent_at、created_at | 客户端操作日志（000004），与推送排障相关 |

推送模板不落表：模板 ID 与字段在 backend/internal/push/models.go 与 mp.go/mini.go 中定义。

## 5. API 契约

统一响应包同 PP-02B。

| 方法 | 路径 | 鉴权 | 请求 | 成功响应 | 主要错误 |
|------|------|------|------|---------|---------|
| POST | /file/upload | Session | multipart，字段 files（可多文件），query type 被后端忽略 | {files:[{fileId,url}]} | 400 4000（biz_code=INVALID_FILE_TYPE/FILE_SIZE_EXCEEDED/USER_IMAGE_STORAGE_LIMIT_EXCEEDED）；500 5001；超时 TimeoutHandler 原始 {"code":"5001","message":"upload timeout"} |
| GET | /file/download/{fileId} | Session | path fileId | {url} | 400 4000；403 4030；404 4040；500 5001 |
| DELETE | /file/{fileId} | Session | path fileId | {} | 400 4000（in use）；403 4030；404 4040；500 5001 |
| PUT | /user/avatar | Session | {fileId}（≤8KB） | {} | 400 4000；403 4030；404 4040；500 5001 |
| POST | /subscribe/record | Session | {templateId?, scene, accept}（≤4096B） | {} | 400 4000（unsupported scene）；413 4130（body 超限）；500 5001 |
| GET | /wx/callback | 公开（WorkerAuth 豁免） | query signature/timestamp/nonce/echostr | 200 echostr 原文 | 403 fail |
| POST | /wx/callback | 公开（WorkerAuth 豁免） | 微信 XML（≤64KB） | 200 XML 被动回复或纯文本 success | 403 fail（签名/加密签名失败）；405 其他方法 |
| POST | /ops/client-log | Session | {events[],device?,appVersion?}（≤64KB） | {} | 400 4000；413 4130；500 5001 |

> `/ops/client-log`：事件最多 200 条；缺 `t`/`type` 或 `type` >128 字节的事件被静默丢弃；单条 `detail` >1024 字节或非法 JSON 时整体丢弃 `detail`（不截断保留）；过滤后为空不落库直接 200（`opslog/handler.go`，写 `client_ops_logs`）。

**模板消息与订阅消息模板 ID**（backend/internal/push/models.go）：

| 常量 | 值 | 用途 |
|------|-----|------|
| TemplateIDAbnormalMP | 9r_Ij5IPLitRyIu-YNjI6S2nVeniMRQ4V4sGmGZalq0 | 异常告警服务号模板 |
| TemplateIDAbnormalMini | i7mcEEMDbhYU1oAC1-E0G0xvIBCmTl6f9c3jOq11m3g | 异常告警小程序订阅 |
| TemplateIDNewPlace | RQmSHeUlGYtrkGJVDZ6bEZ7OaGZUH9xNWmtb0huu8cc | 新地点提醒服务号模板 |

**服务号模板字段**：异常告警 time9=日期时间、thing5=固定提示；新地点 time1=日期、time6=时间、thing5=地点名（>20 runes 截断加 …）。**小程序订阅字段**：异常告警 thing5、time4；miniprogram_state 由 WechatMiniLinkEnvVersion 决定（release→formal，否则 trial）。

## 6. 关键实现约束

- **错误码映射**：文件删除 ErrNotFileOwner/ErrSystemFileDelete → 403 4030；ErrFileInUse → 400 4000；ErrFileNotFound → 404 4040。上传配额 → 400 4000 + biz_code USER_IMAGE_STORAGE_LIMIT_EXCEEDED。
- **上传阈值**：MaxFileSize=10MiB、MaxTotalSize=50MiB、MaxFiles=9、MaxNameLen=255 bytes（按 rune 边界截断）；MaxBytesReader=MaxTotalSize+1MiB；扩展名白名单 {.jpg,.jpeg,.png,.gif,.webp}，.jpeg 归一到 .jpg。
- **配额阈值**：普通 1GiB、VIP 5GiB（环境变量覆盖）；0=不限制；Decrement 用 GREATEST 保底。
- **头像阈值**：MaxAvatarDownloadSize=5MiB、MaxPixelDimension=4096、TrajectoryMarkerSize=75、TrajectoryMarkerBorder=2、GenerateMarkerTimeout=30s。
- **推送阈值**：异常告警窗口 8:00–22:00、新地点 6:00–22:59；发送超时 30s；告警候选条件以 02c AR-11.1 为准（含最后轨迹时间、last_active 与订阅通道条件），告警去重 1 小时/自然日；客服消息单条 2000 字节；公众号 body 64KB；消息去重 TTL 60s；thumb 缓存 48h；资料刷新节流 1h；access_token 缓存 110 分钟；微信 API 响应读取 8KB。
- **事务/后台**：文件配额与记录同事务；物理删除在提交后；异步任务一律 safe.Go（panic 记录日志）。
- **公众号加密**：EncodingAESKey 必须 43 字符（解码 32 字节）；PKCS7 按 32 字节对齐；被动回复附 MsgSignature；解密后校验 appID。
- **幂等/去重**：异常告警 MarkAbnormalAlertSent（SQL 条件原子）；公众号消息 Redis SETNX；access_token Redis 缓存。
- **安全**：下载归属校验；system 文件最多家庭内可见；日志脱敏 access_token；上传扩展名与 Content-Type 双重白名单；路径穿越防护。

## 7. 前端接入

| 页面/组件 | 行为 |
|-----------|------|
| utils/http.ts | uploadFile：压缩后（wx.compressImage，quality 按大小 80/65/50，压缩后仍 >10MiB 报错）逐张上传，单请求 field=files，并发 3；配额错误映射为 USER_IMAGE_STORAGE_LIMIT_EXCEEDED。updateAvatar：上传后 PUT /user/avatar {fileId} 并 setBaseInfo(avatar) |
| components/NoteEdit/NoteEdit.ts | 保存时对新增图片调 uploadFile，再 POST/PUT /diary/details |
| pages/Set/Set.ts | onChooseAvatar → request.updateAvatar；配额超限弹 VIP 升级；mpSubscribed 来自 GET /user/profile |
| components/SubscribePrompt/SubscribePrompt.ts | wx.requestSubscribeMessage(ABNORMAL_TEMPLATE_ID) → POST /subscribe/record {templateId,scene:ABNORMAL_ALERT,accept} |
| components/NoteItem/NoteItem.wxml | coverImg 为空时回退占位图 ../../image/default-bg.png |

## 8. 运维与任务

- **后台任务**（backend/internal/jobs/runner.go）：runCleanupOrphanFiles（JOB_INTERVAL_CLEANUP_ORPHAN_FILES，默认 7 天，最长 30 分钟）、runCleanupOrphanTrajMaps（JOB_INTERVAL_CLEANUP_ORPHAN_TRAJ_MAPS，默认 7 天）、runCleanupTrajectories（JOB_INTERVAL_CLEANUP_TRAJECTORIES，默认 6 小时）、runAbnormalAlertCheck（JOB_INTERVAL_ABNORMAL_ALERT，默认 5 分钟）、runCleanupClientOpsLogs（JOB_INTERVAL_CLEANUP_CLIENT_OPS_LOGS 默认 24h，删除 >30 天）、runPurgeDeletedObjects（JOB_INTERVAL_PURGE_DELETED_OBJECTS，默认 24h，单轮 ≤20 批 × 500 URL，失败整批回退）；任务用 PostgreSQL advisory lock 互斥（ADR-0005）。
- **环境变量**：STORAGE_LOCAL_PATH；OSS_ACCESS_KEY_ID、OSS_ACCESS_KEY_SECRET、OSS_ENDPOINT、OSS_BUCKET、OSS_PUBLIC_URL（ID/SECRET/ENDPOINT/BUCKET/OSS_PUBLIC_URL 任一非空即要求 ID/SECRET/ENDPOINT/BUCKET 四项齐备，见 config.validate）；WECHAT_MP_APPID、WECHAT_MP_SECRET、WECHAT_MP_GHID、WECHAT_MSG_TOKEN、WECHAT_ENCODING_AES_KEY、WECHAT_APPID、WECHAT_MINI_LINK_ENV_VERSION（默认 release）；USER_IMAGE_STORAGE_LIMIT_BYTES、USER_IMAGE_STORAGE_LIMIT_BYTES_VIP。
- **系统配置（环境变量）**：`STORAGE_PUBLIC_BASE_URL`（文件基址，即 `FileBaseURL`：open 模式由 `BuildSysConfig` 无条件覆盖为 `TrimRight(API_HOST, "/")`，此时 `STORAGE_PUBLIC_BASE_URL` 不生效；saas 模式为 validate 硬性必填，缺失启动失败）、`DEFAULT_AVATAR_URL`（必须以 `?seed=` 结尾）、`DEFAULT_TRAJECTORY_ICON`、`DEFAULT_COVER_IMAGE`。
- **Redis 键**：wechat:mp:access_token:{appID}（TTL 110m）、wxmp:msgid:{msgId}（60s）、wxmp:thumb_media_id（48h）、wxmp:profile:refresh:{openid}（1h）、purge:oss:pending（已删对象待刷新 URL 集合，容量护栏 10 万，无 TTL；满溢**弃新保旧**：`Record` 返回 `ErrQueueFull`，入队失败记日志 `record deleted url for purge failed`；上游恢复后队列以每轮 20×500 排空、存量最终可达；`CDN_REFRESH_ENABLED=1` 时启用，ADR-0013）。
- **Migration**：000001_baseline（files、user_avatar_markers、wx_mp_accounts 等）、000004_client_ops_logs；均含 down 脚本。
