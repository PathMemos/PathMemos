# PP-02B 日记（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0002（图片存储使用阿里云 OSS，已接受）
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。

## 1. 背景与目标

- 本域目标：为家庭用户提供按「日」组织的日记记录（手动/自动）、图文+位置表达、封面与时间线回顾、随手记与统计。
- 服务谁：小程序端登录用户；同家庭成员可查看同一家庭当日所有成员的日记卡片与条目。
- 本域边界：
  - 属于本域：日记卡片列表/详情、条目 CRUD、记忆（随手记）CRUD、整日删除、封面设置与降级、轨迹图封面生成、日记统计、自动记录成文入口。
  - 不属于本域：图片压缩与上传/配额（见 PP-02G）、文件存储与 URL 生成（PP-02G）、自动记录的后台定位采集与推送（PP-02G 与自动记录分册）、AI 对话、家庭邀请。
  - **天气**：当前代码中**没有任何天气字段/接口/展示**。
- 关键实现位置：backend/internal/diary/handler.go、backend/internal/diary/service.go、backend/internal/diary/avatar_marker.go、backend/internal/db/sqlc/diary.sql、backend/internal/db/sqlc/diary_entry.sql、backend/internal/db/sqlc/memory.sql、backend/migrations/000001_baseline.up.sql。

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | 日记以「虚拟 ID」寻址：family:{familyID}:date:{YYYY-MM-DD}，而非数据库 diaries.id。后端 parseVirtualID/makeVirtualID 解析与生成 | 家庭共享语义下，卡片/封面按「家庭+日期」聚合，天然对齐 family_daily_covers 主键 | 无 |
| D2 | 封面是「家庭+日期」级（family_daily_covers），条目图片是「个人+日期」级（diaries.user_id） | 封面代表全家当日展示图；条目按作者归属 | 无 |
| D3 | 封面降级优先级固定 manual > image > trajectory > default；default 的 URL 为空串（defaultCoverURL() 返回空字符串），前端渲染占位图 | 保证不白屏；避免后端持有默认图 URL | 无 |
| D4 | 封面刷新用 PostgreSQL advisory lock `lock:covers:{familyId}:{date}`（ADR-0005，无 TTL），且**轨迹图生成等外部 IO 必须在锁释放后**执行 | 纯 DB 临界区亚秒级完成；避免锁内做外部 HTTP/落盘。锁仅用于减少并发重复生成，封面正确性由 DB 元数据与幂等保证 | 无 |
| D5 | 创建/更新条目**同步**评估封面（纯 DB，manual→image→default），轨迹图在锁释放后异步 goroutine 生成；前端保存后轮询 GET /diary/cover-url | 保存后立即有封面；不阻塞响应 | 无 |
| D6 | 读路径（列表/详情）封面行缺失或 URL 失效时先做**无锁**评估并按 30s 去重节流异步触发完整刷新 | 读接口不去抢锁、不阻塞；封面行最终被后端持久化纠正 | 无 |
| D7 | 条目里的 address 由请求原样保存（手动新增由前端替换，自动记录由 autorecord 模块替换） | 职责分层 | 无 |
| D8 | 仅 VIP 可调用自动记录成文接口 | 自动记录属付费能力 | 无 |
| D9 | 图片仅在「日记条目引用 + 文件记录存在 + URL 可构造」三条件同时满足时才算有效封面；任一失效即降级 | 避免 stale 封面 | 无 |
| D10 | 条目图片关联写入用 BatchCreateDiaryEntryImages（unnest），封面清理用批量 ANY($1) UPDATE | 禁止事务内逐行循环 | 无 |
| D11 | 家庭共享视图按**当前**家庭成员实时聚合（`GetFamilyMembers` 取此刻的成员名单），不按「条目写入时所属家庭」归属；`diaries`/`diary_entries` 不存 `family_id` | 家庭视图 = 「当前成员 × 任意历史日期」的实时快照；无需在写入时冗余家庭归属，也无需在成员变动时回填历史 | 无 |

> **D11 的行为后果**：成员退出/被移除后，其此前贡献的条目从家庭其他成员的共享视图中消失（数据未删除，仅不再被聚合，本人仍在个人视图中可见）；新成员加入后，其加入前的个人历史日记会进入新家庭的共享时间轴。

## 3. 核心流程（用户故事 + 时序）

### D-1 首页时间线卡片列表

