# PP-02C 自动记录（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：<无>
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。
> 关键实现位置：`backend/internal/autorecord`、`backend/internal/location`、`backend/internal/jobs`、`backend/internal/redis`、`backend/internal/db/sqlc/auto_record.sql` 等。

## 1. 背景与目标

- 本域目标：让小程序在用户后台/前台自动采集停留点，服务端把轨迹点聚合成「停留地点」，经逆地理编码成文为日记条目，无需用户手动输入。
- 服务谁：已开启自动记录的 VIP 用户（VIP 判定见 §6）。
- 本域边界（做）：
  - 轨迹点采集策略（前端）、批量上报、入库、驻留点聚类、逆地理编码、常用地址替换、同点去重、自动成文。
  - 「新地点提醒」与「异常（失联）告警」两条推送链路的触发判定（推送通道本身属 02g）。
  - 常用地址摘要（后台汇总）与轨迹清理后台任务。
  - 本域使用的 Redis key、降级策略与锁（PG advisory lock）。
- 明确不属于本域：
  - 日记 CRUD / 封面 / 图片（02b）；VIP 付费与权益（02e）；文件与推送模板（02g）。
  - 腾讯地图账号配额采购、POI 分类精度。

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | 采集在前端、聚合在服务端：前端只识别「驻留点」（连续静止 ≥10 分钟且质心 300m 内），后端按 300m/30 分钟再聚类，聚类结果为直接成文单位 | 降低上报量和后端计算量；前端可省电 | 无 |
| D2 | 逆地理编码按「用户 + 自然日」配额 200 次，Redis Lua 原子计数 | 防单个用户耗尽腾讯地图 key 影响全站交互接口 | 无 |
| D3 | 常用地址命中时跳过逆地理编码；未命中才调用腾讯 API | 省配额与延迟；地图不可用时常用地址仍能成文 | 无 |
| D4 | 同点去重与「当天全部自动条目的地址并集集合」比较（`autoAddressSet` 把每条自动条目的 address 与 detail_address 非空值并入同一集合；候选 landmark 或 detail_address 命中集合任一成员即视为重复——landmark 可命中历史条目的 detail_address，反之亦然；`ListAutoEntryAddressesByDate` 一次取集合，成文后并入内存集合） | 旧「只比最后一条」无法识别同日折返旧地点（家→公司→家）导致重复成文；并集成员判断实现最简（一个 set） | 无 |
| D5 | 后台处理任务与用户级锁相互独立：后台 `lock:background:auto_record` 与用户级 `lock:auto_record:{userID}` 均为 PostgreSQL advisory lock（ADR-0005，无 TTL/续期） | 防多实例重复处理；连接断开自动释放，任务自身幂等 | 无 |
| D6 | 逆地理失败保留轨迹点，不立即删除；达到 `maxGeocodeAttempts`(10) 后进入终态（`geocode_attempts >= 10`，SQL 排除），由 7 天清理窗口统一回收 | 容忍暂时性上游故障，避免数据永久丢失；同时防止脏轨迹每轮空转 | 无 |
| D7 | 配额校验与轨迹处理均 fail-closed（Redis 不可用 = 拒绝/跳过） | 保核心库稳定，宁可漏记不耗尽外部配额 | 无 |
| D8 | 自动记录为「尽力而为」：定位采集、逆地理编码、推送、封面刷新、缓存失效等环节失败只记日志；**成文事务失败不删轨迹**（轨迹保留，下轮自然重试，数据不丢）；轮次存在用户级失败时输出告警关键字 `alert=auto_record_failed`（alert-watch 捕获）。全部失败**不得影响手动日记与其他核心业务** | 符合 AGENTS.md「简单优先、容忍小概率异常」；数据路径靠轨迹保留兜底重试，可见性靠告警 | 无 |
| D9 | 轨迹上报幂等以 DB 自然键 `(user_id, recorded_at, lat, lon)` 唯一索引 + `ON CONFLICT DO NOTHING` 实现；前端 `batchSeq` 仅入日志用于观测 | 幂等由 DB 唯一索引保证；重试整批重传不产生重复轨迹点 | 无 |
| D10 | 候选用户按 `user_id` keyset 公平轮转（游标 Redis `job:cursor:auto_record`） | 防止用户数扩展后低频用户饥饿；游标丢失仅从头重扫，不影响正确性（I2） | 无 |
| D11 | 逆地理达到 `maxGeocodeAttempts` 的轨迹为终态：候选 EXISTS 与逐用户查询均排除，跨过上限时记一次 `geocode_discarded` | 终态不再参与聚类与告警，仍保留至 7 天清理，不物理删除 | 无 |