作为家庭成员，我打开首页要看到按日期倒序的日记卡片。

**时序**：GET /diary/info → handler 解析 cursorDate（默认 9999-12-31，格式非法 400）与 size（1–20，<1 取 10，>20 取 20）→ service GetFamilyMembers 取本人 current_family_id 与成员 → ListDiaryCards（record_date < cursorDate 去重后 LIMIT size）→ 批量取地址记录、封面行 → 逐日期 buildCard → JSONWithPagination。

**AC（可断言）**：
- AC1：cursorDate 传 abc 时 HTTP 400，code=4000。
- AC2：size=100 与 size=20 返回的卡片数上限一致（≤20）。
- AC3：当返回日期数 >= size 时响应 nextCursor 等于本页最后一个 recordDate；否则 nextCursor 为空。
- AC4：每个 card 含 id=family:{familyId}:date:{recordDate}、recordDate、coverImg、coverImage、detailCount、isMyDiaryInfo、myDiaryInfoId、memberBriefs、familyMemberAddressConcatRecords。

### D-2 按日期批量取卡片

作为健康/纪念页，我要一次取若干天的卡片。

**时序**：POST /diary/info/dates body {dates:[...]}（64KB）→ dates>31 400；空数组 200 空数组；逐个 normalizeDate（TrimSpace 后 2006-01-02，非法 400）；去重并 sort.Strings → ListDiaryCardsByDates（LIMIT 1000）→ buildCardsForDates → JSON。

**AC**：dates 含重复项时输出卡片按日期升序且不重复；含非法日期时 400 code=4000；超过 31 项 400。

### D-3 日记详情

作为用户，我进入某天详情看到该天条目、图片、封面与我的记忆。

**时序**：GET /diary/details?diaryId=&memberUserId=&page=&size= → parseVirtualID（非法 400）→ GetFamilyMembers 校验家庭（不匹配 403 code=4030）→ memberUserId 过滤（空=全家；self=本人；其他必须是成员，否则 404 code=4040）→ ListDiaryEntries（sort ASC, record_time ASC NULLS LAST, created_at ASC，offset/limit）→ 批量取图片路径 → entryToMap；记忆按 memoryUserID（指定成员时用成员，否则本人）ListMemoriesByUserAndDate → memoryToMap；封面 ResolveCoverImage → `JSONWithExtraAndCount`(data=entries, extra={coverImg,coverImage,memories}, count)。

**AC**：
- size 默认 20、上限 100；page 默认 1。
- 响应 data 为条目数组（记忆由前端另行合并，见 §7）。
- 响应 extra.coverImg 始终存在（可为空串）；extra.coverImage 为 fileId（可空）。
- **注意**：service GetDetails 计算的 count（该天条目总数）已由 handler 透传（`JSONWithExtraAndCount`），HTTP 响应含 count 字段。
- **已接受取舍**：offset 分页在并发增删下可能重复/漏条；单日条目量小、变更窗口短，不改 cursor。

### D-4 手动新增条目

作为用户，我要写文字、传图片、选位置和时间。

**时序**：POST /diary/details body entryRequest → 校验（见 AC）→ resolveRecordDateAndTime（recordTime 缺省用上海当前时间，record_date 取 recordTime 的上海日期）→ validateImageFiles（存在/image 类型/属于本人）→ db.WithTx：UpsertDiary + CreateDiaryEntry + BatchCreateDiaryEntryImages + TouchDiaryUpdatedAt → 返回 {id, card?} → 异步失效 MCP 缓存 → **同步** RefreshFamilyDailyCover（失败仅 warn）→ ListInfoCardsByDates 返回 card。

**AC**：
- body >64KB → 413 code=4130；JSON 非法 → 400。
- imageIds >9 → 400 code=4000（maxImagesPerEntry=9）。
- text >10000 code points → 400 code=4000 + biz_code=TEXT_TOO_LONG。
- color 非 validator.ValidateColor 合法值 → 400 code=4000 + biz_code=INVALID_COLOR_FORMAT；空串与缺省一致视为不设置（创建/更新均映射为 NULL）。
- lat/lon 只传其一或越界 → 400 code=4000 + biz_code=INVALID_COORDINATES。
- address/detailAddress >500 runes → 400 code=4000。
- recordTime 非 RFC3339 → 400 code=4000。
- 图片不存在 → 400 code=4000；非本人图片 → 400 code=4000；非 image → 400 code=4000。
- 成功返回 {id} 且可选 card；card.coverImg 为本轮刷新后的值。

### D-5 编辑条目

作为作者，我要修改自己的条目。

**时序**：PUT /diary/details（id 必填，空 400）→ 取条目/日记，非作者 403 code=4030；取家庭；校验图片 → 计算 removedImageIDs（旧关联中不在新集合的）→ 若改了 recordTime 且上海日期 ≠ 日记日期 → ErrRecordTimeCrossDay 400 code=4000 → db.WithTx：UpdateDiaryEntry（0 行 → 404）、DeleteDiaryEntryImages、批量写新关联、批量清理被移除图片的封面引用、TouchDiaryUpdatedAt → 异步失效 MCP 缓存 → **同步**刷新封面 → 异步物理删除被移除且无其它引用的文件（safe.Go 2 分钟）→ 返回 {card?}；物理删除与 files 行删除、图片配额回退（`DecrementUserImageStorage`）同一事务，扣减失败整体回滚。

**AC**：跨天改期返回 400 code=4000；条目不存在 404 code=4040；非作者 403 code=4030；被移除图片的封面引用被批量清空。

### D-6 删除条目

**时序**：DELETE /diary/details?id= → id 空 400 code=4000 → 取条目/日记（不存在 404 code=4040，非作者 403 code=4030）→ db.WithTx：DeleteDiaryEntryImagesReturningFileIDs、批量清封面引用、DeleteDiaryEntryByID、TouchDiaryUpdatedAt；若条目数=0 且当日记忆数=0 则删除空日记 → 异步刷新封面（safe.Go 30s）+ 异步物理删除图片（safe.Go 2 分钟）+ **同步**失效该用户 MCP 缓存（`mcp.InvalidateUserCache`；创建/更新路径为 safe.Go 异步，删除路径为同步）+ 同步失效家庭 AI 汇总缓存（`InvalidateFamilySummary`；条目增/改/删后 handler 均调用）；物理删除与 files 行删除、图片配额回退（`DecrementUserImageStorage`）同一事务，扣减失败整体回滚。

**AC**：删除最后一条条目且当日无记忆时 diaries 行被删除；有记忆时不删除。

### D-7 自动记录成文（VIP）

**时序**：POST /diary/details/auto body {lat,lon} → vipService.GetVIPInfo，非 VIP 返回 HTTP 403 + code=4030 + biz_code=NOT_VIP→ 坐标校验（非法 400 code=4000）→ 日配额 location.CheckReverseQuota（每用户每日 200 次；Redis 不可用 fail-closed；超限 429 code=4290 + biz_code=RATE_LIMITED）→ 腾讯逆地理编码（失败 500 code=5001）→ record_date 为上海当日，record_time=now → 同一天内与全部自动条目的地址并集集合判重（与后台共用同一语义：landmark/detail_address 命中集合任一成员即重复，返回已有 entryID——首条命中即最新同址条目，不新建）→ db.WithTx：UpsertDiary + CreateDiaryEntry（text=（自动记录）、address=landmark、detail_address=address（空时不写，与后台自动记录一致））→ 异步 refreshCoverAsync + safe.Go 新地点推送（SendNewPlaceAlert）→ 返回 {id}。

**AC**：非 VIP 403 code=4030 biz_code=NOT_VIP；超配额 429 code=4290 biz_code=RATE_LIMITED；同地点重复调用返回同一 entryID 且不新增行；条目 text 精确等于（自动记录）；无 redis 时直接 429。

### D-8 记忆（随手记）CRUD

- POST /diary/details/memory body {title,content,recordTime}（64KB）：title trim 后 1–50 runes（否则 400 code=4000）、content trim 后 ≤10000（否则 400）、recordTime RFC3339Nano（否则 400）→ 生成 UUID、record_date 取上海日期 → db.WithTx：CreateMemory + UpsertDiary → 异步失效 MCP 缓存 → {id}。
- PUT /diary/details/memory body {id,title,content,recordTime}（64KB）：id 空 400；GetMemory 不存在 → 404 code=4040；db.WithTx：UpdateMemory + UpsertDiary（新日期）+ 若日期变化则查旧日记，旧日记条目数与记忆数均为 0 时 DeleteDiaryByID → 异步失效 MCP 缓存。
- DELETE /diary/details/memory?id=：id 空 400；GetMemory 不存在 404；db.WithTx：DeleteMemory（0 行 → 404）+ 该日记条目数与当日记忆数均为 0 则删除空日记 → 异步失效 MCP 缓存。