## 3. 核心流程（用户故事 + 时序）

> 编号稳定；验收标准（AC）均可直接转成断言。

### AR-1 开启/关闭自动记录
- 故事：VIP 用户在小程序首页打开「自动记录」开关。
- AC：
  1. 满足 `getVipInfo().isVip > 0` 后，前端依次执行 `PUT /auto-record/config {enabled:true}` → `wx.startLocationUpdateBackground` + `wx.onLocationChange`，全部成功后本地 storage 置 on（`frontend/miniapp/miniprogram/utils/autoRecord.ts`）。
  2. 后端在 `enabled=true` 时**严格校验 VIP**（`expire_time > 上海当前时间`，**无宽限期**），非 VIP 返回 HTTP 403、`code=4030`、`biz_code=NOT_VIP`。
  3. 关闭时先上报积压驻留点再 `PUT /auto-record/config {enabled:false}`（顺序不可颠倒）；上报失败（含非 VIP 403 `NOT_VIP`）**不阻断关闭**，最后一批轨迹放弃（`closeAutoRecord` 吞上报错误后仍关后端开关，前后端状态保持一致）。
  4. 开启流程整体 30 秒超时；超时后查询后端真实 `enabled`，若后端已开启则保留本地开启并立即尝试恢复监听。

### AR-2 轨迹采集（前端，`autoRecord.ts`）
- 故事：小程序按定位回调累积点，识别静止后产出驻留点。
- AC：
  1. 定位回调先报活心跳（每 30 分钟一次），**在 VIP 检查之前**执行。
  2. 精度过滤：iOS `accuracy > 3000`、其它平台 `accuracy > 500` 的点丢弃。
  3. 质心窗口：前台 60 秒/最少 3 点/最多 200 点；后台 600 秒/最少 2 点/最多 50 点。
  4. 静止判定：窗口质心与稳定质心平面距离平方 < 300m²；离开需连续 2 次越界确认。
  5. 驻留成熟：静止持续 ≥ 10 分钟后生成 1 个驻留点，坐标为 4 位小数四舍五入。
  6. 驻留点写入本地 storage（`papafeiji:autoRecordStayPoints`），上限 50，超出丢弃最旧。

### AR-3 轨迹上报与入库
- 故事：驻留点批量上报服务端。
- AC：
  1. 前端以单飞链 `_serialFlushAndReport` 串行执行「落 storage → 上报」；上报请求 `POST /auto-record/trajectories`，超时 10s。
  2. 上报失败指数退避：`30s × 2^(n-1)`，上限 5 分钟；token 过期时先静默重登再重试 1 次。
  3. 上报成功而清空 storage 失败时，用 `_reportedLeadingCount` 记录已上报前缀，后续只清空不重报。
  4. 后端一次最多 50 点（`maxBatchPoints`），body 最多 64 KiB（超限 413 `code=4130`），非 VIP 返回 403 `code=4030` + `biz_code=NOT_VIP`。
  5. 每点 `lat` / `lon` / `recordedAt`（RFC3339）必填；经纬度越界返回 400；缺字段返回 400。
  6. 入库 `auto_record_trajectories`，每次上传请求执行一条 `INSERT ... SELECT unnest(...)` 批量语句（`InsertTrajectories`，整批一次入库而非每点一条），`geocode_attempts=0`，`ON CONFLICT DO NOTHING`（唯一索引 `uq_auto_record_trajectories_point`，迁移 000005）；每次上传都 best-effort 刷新 `users.last_active_at`。
  7. 前端 `batchSeq` 被解析并随上传日志记录（`batch_seq`），仅用于观测；幂等不依赖它。

### AR-4 驻留点聚类（`autorecord/service.go mergeStayPoints`）
- 故事：后台任务把轨迹点合并为停留点。
- AC：
  1. 一轮处理 `ListTrajectoriesByUser` 取该用户 `recorded_at ASC LIMIT 100`。
  2. 连续点「与上一个点的平面距离 ≤ 300m」且「与聚类最后一点时间差 ≤ 30 分钟」则并入同一 cluster。
  3. cluster 代表坐标为全部成员的算术平均质心（7 位小数）；代表时间为成员中最大 `recorded_at`。
  4. 坐标 NaN 的点收入 invalid 集合被删除。
  5. 无任何 cluster 时删除本轮全部轨迹点。
  6. cluster 的 `record_date` 取代表时间（成员中最大 `recorded_at`）的上海日期，跨午夜时归入次日。
  7. 候选用户由 `ListAutoRecordCandidates` 按 `user_id ASC` keyset 游标取前 100（`maxPendingPerUser`）；游标存 Redis `job:cursor:auto_record`；**仅当本轮候选取满 100（满批）时**才统计候选总数，>1000 记 `auto_record_backlog_warn`；候选仅含 VIP 未过期用户（`JOIN user_vips` 且 `expire_time > now()`），VIP 过期用户的存量轨迹不再进入聚类成文（保留至 7 天清理）。

### AR-5 逆地理编码与地址生成
- 故事：把 cluster 坐标翻译为地址。
- AC：
  1. 先在该用户常用地址中按 300m 匹配；命中则 `landmark = 常用地址名`，**跳过**腾讯 API。
  2. 未命中则检查日配额；配额耗尽（或 Redis 不可用）跳过该 cluster，**不累加** `geocode_attempts`，轨迹保留。
  3. `location.Client.Reverse`：多 key 原子轮询，每次请求前全局限速 `WaitGeoCoder`（10 RPS token bucket），单次超时 3s，失败重试 1 次（切换 key、等待 100ms）。
  4. 地址取值：`address = result.address`；`detailAddress` 优先 `formatted_addresses.recommend` → `rough` → `title`；`landmark` 优先 `title` → 第一个 POI 标题 → `detail`；autorecord 层再兜底：`landmark` 仍为空时回退 `address`。
  5. 上游返回 error 时跳过该 cluster，对 cluster 内所有点统一 `geocode_attempts + 1`（`handleGeocodeRetry` 不做上限判断；终态由查询层 `geocode_attempts < maxGeocodeAttempts` 排除，跨过上限那一次记 `geocode_discarded`）；**不删除轨迹**。
  6. 空地址且空 landmark 时走与 5 相同的重试逻辑，仍保留轨迹。

### AR-6 常用地址替换
- 故事：用户把某地址命名为「家」，命中时成文直接用该名称。
- AC：
  1. 处理每个用户前一次性查询 `user_common_addresses`（最多 10 条，`count DESC, updated_at DESC`），内存复用。
  2. 匹配命中时 `diary_entries.address = 常用地址名`（即 `landmark`），并跳过逆地理编码：`detail_address` 不写（NULL），`lat`/`lon` 为 cluster 质心（与未命中路径一致）。
  3. 多条命中取列表中靠前者（即 `count` 最高者）。

### AR-7 同点去重
- 故事：同一地点一天内只生成一条自动记录。
- AC：
  1. 按 cluster 日（`recorded_at` 的上海时区日期，格式 `2006-01-02`）查询该用户当天全部 `text='（自动记录）'` 条目的地址集合（`ListAutoEntryAddressesByDate`，每日期缓存一次；landmark/detail_address 非空值并入集合）。
  2. 候选的 `landmark` 与 `address` 分别对「当天全部自动条目的 address + detail_address 非空值」集合判重（`isDuplicateAutoAddress`：命中集合任一成员即重复，landmark 可命中历史条目的 detail_address，反之亦然）→ 判重，删除本 cluster 全部轨迹，不成文。
  3. 当天无自动记录时用零值哨兵缓存，不重复查库；成文成功后把新条目写回当天缓存，供后续 cluster 比较。
  4. `POST /diary/details/auto`（手动首次成文）与后台共用同一集合判重（`ListAutoEntryAddressesByDate` + `FindDuplicateAutoEntry`，评审定稿统一）；判重时直接返回已存在条目 id（响应契约不变，ORDER BY record_time DESC 保证返回最新同址条目）。