**AC**：title 为纯空白 400；改期会产生新日期日记行并清理旧空日记；删除不存在的 memory 404 code=4040。

### D-9 设置/清除手动封面

**时序**：PUT /diary/info body {id, coverImage}（64KB）→ parseVirtualID（非法 400）→ UpdateCover：
- 校验家庭归属（不匹配 403 code=4030）；
- 抢锁 lock:covers:{familyID}:{date}（PG advisory lock，ADR-0005，无 TTL），抢不到 → OPERATION_IN_PROGRESS（HTTP 429）；
- coverImage 非空：文件存在（否则 400 4000）、file_type=image（否则 400）、storage.URL 可构造（否则 error）、IsImageUsedByFamilyDate 为真（否则 ErrCoverImageNotFromDiary 400）；写 SetFamilyDailyManualCover；收集旧轨迹图文件在锁释放后物理删除；
- coverImage 空/NULL：ClearFamilyDailyManualCover → 锁内降级评估：有可用图片写 image；否则无定位点写 default，有定位点写 default 并置 needAsyncRefresh（锁释放后 safe.Go 30s 调 RefreshFamilyDailyCover 生成轨迹图）。

**AC**：设置不属于当日家庭日记的图片 → 400（ErrCoverImageNotFromDiary）；锁冲突 → 429 code=4290 biz_code=OPERATION_IN_PROGRESS；清除后无图无定位点 → cover_type=default。

### D-10 删除整日日记

**时序**：DELETE /diary/info?id= → parseVirtualID（非法 400）→ DeleteDiary：校验家庭（403）→ 取本人该日期日记（不存在直接返回成功）→ db.WithTx：删除本人当日 memories、DeleteDiaryImagesReturningFileIDs、批量清手动/图片封面引用、DeleteDiaryByID（级联删除该用户当日 entries）→ 异步失效 MCP 缓存 → 异步物理删除文件（2 分钟）→ 异步刷新家庭当日封面（30s）→ 记录 OPS-LOG diary deleted；物理删除与 files 行删除、图片配额回退（`DecrementUserImageStorage`）同一事务，扣减失败整体回滚。handler 成功后 InvalidateFamilySummary。

**AC**：只删除当前用户在该日期的日记/记忆，不影响其他成员同日日记；家庭不匹配 403；id 非法 400。

### D-11 封面降级（核心不变量）

优先级 manual > image > trajectory > default。

**manual 有效条件**（refreshFamilyDailyCover/evaluateCoverForDate）：图片仍被当日家庭条目引用 + 文件记录存在 + storage.URL 可构造；任一失败清 manual_cover_file_id 并继续降级。

**image**：GetLatestImageEntry 排序 sort_order ASC, record_time DESC NULLS LAST, created_at DESC；取到后校验 URL 可构造；否则降级。

**trajectory**：存在定位点 entry 时生成腾讯静态地图并写文件；无定位点直接 default。**default**：cover_type=default 且 URL 为空串。

**AC**：cover_type=manual 但图片已不被引用 → 刷新后 cover_type != manual；无图片无定位点 → coverImg=空串；有定位点先写 default 再异步升级 trajectory。

### D-12 封面异步生成与前端轮询

- 写路径（创建/更新/删除条目、autorecord）触发 refreshCoverAsync：Redis SETNX `lock:covers:refresh:{familyID}:{date}` TTL 30s 作**去重节流标记**（非互斥锁，与 `ai:turn`/`wxmp:msgid` 同类保留；Redis 不可用时仅告警并继续，符合 I2），命中则直接返回；未命中 safe.Go 调 RefreshFamilyDailyCover，互斥由其内部 `lock:covers:{familyID}:{date}` advisory lock 承担。
- RefreshFamilyDailyCover（withTrajectory=true）锁内只评估并写 default；锁释放后（defer）safe.Go（context.Background() 30s）生成轨迹图并 upsert trajectory；若生成期间用户已设 manual，则跳过 trajectory upsert。
- 前端保存后轮询 GET /diary/cover-url，每 500ms，最多 12 次（约 6 秒）；封面变化（含变空串）即停止。