### AR-8 自动成文（后台）
- 故事：cluster 生成一条自动日记条目。
- AC：
  1. 事务内 `UpsertDiary`（`diaries(user_id, record_date)` 唯一）+ `CreateAutoRecordEntry`。
  2. 条目字段：`text='（自动记录）'`，`address=landmark`（landmark 为空时仍写空串且 Valid=true），`detail_address=address`（空时不写），`lat/lon`=质心，`record_time`=cluster 最大时间；`sort=0`、`color=NULL`。
  3. 成功后删除本 cluster 轨迹，并异步触发封面刷新 + 新地点提醒；事务失败时轨迹保留，等待下一轮重试。
  4. 完成后（有新建日期且用户有 current_family_id）异步刷新家庭日封面，再删除 `ai:family_summary:{familyID}` 缓存；该异步任务在用户锁释放后才启动。
  5. 单轮处理结果打 OPS-LOG（`trajectories` / `clusters` / `entries_created` / `deleted`）。
  6. 同日多 cluster 复用同一条 `diaries` 记录，同一条日记可同时包含手动条目与自动条目。

### AR-9 首次即时成文
- 故事：开启自动记录后立即用当前定位生成一条自动记录（不等后台）。
- AC：
  1. 前端 `_saveFirstRecord`：`wx.getLocation`（10s 超时）→ `POST /diary/details/auto {lat,lon}`（4 位小数）。
  2. 后端校验 VIP（严格，无宽限期）→ 校验坐标 → 配额检查（失败返回 429 `code=4290,biz_code=RATE_LIMITED`）→ 逆地理 → 去重 → 事务成文。
  3. 非 VIP 返回 403，`code=4030`、`biz_code=NOT_VIP`。
  4. 成功后失效 `ai:family_summary:{familyID}` 缓存并异步推送新地点提醒。

### AR-10 新地点提醒
- 故事：每次生成新地点后给服务号订阅用户推送。
- AC：
  1. 仅服务号通道；仅 6:00–23:00（上海时区）内发送。
  2. 要求 `wx_mp_accounts.subscribed=true` 且 `mp_openid` 非空，否则跳过。
  3. 页面路径 `pages/NoteDetail/NoteDetail?baseInfo=<JSON>`；地点名取 `address`，为空则 `detail_address`。
  4. 发送失败只记日志；微信 `code=43004` 时将订阅状态置 false。

### AR-11 异常（失联）告警
- 故事：开启了自动记录但长时间无轨迹/无报活的 VIP 用户，收到「异常」提醒。
- AC：
  1. 候选条件（`ListAbnormalAlertCandidates`）：`auto_record_enabled=true`、VIP 未过期（`v.expire_time > now()`，严格）、`abnormal_alert_sent_at` 为空或早于 1 小时前、`last_active_at` 为空或早于「当前时间 − 60 分钟」、最后一条轨迹时间早于「当前时间 − 60 分钟」，且（小程序已接受订阅（前端经 `POST /subscribe/record` 登记）或 服务号已订阅）。
  2. 任务仅在 8:00–22:00（上海时区）执行；每批 100，循环取批直到取空或整批均为本轮已尝试用户。
  3. 发送前先 `MarkAbnormalAlertSent` 原子占位：仅当 `abnormal_alert_sent_at` 为空或上海日期早于今天才更新成功；更新 0 行表示今天已发，跳过 → **每自然日（上海）最多一次**。
  4. 服务号与小程序通道各自独立尝试（`canMP` / `canMini`），两者都不可用时直接返回且不占位。
  5. 发送失败不撤销占位（宁可漏报不重报）；微信 `code=43101` 时把 `abnormal_subscribe_accepted` 置 false，`43004` 时把服务号订阅置 false。
  6. VIP 判定使用 `vip.InfoProvider.GetVIPInfo`（严格，无宽限期），与候选 SQL 的 `v.expire_time > now()` 口径一致。
  7. `PUT /auto-record/active` 仅更新 `users.last_active_at`，不重置 `abnormal_alert_sent_at`；告警抑制由候选 SQL 的 `last_active_at < now()-1h` 条件实现。

### AR-12 常用地址摘要（后台汇总）
- 故事：每天凌晨把用户日记里的高频地址固化为「常用地址」，供自动记录替换。
- AC：
  1. 每天 03:00（上海）执行一次，`maxDuration=30m`。
  2. 只处理「昨日 00:00–24:00（上海）`diary_entries.updated_at` 有变动」的用户，keyset 游标分批 1000，固定 5 worker。
  3. 单用户先过 `NeedsCommonAddressRefresh` 门控（日记最大 `updated_at` > 常用地址最大 `updated_at`，或常用地址非空而日记为空）；不需刷新直接跳过。
  4. 取该用户 `address` 非空的 top10（`COUNT(*) DESC, MAX(created_at) DESC`），批量取每个地址的最新坐标；无坐标的脏地址跳过。
  5. 同一事务内先 `DELETE` 旧记录再批量 `INSERT`；top 为空只删不插。

### AR-13 轨迹清理
- 故事：删除过期轨迹点，防表膨胀。
- AC：
  1. 每 6 小时执行，`maxDuration=10m`；分批 1000，循环直到删除行数 < 1000。
  2. 删除条件 `created_at < now() - interval '7 days'`。

## 4. 数据模型

| 表 | 关键字段 | 说明 |
|----|---------|------|
| `auto_record_trajectories` | `id text PK`、`user_id text FK→users ON DELETE CASCADE`、`lat numeric(10,7)`、`lon numeric(10,7)`、`recorded_at timestamptz DEFAULT now()`、`geocode_attempts int DEFAULT 0`、`created_at timestamptz DEFAULT now()`；CHECK lat∈[-90,90]、lon∈[-180,180] | 轨迹点。索引：`created_at`、`geocode_attempts`、`recorded_at`、`(user_id,geocode_attempts)`、`(user_id,recorded_at)`。唯一索引 `uq_auto_record_trajectories_point (user_id, recorded_at, lat, lon)`（迁移 000005，`ON CONFLICT DO NOTHING` 幂等，见 AR-3/§6.2；`000001_baseline.up.sql` 基线本身不含该约束） |
| `user_common_addresses` | `user_id text`、`name text`、`lat numeric(10,7)`、`lon numeric(10,7)`、`count int DEFAULT 0`、`updated_at timestamptz`；PK `(user_id,name)`；CHECK count≥0、坐标范围 | 常用地址摘要。由 AR-12 汇总写入 |
| `diaries` | `id text PK`、`user_id`、`record_date date`、UNIQUE `(user_id,record_date)` | 自动成文的日记载体；同日多 cluster 复用同一条日记 |
| `diary_entries` | `id`、`diary_id`、`created_by`、`text`、`lat`、`lon`、`address`、`detail_address`、`record_time`、`sort`、`color` | 自动记录条目：`text='（自动记录）'`、`address=landmark`、`detail_address=detail`；`sort=0`、`color=NULL`。索引 `idx_diary_entries_auto_last ON (created_by, record_time DESC) WHERE text='（自动记录）'` |
| `users` | `auto_record_enabled bool DEFAULT false`、`abnormal_subscribe_accepted bool DEFAULT false`、`abnormal_alert_sent_at timestamptz`、`last_active_at timestamptz`；索引 `idx_users_auto_record_enabled` | 开关与告警状态 |
| `user_vips` | `user_id`、`expire_time timestamptz` | VIP 判定数据源 |

> 源：`backend/migrations/000001_baseline.up.sql`；`backend/internal/db/sqlc/auto_record.sql`、`user_common_address.sql`、`diary_entry.sql`、`user.sql`、`vip.sql`。迁移 000002（封面 trigger）、000003（seed）、000004（client_ops_logs）、000006（drop sys_configs）与本域无直接关系。

## 5. API 契约