**AC**：GET /diary/cover-url 缺 familyId/recordDate → 400 code=4000；非本家庭成员 → 403 code=4030；无封面行 → 200 {coverImg:空串}；DB 错误 → 500 code=5001。

### D-13 日记统计

GET /diary/stats：取家庭成员 → 上海当前时间所在周（周一为周起点）→ 返回 {firstRecordDate, recordDays, totalEntries, weeklyEntries}。

**AC**：无任何记录时 firstRecordDate 为空串、recordDays=0；recordDays 为「今天 − 首条记录日 + 1」（首条日期早于 1970 时按无记录处理）。

## 4. 数据模型

表定义对应迁移：backend/migrations/000001_baseline.up.sql（与本文须一致，由 `scripts/spec-check.sh` 校验）。

| 表 | 关键字段 | 说明 |
|----|---------|------|
| diaries | id text PK、user_id text NOT NULL、record_date date NOT NULL、created_at/updated_at timestamptz；UNIQUE(user_id,record_date)；FK user_id → users ON DELETE CASCADE | 每人每日一行日记头；UpsertDiary 依赖唯一键 |
| diary_entries | id PK、diary_id FK→diaries ON DELETE CASCADE、created_by FK→users ON DELETE CASCADE、text text、lat/lon numeric(10,7)、address/detail_address text、record_time timestamptz、sort int DEFAULT 0、color text、created_at/updated_at | CHECK：address/detail_address ≤500 字符；lat/lon 成对；lat∈[-90,90]、lon∈[-180,180]；color 匹配 #(3/6/8 位十六进制) |
| diary_entry_images | id PK、diary_entry_id FK→diary_entries ON DELETE CASCADE、file_id FK→files(id)（无 CASCADE）、sort_order int DEFAULT 0、created_at；UNIQUE(diary_entry_id,file_id) | 条目-文件关联；索引 entry_id/file_id |
| memories | id PK、user_id FK→users ON DELETE CASCADE、record_time timestamptz NOT NULL、record_date date NOT NULL、title text NOT NULL、content text NOT NULL、created_at | 无 updated_at；查询覆盖索引 (user_id, record_date DESC, record_time DESC) INCLUDE(title,content) |
| family_daily_covers | family_id text（FK→families ON DELETE CASCADE，000009）、record_date date、复合 PK(family_id,record_date)、cover_file_id FK→files ON DELETE SET NULL、cover_type text DEFAULT default、manual_cover_file_id FK→files ON DELETE SET NULL、updated_at | CHECK cover_type ∈ {image,trajectory,default,manual}；触发器 trg_family_daily_covers_fix_type：manual_cover_file_id IS NULL 且 cover_type=manual → default，cover_file_id IS NULL 且 cover_type∈{image,trajectory} → default |
| files | 见 PP-02G | 封面/图片/轨迹图/头像 marker 共用 |
| users | current_family_id、avatar_file_id 等 | 提供家庭与头像信息 |


## 5. API 契约

统一响应包：{code, biz_code?, message, data?, extra?, count?, nextCursor?, request_id}（backend/internal/middleware/response.go）。成功 code=0000。

| 方法 | 路径 | 鉴权 | 请求 | 成功响应 | 主要错误 |
|------|------|------|------|---------|---------|
| GET | /diary/info | Session | cursorDate(默认 9999-12-31)、size(1–20，默认 10) | data=[card]、count、nextCursor | 400 4000；500 5001 |
| POST | /diary/info/dates | Session | {dates:[YYYY-MM-DD]}（≤31，64KB） | data=[card] | 400 4000；500 5001 |
| GET | /diary/stats | Session | — | {firstRecordDate,recordDays,totalEntries,weeklyEntries} | 500 5001 |
| PUT | /diary/info | Session | {id:family:..:date:.., coverImage?:fileId}（64KB） | {} | 400 4000；403 4030；404 4040；429 OPERATION_IN_PROGRESS；500 5001 |
| DELETE | /diary/info?id= | Session | query id | {} | 400 4000；403 4030；500 5001 |
| GET | /diary/details | Session | diaryId、memberUserId?、page(默认 1)、size(默认 20，≤100) | data=[entry]、count=条目总数、extra={coverImg,coverImage,memories} | 400 4000；403 4030；404 4040；500 5001 |
| POST | /diary/details | Session | entryRequest（64KB） | {id, card?} | 413 4130（超限）；400 code=4000（biz_code=TEXT_TOO_LONG/INVALID_COLOR_FORMAT/INVALID_COORDINATES）；500 5001 |
| PUT | /diary/details | Session | entryRequest 含 id（64KB） | {card?} | 400 4000；403 4030；404 4040；500 5001 |
| DELETE | /diary/details?id= | Session | query id | {} | 400 4000；403 4030；404 4040；500 5001 |
| POST | /diary/details/auto | Session + VIP | {lat,lon}（64KB） | {id} | 400 4000；403 4030 NOT_VIP；429 4290 RATE_LIMITED / OPERATION_IN_PROGRESS（后台锁冲突，见 02c §5.1）；500 5001 |
| POST | /diary/details/memory | Session | {title,content,recordTime}（64KB） | {id} | 400 4000；500 5001 |
| PUT | /diary/details/memory | Session | {id,title,content,recordTime}（64KB） | {} | 400 4000；404 4040；500 5001 |
| DELETE | /diary/details/memory?id= | Session | query id | {} | 400 4000；404 4040；500 5001 |
| GET | /diary/cover-url | Session | query familyId、recordDate | {coverImg} | 400 4000；403 4030；500 5001 |