统一响应信封见 03-api（成功 `code=0000, message=ok`；失败 `code`、`message`、`biz_code`）。以下路由均注册在 `main.go` 的 session 鉴权组内（`backend/cmd/server/main.go`），SaaS 外部经 Cloudflare Worker 原样转发（api-worker 不重写 path）。

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/auto-record/config` | Session | 返回 `data.enabled`；非 VIP 恒为 false，不暴露真实开关值；读用户 DB 失败时降级返回 200 `{enabled:false}`（非 500）（`autorecord/handler.go`） |
| PUT | `/auto-record/config` | Session | body `{enabled:bool}`，≤4096B；开启需**严格 VIP**，否则 403 `code=4030,biz_code=NOT_VIP`；成功返回空 data |
| PUT | `/auto-record/active` | Session | 刷新 `users.last_active_at`（best-effort 心跳）；DB 错误 500 `code=5001` |
| POST | `/auto-record/trajectories` | Session | body `{points:[{lat,lon,recordedAt}]}`，≤64KiB，≤50 点；非 VIP 403 `NOT_VIP`；坐标越界/缺字段/时间非 RFC3339 → 400 `code=4000`；空数组返回成功空 data |
| GET | `/location/reverse` | Session | query `latitude,longitude`（可 `pois=0` 关闭 POI）；坐标 4 位小数取整；配额超限 429 `code=4290,biz_code=RATE_LIMITED`；上游失败 500；返回 `address/detailAddress/landmark/areaCode/areaName/pois` |
| POST | `/diary/details/auto` | Session | body `{lat,lon}`，≤64KiB；非 VIP 403 `code=4030,biz_code=NOT_VIP`；坐标非法 400；配额超限 429 `code=4290`；与后台任务并发占用同一用户锁时 429 `code=4290,biz_code=OPERATION_IN_PROGRESS`；成功 `data.id` |

### 5.1 错误码（本域实际抛出；code 固定枚举 + biz_code 语义码，见 03-api §3）

| 场景 | HTTP | `code` | `biz_code` | 源 |
|------|------|---------|-------------|----|
| `PUT /auto-record/config` 非 VIP 开启 | 403 | 4030 | NOT_VIP | `autorecord/handler.go` |
| `POST /auto-record/trajectories` 非 VIP | 403 | 4030 | NOT_VIP | `autorecord/handler.go`（400 → 403） |
| 轨迹 body 超限（>64KiB） | 413 | 4130 | — | `autorecord/handler.go` |
| 轨迹 body 非法 / 超 50 点 / 坐标越界 / 时间非法 | 400 | 4000 | — | `autorecord/handler.go` |
| 轨迹入库失败 | 500 | 5001 | — | `autorecord/handler.go` |
| `GET /location/reverse` 配额超限 | 429 | 4290 | RATE_LIMITED | `location/handler.go` |
| `GET /location/reverse` 参数/坐标非法 | 400 | 4000 | — | `location/handler.go` |
| `GET /location/reverse` 上游失败 | 500 | 5001 | — | `location/handler.go` |
| `POST /diary/details/auto` 非 VIP | 403 | 4030 | NOT_VIP | `diary/handler.go` |
| `POST /diary/details/auto` 配额超限 | 429 | 4290 | RATE_LIMITED | `diary/handler.go` |
| `POST /diary/details/auto` 自动记录用户锁被后台占用 | 429 | 4290 | OPERATION_IN_PROGRESS | `diary/handler.go` |

## 6. 关键实现约束

### 6.1 阈值常量

| 常量 | 值 | 位置 |
|------|-----|------|
| `stayPointMergeRadiusM` | 300.0 m | `autorecord/service.go` |
| `stayPointMergeWindow` | 30 min | 同上 |
| `maxPendingPerUser` | 100 | 同上（**一名双义**：既是单用户单轮轨迹处理上限（AR-4.1 `LIMIT`），也是候选用户批上限（AR-4.7）；两处共用同一常量，调整需同时评估两处影响） |
| `processWorkers` | 5 | 同上 |
| `maxGeocodeAttempts` | 10 | 同上 |
| `commonAddressMatchRadiusM` | 300.0 m | 同上 |
| `commonAddressMaxCount` | 10 | `db/common_address.go` |
| `maxBatchPoints` | 50 | `autorecord/handler.go` |
| `maxUploadBodySize` | 64 KiB | 同上 |
| `MaxReversePerUserPerDay` | 200 | `location/quota.go` |
| `reverseQuotaTTL` | 24 h | 同上 |
| `geoCoderRPS` | 10 | `pkg/limiter/tencent_map.go` |
| `abnormalAlertBatchSize` | 100 | `jobs/runner.go` |
| `cleanupBatchSize` | 1000 | 同上 |
| `commonAddressSummaryWorkers` / `BatchSize` | 5 / 1000 | 同上 |
| 轨迹清理窗口 | `created_at < now() - interval '7 days'` | `auto_record.sql` |

> VIP 判定一律严格（`expire_time > now()`），无宽限期常量；分布式锁为 PG advisory lock，无 TTL/续期常量（ADR-0005）。

### 6.2 锁与幂等

- 用户级锁 `lock:auto_record:{userID}`：PostgreSQL advisory lock（ADR-0005，无 TTL），获取失败立即返回 error（不自旋）；`defer Unlock` 用 `context.WithoutCancel`。`autorecord/service.go`。
- 后台任务锁 `lock:background:{task}`：PostgreSQL advisory lock（无 TTL），获取失败静默跳过当轮；任务 context 受 `maxDuration` 约束，超时取消。`jobs/runner.go`。
- 后台成文的游标 / 已处理标记仅为减少重复处理；丢失或过期时通过重新扫描 + DB 幂等（唯一约束 / `ON CONFLICT`）兜底，**不作为正确性前提**。
- 幂等/去重：
  - 自动记录同点去重（AR-7）无数据库唯一约束，靠应用层比较。
  - 异常告警每日一次靠 `MarkAbnormalAlertSent` 的条件 UPDATE（Shanghai 日期）。
  - `CreateAutoRecordEntry` 带 `ON CONFLICT (id) DO NOTHING`；`UpsertDiary` 走 `(user_id, record_date)` 唯一冲突。
  - 轨迹上报按自然键 `(user_id, recorded_at, lat, lon)` 唯一索引 + `ON CONFLICT DO NOTHING` 幂等（迁移 000005）；前端 `batchSeq` 仅记录日志用于观测。

### 6.3 降级与失败处理

| 依赖 | 失败时行为 |
|------|-----------|
| Redis 配额检查 | `CheckReverseQuota` 返回 false（fail-closed）：`/location/reverse` 429；后台处理跳过 cluster 且不删轨迹；`/diary/details/auto` 429 |
| PG advisory 锁 | 获取失败记日志并跳过该用户/任务；DB 不可用时任务自然失败（幂等，下轮重试） |
| 腾讯地图 | 重试 1 次后失败：跳过该 cluster（保留轨迹）；`/location/reverse` 500 |
| Redis 缓存失效（`ai:family_summary`） | 仅 warn 日志，不影响成文 |
| 推送（新地点 / 异常告警） | 仅记日志，不回滚已写数据/已占位告警 |

### 6.4 环境变量（`backend/internal/config/config.go`、`backend/internal/redis/redis.go`）

| 变量 | 必填 | 默认 | 说明 |
|------|------|------|------|
| `TENCENT_MAP_KEY` | 是（缺失 validate 失败） | — | 逗号分隔多 key，轮询使用 |
| `JOB_INTERVAL_AUTO_RECORD` | 否 | `5m` | 自动记录轮询间隔；`maxDuration=10m` |
| `JOB_INTERVAL_ABNORMAL_ALERT` | 否 | `5m` | 异常告警轮询间隔；`maxDuration=5m` |
| `JOB_INTERVAL_CLEANUP_TRAJECTORIES` | 否 | `6h` | 轨迹清理间隔；`maxDuration=10m` |
| `REDIS_ADDR` | 是 | — | Redis 地址（`redis://` / `rediss://` 或 host:port） |
| `REDIS_PASSWORD` | 否 | — | 非 URL 形式时使用 |
| `REDIS_POOL_SIZE` | 否 | 50（最小 10） | 连接池大小 |

> 常用地址汇总为固定时刻 03:00，无对应环境变量。

## 7. 前端接入

- 文件：`frontend/miniapp/miniprogram/utils/autoRecord.ts`（采集/上报/恢复状态机）、`pages/index/index.ts`（开关入口）、`app.ts`（启动与前后台钩子）、`utils/concurrency.ts`（通用并发工具）。
- `pages/index/index.ts`：`toggleAutoRecord()` 是 index 页唯一手动开关入口（详情/编辑内另有 `components/AutoBtn/AutoBtn.ts` 的 `change`）；非 VIP 弹窗引导到 VIP 页；开启成功后置 `showSubscribePrompt=true` 引导订阅；`_changing` 防重复触发，且不在 `onHide` 复位。
- `app.ts`：`onLaunch` 读 `STORAGE_KEY_ENABLED` 初始化 `globalData.openAutoRecorded`；登录失败也调用 `tryRestoreAutoRecord`；`onShow` 有 30 秒恢复冷却（仅成功才计时）；`onHide` 触发一次 flush+report。
- 本地 storage key：`papafeiji:autoRecordEnabled`（开关）、`papafeiji:autoRecordStayPoints`（驻留点队列）。
- 客户端审计日志：上报前后 `opsLog('auto_upload')` / `opsLogFail('auto_upload_fail')`；首次成文 `auto_entry_ok` / `auto_entry_fail`（写入 `client_ops_logs`，见 migration 000004）。
- 前端关键阈值（`autoRecord.ts`）：