**虚拟 ID**：family:{familyId}:date:{YYYY-MM-DD}；parseVirtualID 要求 4 段且 parts[0]==family、parts[2]==date、日期合法，否则 400。

**entryRequest 字段**：id, text, imageIds[], lat, lon, address, detailAddress, recordTime, sort, color。text/lat/lon/address/detailAddress/recordTime/sort/color 均为可选指针，缺省不修改（更新时）。更新时 recordTime 传空串会置 NULL。

**card 字段**：id, recordDate, coverImg, coverImage, detailCount, isMyDiaryInfo, myDiaryInfoId, memberBriefs[{userId,avatarUrl,nickName,entryCount}], familyMemberAddressConcatRecords[{familyMemberAvatar,familyMemberAddressConcat}]。

**entry 字段**（entryToMap）：id, recordText, recordImages[{id,filePath}], diaryLat, diaryLon, diaryAddress, detailAddr, recordTime(上海 2006-01-02 15:04:05), sort, color, familyMemberUserId, familyMemberAvatarUrl, familyMemberNickName, createdAt(RFC3339), updatedAt(RFC3339)。记忆项额外 source=memory、color=#543116、recordImages=[]、recordTime 来自 record_time。

## 6. 关键实现约束

- **锁**：lock:covers:{familyID}:{recordDate}，PostgreSQL advisory lock（ADR-0005，无 TTL）；锁失败立即返回 ErrCoverUpdateInProgress（429 OPERATION_IN_PROGRESS），不等待。defer Unlock 用 context.WithoutCancel(ctx)。
- **异步去重**：lock:covers:refresh:{familyID}:{recordDate}，Redis SETNX 30s 去重节流标记（非互斥锁；Redis 故障仅失去节流，不影响正确性）；读路径封面失效时先无锁评估，再异步完整刷新。
- **事务**：条目/记忆/日记的创建、更新、删除均在 db.WithTx 内；DB 错误统一 fmt.Errorf(操作名: %w, err)；业务 sentinel error 裸返回。
- **后台任务**：封面异步刷新、轨迹图生成、物理文件删除、MCP 缓存失效、新地点推送均经 safe.Go（panic 记录日志），带 30s/2min 超时。
- **批量**：图片关联用 BatchCreateDiaryEntryImages（unnest）；封面引用清理用 ANY($1) 批量 UPDATE。
- **错误码映射**（writeDiaryServiceError；code 固定枚举 + biz_code 语义码，见 03-api §3）：
  - ErrEntryNotFound → 404 4040
  - ErrFileNotFound/ErrNotFileOwner/ErrFileNotImage/ErrRecordTimeCrossDay/ErrCoverImageNotFromDiary → 400 4000
  - 校验类 sentinel（text 超长/颜色/坐标）→ 400 4000 + 对应 biz_code（TEXT_TOO_LONG/INVALID_COLOR_FORMAT/INVALID_COORDINATES）
  - ErrFamilyMismatch → 403 4030
  - ErrPermissionDenied → 403 4030
  - ErrCoverUpdateInProgress → 429 4290 + OPERATION_IN_PROGRESS
  - 其它 → 500 5001（不暴露内部细节）
- **阈值**：body 64KB；maxImagesPerEntry=9；text ≤10000 code points；address/detailAddress ≤500 runes；memory title 1–50 runes、content ≤10000；list size 1–20；dates ≤31；details size ≤100；refresh 去重 30s（advisory lock 无 TTL）；逆地理配额 200/日；轨迹 marker 每用户 ≤30、全局 ≤80；静态地图 URL 估算上限 8000；腾讯静态图响应读取上限 10MB。
- **封面 URL**：defaultCoverURL() 返回空串；storage.URL 的校验仅「可构造 URL」，不含 HTTP 可达性探测。
- **OPS-LOG**：创建/更新/删除/自动记录、删除整日日记写 slog.InfoContext 审计日志（diary entry created/updated/deleted、diary auto entry created、diary deleted），用于数据复盘。

## 7. 前端接入

| 页面/组件 | 行为 |
|-----------|------|
| pages/index/index.ts | GET /diary/info（data/count/nextCursor）、GET /diary/stats；从详情返回时按 globalData._pendingCoverPoll 调 _pollCoverForDate：GET /diary/cover-url，500ms 一次、最多 12 次、封面变化即停；封面空串回退占位图 image/default-bg.png |
| pages/NoteDetail/NoteDetail.ts | GET /diary/details（data 与 extra.coverImg/coverImage/memories）；_normalizeEntry 生成 dotColor 与 recordImages；条目与记忆由 _mergedFullList 合并排序；reloadAfterEdit 后启动 _startCoverPolling（同 500ms×12）并设置 _pendingCoverPoll（10s 兜底清理） |
| components/NoteEdit/NoteEdit.ts | 保存时先 request.uploadFile 上传新增图片，再 POST/PUT /diary/details（recordTime=toISOString()）；配额超限 USER_IMAGE_STORAGE_LIMIT_EXCEEDED 弹升级 |
| components/CoverEdit/CoverEdit.ts | PUT /diary/info 提交 {id, coverImage}；未选图片直接关闭不发送 |
| components/MemoryEdit/MemoryEdit.ts | POST/PUT /diary/details/memory；前端校验 title 1–50、content ≤10000 |
| components/RecordItem/RecordItem.ts、components/NoteItem/NoteItem.ts、components/MemoryItem/MemoryItem.ts | DELETE /diary/details、DELETE /diary/info、DELETE /diary/details/memory |
| utils/http.ts | 统一封装 request.get/post/put/del、uploadFile、updateAvatar；携带 Authorization: Bearer <sessionId> 与 Accept-Language |

## 8. 运维与任务

- **后台任务**（backend/internal/jobs/runner.go）：孤儿文件清理（默认 7 天周期）、旧系统文件清理（轨迹图 7 天）、自动记录（5 分钟）、异常告警（5 分钟）、公共地址汇总（每日 03:00）、AI 日志清理（>90 天）、订单关闭、客户端日志清理（>30 天）、轨迹清理（默认 6 小时）、已删对象 CDN 刷新（默认 24 小时，`CDN_REFRESH_ENABLED=1` 才启用）。任务用 PostgreSQL advisory lock 互斥，任务自身幂等。
- 并发换头像 / 并发封面刷新可能产生孤儿轨迹文件，由 7 天清理任务回收。
- **Redis 键**：`lock:covers:refresh:{family}:{date}`（SETNX 30s 去重节流标记）、location:reverse:{user}:{date}、ai:family_summary:{family}（InvalidateFamilySummary）。
- **锁**：`lock:covers:{family}:{date}` 为 PostgreSQL advisory lock（ADR-0005），不是 Redis 键。
- **环境变量**：STORAGE_LOCAL_PATH（默认 /opt/pathmemos/uploads）、TENCENT_MAP_KEY（可多值，backend/internal/config/config.go）、OSS_*、JOB_INTERVAL_*。
- **轨迹图外部依赖**：腾讯静态地图 https://apis.map.qq.com/ws/staticmap/v2/；应用内限流 limiter.WaitStaticMap。
- **Migration**：本域相关为 000001_baseline、000002_fix_cover_trigger_preserve_file_id（封面触发器保留 cover_file_id）、000009_family_covers_fk（`family_daily_covers.family_id` 补外键）；当前最大编号 000011_user_invites_inviter_set_null；均含 .down.sql。