| 常量 | 值 |
|------|-----|
| 质心计算间隔（前台/后台） | 30s / 120s |
| 质心窗口（前台/后台） | 60s / 600s |
| 最少点数（前台/后台） | 3 / 2 |
| 最多点数（前台/后台） | 200 / 50 |
| 静止阈值 | 300 m |
| 离开确认次数 | 2 |
| 后台静止检查间隔 | 60s |
| 驻留成熟时长 | 10 min |
| 本地队列上限 | 50 |
| 坐标精度 | 4 位小数 |
| 精度过滤 | iOS 3000 / 其它 500 |
| VIP 缓存有效期 | 5 min |
| 心跳间隔 | 30 min |
| 后台兜底 getLocation 间隔 | 5 min |
| 上报退避 | 30s×2ⁿ，上限 5 min |
| 恢复重试退避 | 60s→5min；看门狗 60s |
| 开启超时 | 30s |

- `utils/concurrency.ts` 的 `runWithConcurrency` 当前**仅被 `utils/http.ts`（图片上传并发 3）使用**，自动记录未使用它（`autoRecord.ts` 用单飞 Promise 串行）。

## 8. 运维与任务

后台任务总览（`jobs/runner.go Runner.Start`，SaaS 与开源均运行；所有锁均为 PG advisory lock，无 TTL/续期）：

| 任务 | lock key | 间隔 | maxDuration | 本域 |
|------|----------|------|-------------|------|
| 自动记录 | `lock:background:auto_record` | `JOB_INTERVAL_AUTO_RECORD` 默认 5m | 10m | 是 |
| 异常告警 | `lock:background:abnormal_alert` | `JOB_INTERVAL_ABNORMAL_ALERT` 默认 5m | 5m | 是 |
| 轨迹清理 | `lock:background:cleanup_trajectories` | `JOB_INTERVAL_CLEANUP_TRAJECTORIES` 默认 6h | 10m | 是 |
| 常用地址汇总 | `lock:background:common_address_summary` | 每日 03:00（上海），`scheduleDailyAt` | 30m | 是 |
| 已删对象 CDN 刷新 | `lock:background:purge_deleted_objects` | 默认 24h（`CDN_REFRESH_ENABLED=1` 才启用） | 10m | 否 |
| 订单关闭 | `lock:background:order_close` | 默认 1m | 5m | 否 |
| AI 日志清理 | `lock:background:cleanup_ai_logs` | 默认 24h | 10m | 否 |
| 孤儿文件清理 | `lock:background:cleanup_orphan_files` | 默认 7d | 30m | 否 |
| 孤儿轨迹图清理 | `lock:background:cleanup_orphan_traj_maps` | 默认 7d | 10m | 否 |
| 客户端日志清理 | `lock:background:cleanup_client_ops_logs` | 默认 24h | 10m | 否 |

- 停止：`Runner.Stop` 取消 ctx、停 ticker、等待 goroutine，30s 超时。
- 任务观测：`recordJobSuccess`/`recordJobFailure` 写 Redis `job:last_success:{task}` / `job:last_failure:{task}` / `job:fail_streak:{task}`；`watchJobHealth` 每分钟检查 `now - last_success > 3× 周期` 输出 `job_stale`，并消费 `job:trigger:{task}` 做人工补跑（`KnownJobNames` 共 10 项）。
- 后台用独立 `bgPool`（自动记录/后台任务），主 `pool` 服务请求。
- Redis 客户端（`internal/redis/redis.go`）：PoolSize 默认 50（`REDIS_POOL_SIZE`，<10 取 10）、MinIdleConns 5、Dial 5s、Read/Write 3s；启动异步 Ping 失败仅 warn，不阻断启动。
- 本域使用的 Redis key：

| Key | 类型 | TTL | 用途 |
|-----|------|-----|------|
| `location:reverse:{userID}:{YYYY-MM-DD}` | counter | 首次 +24h | 逆地理日配额 |
| `ai:family_summary:{familyID}` | string 缓存 | 由 AI 模块设定 | 成文后失效（best-effort） |
| `job:cursor:auto_record` | string | 无 | 候选用户公平轮转游标（最后处理的 user_id） |
| `job:last_success:{task}` / `job:last_failure:{task}` | string(RFC3339) | 无 | 后台任务最近成功/失败时间 |
| `job:fail_streak:{task}` | counter | 无 | 后台任务连续失败次数 |
| `job:trigger:{task}` | string | 无 | 人工补跑触发键（`job run <name>` 写入，常驻 app 每分钟消费后删除） |

> 注意：`lock:auto_record:{userID}` 与 `lock:background:{task}` 为 **PostgreSQL advisory lock**（ADR-0005），已不是 Redis key，无 TTL/续期。
