# PP-02A 认证与账号（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：无
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。

## 1. 背景与目标

本域负责「用户是谁、如何登录、如何维护账号身份」：

- 微信小程序登录与会话（服务端 session 存 Redis）。
- 手机号绑定 / 解绑（含每日绑定次数限制）。
- 邀请人关系建立（注册期绑定 + 登录后补绑）与邀请奖励发放。
- 用户资料（头像 / 昵称 / 语言）读取与更新。
- 账号注销（含家庭解散 / 成员迁移 / 数据与文件清理的入口编排）。

**不属于本域**（由其它分册负责）：

- 邀请码 / 二维码的生成与解析、家庭邀请链接与家庭成员管理 → PP-02D。
- VIP 试用 / 免费领取 / 支付的具体权益计算 → PP-02E（本域只调用 `vipService` 的发放接口）。
- 微信公众号（服务号）订阅状态写入 → 由 `wxmp` 模块维护；本域只读 `wx_mp_accounts.subscribed` 作为 `mpSubscribed`。
- 头像文件上传 / 存储实现 → 文件分册（本域只接收 `fileId` 并校验归属）。

**关键实现位置**（关键处正文内联标注）：

| 文件 | 职责 |
|---|---|
| `backend/internal/middleware/session.go` | 服务端 session 生成 / 校验 / 滑动续期 / 删除 |
| `backend/internal/middleware/response.go`、`body.go` | 统一响应包与 JSON body 读取 |
| `backend/internal/middleware/ratelimit.go` | IP 滑动窗口限流 |
| `backend/internal/auth/handler.go` | 登录 / 登出 / 手机号 / 补绑 / 注销 / 昵称种子 |
| `backend/internal/auth/wechat.go` | jscode2session、getuserphonenumber、access_token 缓存 |
| `backend/internal/user/handler.go` | 资料 / 头像 / 昵称 / 语言 / VIP / 常用地址 |
| `backend/internal/userinfo/builder.go` | 对外 `userInfo` 字段构造 |
| `backend/internal/user/cleanup.go` | 注销后 session / 物理文件清理 |
| `backend/internal/family/service.go`（`DeleteAccount`） | 注销事务（家庭关系 + 用户数据） |
| `backend/cmd/server/main.go` | 路由挂载、限流器、鉴权分组 |
| `backend/cmd/admin/main.go` + `deploy/delete-user.sh` | 按手机号运维删除用户 |
| `backend/migrations/000001_baseline.up.sql` | users / user_invites / user_invite_codes / wx_mp_accounts 表定义 |

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | 鉴权采用**服务端 session**（Redis 存储） | 可任意时刻按用户批量删除会话（注销 / 踢下线） | 无 |
| D2 | 会话有**滑动 TTL 30 天** + **绝对生命周期 90 天** 双重限制 | 活跃用户自动续期；持续活跃也不能超过 90 天，强制重新登录 | 无 |
| D3 | 用户身份键按 **unionid 优先、其次 openid** 匹配；二者在同一个微信主体下唯一 | 小程序与公众号可归一账号；`users.unionid` / `open_id` 各带唯一索引 | 无 |
| D4 | 新用户注册事务内**自动创建个人家庭**并发放试用 VIP | 个人家庭是所有用户的默认归属，保证家庭模型可自足 | 无 |
| D5 | 手机号**绑定**每天最多 1 次（按 `phone_bind_time` 的上海日期判断）；**解绑不调用微信，但保留 `phone_bind_time`**，故当天解绑后当天不能再绑定 | 绑定消耗微信 code 交换额度，需限频；保留时间避免「绑→解绑→再绑」绕过日限 | 无 |
| D6 | 邀请关系**双写** `users.invited_by` 与 `user_invites`；登录后补绑接口幂等，注册超过 7 天静默成功不绑定 | 奖励面向新用户，防老用户扫码刷奖励 | 无 |
| D7 | 注册期绑定与补绑**共用同一奖励函数** `applyInviteRewardsWithTx` | 两条路径奖励逻辑永不漂移 | 无 |
| D8 | 语言仅接受白名单 `zh` / `zh-Hant` / `en`，默认 `zh` | 供 AI 对话按用户语言输出 | 无 |

## 3. 核心流程（用户故事 + 时序）

> 编号前缀 `A-` 稳定不变；验收标准（AC）均可直接转成断言。

### A-1 微信登录（新用户自动注册）

**故事**：作为微信用户，我在小程序内静默登录，无需注册表单；首次登录即获得试用 VIP 与个人家庭。

时序（源：`backend/internal/auth/handler.go:93` `Login`、`findOrCreateUser`）：

1. 客户端 `wx.login()` 取 code，`POST /auth/login`，body 可选 `inviter`。
2. `middleware.ReadJSONBody(w,r,&req,4096)`；`code` 为空 → 400。
3. `h.wechat.Jscode2session`：微信 `errcode` 40029/40163 → `ErrWechatInvalidCode`；其它非 0（含 -1）→ `ErrWechatService`。
4. `findOrCreateUser`：
   - `session.UnionID != ""` 且 `GetUserByUnionID` 命中 → 仅 `UpdateUserSessionKey`，返回老用户。
   - 否则 `GetUserByOpenID` 命中 → `UpdateUserSessionKey`；若原 `unionid` 为空则补写 `UpdateUserUnionID`。
   - 都未命中 → 事务内新建：`CreateFamily(is_personal=true)` → `CreateUser`（`user_type='wechat'`，随机昵称）→ `UpsertFamilyMembership(role='owner')` → `IssueTrialVIPWithTx`（撞 openid 墓碑、即该微信主体已领取过 trial 时**静默跳过发放、不回滚注册**；409 仅由 `/vip/new-user` 端点承载）→ `applyInviteRewardsWithTx` → `createOrGetUserInviteCode`（8 位短码）。
5. 若 `session.UnionID != ""`：`LinkWxMPAccountByUnionID` 把 `user_id IS NULL` 的公众号记录认领给该用户，失败仅 WARN。
6. `sessions.Create(ctx, user.ID)` 写 Redis（Lua 原子脚本）。
7. 读 `wx_mp_accounts.subscribed` 得 `mpSubscribed`（`ErrNoRows` 静默视为未订阅；真实 DB 错误 WARN 后按 false 处理）。
8. 返回 `{ sessionId, newUser, userInfo }`；新用户异步生成默认头像 marker。

**AC**

- 请求体 > 4096 字节 → HTTP **413**、`code="4130"`；非法 JSON → HTTP 400、`code="4000"`。
- `code` 缺失 → HTTP 400，`message="code is required"`。
- 微信返回 40029/40163 → HTTP 400，`message="invalid wechat code"`；其它微信错误 → HTTP 500，`message="wechat login failed"`。
- 成功响应**必含** `data.sessionId`（64 字符 hex）、`data.newUser`、`data.userInfo`。
- `data.userInfo` 仅含 `id/avatarUrl/nickName/isVip/expireTime/currentFamilyId/mpSubscribed`，**不含** `session_key/openid/unionid/phone_number`。
- 首次登录响应 `newUser=true`，且 DB 中该用户存在个人家庭（`personal_family_id = current_family_id`、`family_members.role='owner'`）。
- 同一 unionid / openid 重复登录不新建用户（`users` 行数不增），仅刷新 `session_key`。
- 并发首登时 `users` 唯一索引冲突（23505）被捕获：回查已存在用户、补写 `session_key`（及 `unionid`），返回 `newUser=false`。

### A-2 登出

**故事**：用户点击退出后，当前设备的会话立即失效。

时序（`auth/handler.go` `Logout`）：从 context 取 `sessionID` → `sessions.Delete`（Lua 脚本从 `session:{id}` 反查 uid，移除 `sessions:user:{uid}` 索引并删除 `session:{id}`、`session:abs:{id}`）→ 200 `{}`。

**AC**

- 携带有效 Bearer session 调 `POST /auth/logout` → 200；随后用同一 session 调任意受保护接口 → 401。
- `sessions.Delete` 报 Redis 错误 → HTTP 500，`message="logout failed"`。

### A-3 手机号绑定

**故事**：用户授权微信手机号后绑定到自己账号，每天只能改一次。

时序（`auth/handler.go:215` `BindPhone`）：

1. 读取 body（4096 上限），取 `code`。
2. `GetUserByID`；再判 `phoneModificationLockedToday`：`phone_bind_time` 的**上海日期**等于今天 → 400。
3. `wechat.GetPhoneNumber(ctx, code)`（内部走 `stable_token` 换 access_token，带 Redis 缓存）。
4. `BindUserPhoneIfAllowed` 原子条件更新 `phone_number` + `phone_bind_time=now`（日限在 SQL 内判断）。
5. 命中 `phone_number` 唯一索引冲突 → 409。

**AC**

- 当天已绑定过再调 `POST /auth/phone/bind` → HTTP 400，`message="phone can only be modified once per day"`，**且不调用微信接口**（日限检查在微信调用之前）。
- `code` 无效（微信 40029/40163）→ HTTP 400，`message="invalid phone code"`；其他微信错误 → HTTP 500。
- 新手机号已被别的用户绑定 → HTTP **409**，`code="4090"`，`biz_code="PHONE_ALREADY_BOUND"`，`message="phone already bound"`。
- 成功 → HTTP 200 `{}`；`users.phone_bind_time` 更新为当前时间。
- 日限为**原子条件更新**（`BindUserPhoneIfAllowed ... WHERE phone_bind_time 为空或不在今天`），并发下最多成功 1 次。

### A-4 手机号解绑

**故事**：解绑手机号不消耗微信额度；因日限按“当天是否绑定过”判定，当天绑定并解绑后，当天不能再绑定；跨日解绑不影响当日绑定额度（判定依据为最近一次绑定日期，见 D5/AC）。

时序（`auth/handler.go:276` `UnbindPhone`）：读 body → `GetUserByID` → 未绑定 → 400 → `UpdateUserPhone(phone_number=null, phone_bind_time=保留原值)` → 200。

**AC**

- 未绑定手机号时调用 → HTTP 400，`message="phone not bound"`。
- 成功解绑后 `users.phone_number` 为 NULL，`users.phone_bind_time` 保留（用于日限）；`GET /auth/phone` 返回 `phoneNumber=null`，`canModifyToday` 按“当天是否绑定过”判定（当天绑定过则为 false）。
- 解绑**不校验** `code`、不调用微信（`code` 字段仅为客户端兼容保留）。
- `GET /auth/phone` 在用户不存在时返回 HTTP 400（`StatusBadRequest`），而非 404。

### A-5 登录后补绑邀请人

**故事**：极弱网下用户可能在场景码解析完成前就完成登录，客户端解析完成后调用补绑接口。

时序（`auth/handler.go:326` `BindInviter`）：

1. 读 body；`inviter` 为空或等于自己 → 400。
2. 校验邀请人存在（`pgx.ErrNoRows` → 400 `inviter not found`）。
3. 读当前用户；`invited_by` 非空 **或** 存在 `user_invites` 记录 → 幂等 200。
4. `time.Since(user.CreatedAt) > 7*24h` → 静默 200（不绑定、不奖励）。
5. `db.WithTx`：`GetUserByIDForUpdate`（行锁）→ 二次判重 → `UpdateUserInvitedBy`（SQL 条件 `invited_by IS NULL OR ''`）→ `applyInviteRewardsWithTx`。

**AC**

- 已有邀请人时重复调用 → 200 且不新增 `user_invites`、不重复加 VIP。
- 注册超过 7 天 → 200 且 `users.invited_by` 保持为空、无 `user_invites` 记录。
- 邀请人不存在 / 自邀 → HTTP 400。
- 并发两次同用户补绑：行锁 + 唯一索引（`user_invites.user_id`）保证只奖励一次。

### A-6 邀请奖励规则（注册期与补绑共用）

源：`auth/handler.go:675` `applyInviteRewardsWithTx`、`backend/internal/db/sqlc/invite.sql`。

- 事务内确认邀请人存在（不存在则跳过，不失败）。
- 插入 `user_invites(user_id=被邀请人, inviter_id=邀请人, entry_count=0, user_open_id=被邀请人 openid)`。
- 被邀请人：先 `MarkInviteeRewarded`（execrows，`WHERE reward_invitee_at IS NULL`；openid 撞 `uq_user_invites_user_open_id` 部分唯一索引 23505 时仅 Info 日志跳过）→ 标记成功（markRows>0）才 `ExtendVIPDaysWithTx(+3 天)`。
- 邀请人：先 `LockInviterReward`（PostgreSQL 事务级 advisory lock `hashtext('inviter_reward:'||inviter_id)`）→ `CountInviterMonthlyRewardDays`（当月 `reward_inviter_at` 计数 × 7）→ 若 `< 14`：先 `MarkInviterRewarded`（返回受影响行数）→ 行数 > 0 才 `ExtendVIPDaysWithTx(+7 天)`。

**AC**

- 被邀请人 +3 天、邀请人 +7 天，各只加一次。
- 邀请人每个自然月最多获得 2 次奖励（当月已计 14 天即不再加）。
- 并发的两次奖励不会双发（advisory 锁 + `MarkInviterRewarded` 的 `WHERE reward_inviter_at IS NULL`）。
- 每个微信主体最多绑定一个邀请人（`user_invites.user_id` 唯一，注销后行保留仅 openid 墓碑）；链接加入奖励（`family/grantJoinReward`，见 PP-02D，窗口同为注册后 7 天（两入口统一））与登录期奖励互斥，仅先到者生效。被邀请奖励按 openid 终身一次（`uq_user_invites_user_open_id` 部分唯一索引）。

### A-7 用户资料 / 语言

源：`backend/internal/user/handler.go`、`backend/internal/userinfo/builder.go`。

- `GET /user/profile`：`GetUserByID` → 读公众号订阅（`ErrNoRows` 静默降级 false）→ `userinfo.Build`。
- `PUT /user/avatar`：`fileId` 必填；文件须存在、`file_type='image'`、`created_by=当前用户`；成功后同步删除旧头像物理文件（解引用后）、异步生成新 marker（30s 超时）。
- `PUT /user/nickname`：`validator.ValidateNickname`（trim 后 1–20 个 code point、无控制字符）。
- `PUT /user/lang`：白名单 `zh/zh-Hant/en`。
- `GET /user/vip`：返回 `{isVip, expireTime}`（`expireTime` 为上海时区 `2006-01-02T15:04:05-07:00` 字符串；未过期才 `isVip=true`）。

**AC**

- `GET /user/profile` 用户不存在 → 404；DB 错误 → 500；成功字段固定为 `id/avatarUrl/nickName/isVip/expireTime/currentFamilyId/mpSubscribed`。
- 头像文件不属于当前用户 → 403；文件非 image → 400；文件不存在 → 404。
- 昵称长度 21 个 code point → 400；昵称去空格后为空 → 400。
- `PUT /user/lang` 传 `ja` → 400 `unsupported language`；传 `zh-Hant` → 200。

### A-8 账号注销

**故事**：用户输入自己的昵称确认后，永久删除账号及其数据。

时序（`auth/handler.go:413` `DeleteAccount` → `family.Service.DeleteAccount` → `user.CleanupAfterAccountDeletion`）：

1. 读 body，`confirmName` 非空 → 与当前 `users.nickname` 精确比较，不等 → 400。
2. **先删除当前 session**（失败 → 500，不继续）。
3. `familyService.DeleteAccount`：用户级锁 `lock:delete_account:{userID}`（PG advisory lock）→ 读家庭快照 → 家庭级锁 `lock:family:{id}`（字典序，PG advisory lock）→ `WithTxDeferrable`（RepeatableRead, Deferrable, 串行化冲突重试 3 次）内：家庭关系处理（owner 解散/成员迁回个人家庭）、清空 current/personal family、删封面、删个人家庭、`NullifyOrdersByUser`、删 API key / 邀请码 / 常用地址 / AI 日配额、分页收集文件路径（每批 1000）、`DeleteUser`。
4. DB 提交后：`safe.Go` 异步（`context.Background()+5min`）执行 `CleanupAfterAccountDeletion`：对 `AffectedUserIDs` 逐个 `sessions.DeleteAll`、按路径删物理文件、删封面、删用户头像 marker（`system-assets/default-marker.png` 除外）。
5. 返回 200（清理失败只记 ERROR 日志）。

**AC**

- `confirmName` 与昵称不一致 → 400 `confirmName mismatch`；用户不存在 → 404；昵称为空/无效 → 500。
- 注销成功（200）后：`users` 无该 id；`user_invite_codes` 无该用户记录；`user_invites` 行保留（`user_id` 置 NULL 墓碑，见下方例外）；该用户全部 session 被删（旧 session 调接口 → 401）。
- 并发注销同一用户：未抢到用户锁的一次返回 HTTP 429（`biz_code=OPERATION_IN_PROGRESS`）。
- 同一 IP 1 小时内第 6 次 `DELETE /auth/account` → HTTP 429，`code="4290"`，`biz_code="RATE_LIMITED"`。
- DB 事务成功提交后，即便后续 session / 文件清理失败，响应仍为 200。
- 注销为物理删除；同一微信后续登录会创建新的用户（新 `id`），无数据恢复。**例外（领取/邀请墓碑）**：`user_vip_claims` 与 `user_invites` 的 `user_id` 因 FK `ON DELETE SET NULL` 保留仅含 openid 的墓碑行（无业务数据），作为 trial/free 领取与被邀请奖励「每微信主体终身一次」的判定依据；`user_invites.inviter_id` 同为 SET NULL（000011），邀请人注销不再连带删除墓碑行、终身一次判定不受邀请人存续影响。

## 4. 数据模型

源：`backend/migrations/000001_baseline.up.sql`。

### 4.1 `users`

| 字段 | 类型 | 约束 / 说明 |
|---|---|---|
| `id` | text | PK；UUID v7（`pkg/util/uuid.go`） |
| `open_id` | text | NOT NULL；唯一索引 `idx_users_openid` |
| `unionid` | text | 可空；部分唯一索引 `idx_users_unionid`（`WHERE unionid IS NOT NULL`） |
| `phone_number` | text | 可空；部分唯一索引 `idx_users_phone_number` |
| `phone_bind_time` | timestamptz | 可空；手机号日限判定依据（上海日期） |
| `avatar` / `avatar_file_id` | text / text | `avatar` ≤2048；`avatar_file_id` FK `files(id) ON DELETE SET NULL` |
| `nickname` | text | 可空；注销确认名；新用户随机昵称 |
| `user_type` | text | 默认 `'wechat'`；CHECK 仅允许 `'wechat'` |
| `personal_family_id` / `current_family_id` | text | FK `families(id) ON DELETE RESTRICT` |
| `invited_by` | text | FK `users(id) ON DELETE SET NULL`；索引 `idx_users_invited_by` |
| `session_key` | text | 微信 session_key（敏感，不外泄） |
| `lang` | text | 默认 `'zh'` |
| `auto_record_enabled` | bool | 默认 false |
| `image_storage_bytes` | bigint | 默认 0；CHECK ≥0 |
| `abnormal_subscribe_accepted` / `abnormal_alert_sent_at` / `last_active_at` | bool / timestamptz / timestamptz | 异常告警相关（其它分册使用） |
| `created_at` / `updated_at` | timestamptz | 默认 `now()`；`updated_at` 由各 UPDATE 显式刷新 |

### 4.2 `user_invites`

| 字段 | 类型 | 约束 / 说明 |
|---|---|---|
| `id` | text | PK |
| `user_id` | text | **UNIQUE**（`user_invites_user_id_key`）；FK `users ON DELETE SET NULL`（000008：注销保留行，作被邀请奖励终身一次判定）；即「一个用户最多一个邀请人」 |
| `inviter_id` | text | **可空**；FK `users ON DELETE SET NULL`（000011：邀请人注销保留被邀请人墓碑行，使「每微信主体终身一次被邀请奖励」不被绕过；down 恢复 CASCADE 前需人工清理 `inviter_id IS NULL` 的孤儿墓碑行）；索引 `(inviter_id, created_at DESC)` |
| `entry_count` | int | 默认 0 |
| `user_open_id` | text | 可空；000008 新增并从 `users.open_id` 回填；被邀请人微信 openid 冗余，注册期与家庭链接两条奖励路径均写入，注销后行保留作被邀请奖励终身一次判定 |
| `reward_inviter_at` | timestamptz | 邀请人奖励发放时间；月度上限按此字段统计 |
| `reward_invitee_at` | timestamptz | 被邀请人奖励发放时间 |
| `created_at` | timestamptz | 默认 now() |

补充索引：`idx_user_invites_pending (user_id) WHERE reward_invitee_at IS NULL`；`idx_user_invites_reward_inviter_at (inviter_id, reward_inviter_at) WHERE reward_inviter_at IS NOT NULL`；`uq_user_invites_user_open_id (user_open_id) WHERE user_open_id IS NOT NULL AND reward_invitee_at IS NOT NULL`（000008，被邀请奖励每微信主体终身一次）。

### 4.3 `user_invite_codes`

| 字段 | 类型 | 约束 / 说明 |
|---|---|---|
| `user_id` | text | PK；FK `users ON DELETE CASCADE`（一用户一码） |
| `short_code` | text | **UNIQUE**；8 位，字符集 `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`；索引 `idx_user_invite_codes_short_code_lookup` |
| `created_at` | timestamptz | 默认 now() |
| `expires_at` | timestamptz | 可空；**当前代码从不写入（恒为 NULL）**，解析 SQL 判 `expires_at IS NULL OR expires_at > now()` |
| `used_at` | timestamptz | 可空；解析已改纯读（`ResolveInviterFromCode` 为纯 SELECT，全库无写入点），列保留备将来过期策略启用 |

### 4.4 `wx_mp_accounts`（本域只读）

| 字段 | 说明 |
|---|---|
| `user_id` | FK `users ON DELETE CASCADE`；`LinkWxMPAccountByUnionID` 仅认领 `user_id IS NULL` 的记录 |
| `unionid` | 关联键 |
| `subscribed` | bool；登录 / profile 响应中的 `mpSubscribed` 来源 |

### 4.5 数据字典

| 名称 | 取值 | 实现位置 |
|---|---|---|
| 语言 `lang` | `zh` / `zh-Hant` / `en` | `user/handler.go:202` `supportedLangs` |
| 用户类型 | `wechat`（唯一） | migration CHECK |
| 会话 TTL | 滑动 30 天，绝对 90 天 | `middleware/session.go:27-28` |
| 手机绑定日限 | 1 次 / 自然日（Asia/Shanghai），原子条件更新 | `db/sqlc/user.sql` `BindUserPhoneIfAllowed`（调用方 `auth/handler.go`） |
| 手机绑定限流 | 10 次 / 分钟 / IP | `cmd/server/main.go` |
| 试用 VIP id | `vip-trial-0001` | `vip/service.go:20` |
| 免费活动领取 VIP id | `vip-free-0001` | 前端 `GET /vip/free/check` 检查用（`config/index.ts:53`，常量名 `NEW_USER_FREE_VIP_ID` 为历史命名）；商品行由 `vips` 种子（migration 000003）提供 |

## 5. API 契约

> 鉴权列：「公开」= 注册在公开路由组（仅 IP 限流）；「Session」= 需 `Authorization: Bearer <sessionId>`。
> 统一响应包见 §6.6。所有路径为 Go 路由注册路径；SaaS 小程序直连 `https://pro.papafeiji.cn`（`frontend/miniapp/miniprogram/config/index.ts:6`）。

### 5.1 端点登记

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/auth/login` | 公开 | 微信 code 登录 / 自动注册；body `{code, inviter?}`（≤4096B） |
| POST | `/auth/logout` | Session | 删除当前 session |
| GET | `/auth/phone` | Session | 返回 `{phoneNumber, canModifyToday}` |
| POST | `/auth/phone/bind` | Session | body `{code}`（≤4096B）；绑定微信手机号 |
| POST | `/auth/phone/unbind` | Session | body `{code?}`（≤4096B）；解绑手机号 |
| POST | `/auth/inviter` | Session | body `{inviter}`（≤4096B）；补绑邀请人 |
| DELETE | `/auth/account` | Session + IP 限流 5/h | body `{confirmName}`（≤4096B）；注销账号 |
| GET | `/user/profile` | Session | 用户资料（`userinfo.Build`） |
| PUT | `/user/avatar` | Session | body `{fileId}`（≤8KB） |
| PUT | `/user/nickname` | Session | body `{nickName}`（≤8KB） |
| PUT | `/user/lang` | Session | body `{lang}`（≤8KB） |
| GET | `/user/vip` | Session | `{isVip, expireTime}` |
| GET | `/user/common-addresses` | Session | `{addresses:[{name,lat,lon,count}]}`（≤10 条，count 降序） |
| POST | `/user/common-addresses/refresh` | Session | 无 body；重算并返回地址列表 |
| PUT | `/user/common-addresses/{name}` | Session | body `{newName}`（≤8KB）；重命名/合并 + 替换日记地址 |

> 鉴权中间件：`backend/internal/middleware/session.go:279` `SessionMiddleware.Handler`；SaaS 模式挂 Session，开源版（`DEPLOYMENT_MODE=open`）挂 `NewOpenAuthMiddleware`（`backend/cmd/server/main.go:242`）。

### 5.2 请求 / 响应字段

- `POST /auth/login` → `data: {sessionId, newUser, userInfo}`；`userInfo: {id, avatarUrl, nickName, isVip, expireTime, currentFamilyId, mpSubscribed}`。
- `GET /auth/phone` → `{phoneNumber: string|null, canModifyToday: bool}`。
- `GET /user/vip` → `{isVip: bool, expireTime: string|null}`。
- `GET /user/common-addresses` / refresh → `{addresses: [{name, lat, lon, count}]}`。
- `PUT /user/common-addresses/{name}` 重命名/合并时批量替换 `diary_entries.address`（每批 1000）。

### 5.3 错误码映射（本域）

| 场景 | HTTP | `code` | `biz_code` | `message` |
|---|---|---|---|---|
| body 非法 / 字段缺失 | 400 | 4000 | — | 各 handler 文案 |
| 微信 code 无效 | 400 | 4000 | — | `invalid wechat code` |
| 微信服务错误 | 500 | 5001 | — | `wechat login failed` |
| 手机号日限 | 400 | 4000 | — | `phone can only be modified once per day` |
| 手机号已被占用 | 409 | 4090 | `PHONE_ALREADY_BOUND` | `phone already bound` |
| 昵称校验失败 | 400 | 4000 | — | `nickname ...` |
| 语言不在白名单 | 400 | 4000 | — | `unsupported language` |
| 无 / 失效 session | 401 | 4010 | `SESSION_INVALID` | `missing session` / `invalid or expired session` |
| Redis 校验异常 | 500 | 5001 | — | `failed to check session` |
| 资源不存在 | 404 | 4040 | — | 各 handler 文案 |
| 越权 | 403 | 4030 | — | 各 handler 文案 |
| 注销并发 | 429 | 4290 | `OPERATION_IN_PROGRESS` | `operation in progress` |
| IP 限流（注销） | 429 | 4290 | `RATE_LIMITED` | `too many requests` |
| 通用内部错误 | 500 | 5001 | — | 各 handler 文案 |

> `code` 词汇规范见 03-api §3.1（ADR-0008）；对应常量：`backend/pkg/errors/codes.go`（`CodeSuccess=0000`、`CodeBadRequest=4000`、`CodeUnauthorized=4010`、`CodeForbidden=4030`、`CodeNotFound=4040`、`CodeConflict=4090`、`CodeRequestEntityTooLarge=4130`、`CodeTooManyRequests=4290`、`CodeInternalError=5001`）；语义码一律放 `biz_code`。

## 6. 关键实现约束

凡「改了就影响行为」的点。

### 6.1 会话（Redis）

源：`backend/internal/middleware/session.go`。

| Key | 含义 | TTL |
|---|---|---|
| `session:{sessionID}` | value = userID | 滑动 30 天（每次 Get 续期） |
| `session:abs:{sessionID}` | 绝对生命周期标记（value=1） | 固定 90 天，**续期不刷新** |
| `sessions:user:{userID}` | 该用户 sessionID 集合 | 随滑动续期 |

- sessionID：`crypto/rand` 32 字节 → hex（64 字符），不可猜测。
- 创建 / 读取 / 删除 / 全删均为 **Lua 原子脚本**（`createSessionScript` / `getSessionScript` / `deleteSessionScript` / `deleteAllSessionsScript` / `flushAllSessionsScript`）。
- 创建脚本登录时最多清理 50 个过期 session（`smembers` 迭代截断 50，避免阻塞 Redis）。
- `Get` 在 `abs` 不存在时删除 `session:{id}` 并返回 `redis.Nil`；调用方据此返回 401。
- `Delete` / `DeleteAll` 同步删除 `session:abs:*`，避免残留。
- `FlushAllSessions`（SCAN 批量删）仅用于开源版 `OPEN_API_KEY` 变更时强制全体下线。
- Redis 异常时 session 校验 fail-closed：校验失败返回 HTTP 500，不降级放行；`/health/ready` 对 Redis 故障返回 200 并上报 `status: degraded` + `redis: down`（避免 watchdog 重启）。

### 6.2 事务与隔离

- 登录建号：`db.WithTx`（`backend/internal/auth/handler.go`），普通隔离级别。
- 家庭变更 / 注销：`db.WithTxDeferrable`，`pgx.RepeatableRead + Deferrable`，串行化冲突（40001）重试 3 次、间隔 50ms（`backend/internal/db/pool.go` `WithTxDeferrable`）。
- 注销把家庭关系与用户数据放在**同一事务**，DB 提交后不可逆。

### 6.3 锁与幂等

| 锁 / 约束 | Key / 对象 | 机制 | 作用 |
|---|---|---|---|
| 用户级注销锁 | `lock:delete_account:{userID}` | PG advisory lock（无 TTL） | 同一用户并发注销互斥，未抢到 → `ErrOperationInProgress`（429） |
| 家庭级锁 | `lock:family:{familyID}` | PG advisory lock（无 TTL） | 家庭变更串行；`TryLocks` 按字典序 |
| 邀请人月度奖励锁 | PG advisory `hashtext('inviter_reward:'||inviterID)` | 事务级 | 防并发超发 |
| `user_invites.user_id` UNIQUE | — | — | 一个用户最多一个邀请人 |
| `users.phone_number` 部分唯一 | — | — | 手机号全局唯一 |
| `users.open_id` / `unionid` 唯一 | — | — | 同微信主体不重复建号 |
| `users` 条件更新 | `UpdateUserInvitedBy ... WHERE invited_by IS NULL OR ''` | — | 补绑幂等 |
| `MarkInviterRewarded` | `... WHERE reward_inviter_at IS NULL` | — | 先标记后发放，防双发 |

### 6.4 限流阈值

| 限流器 | 阈值 | 作用范围 | 实现位置 |
|---|---|---|---|
| 全局公开限流 | 60 次 / 分钟 / IP | 所有公开路由 | `main.go:209` |
| 注销接口限流 | 5 次 / 小时 / IP | `DELETE /auth/account` | `main.go:251` |
| `/invite/resolve` 限流 | 60 次 / 小时 / IP | 邀请码解析 | `invite/handler.go` |
| `/health/live`、`/health/ready`、`/health` 限流 | 30 次 / 分钟 / IP | 健康检查 | `main.go:200-203` |

- 限流算法：进程内滑动窗口（`slidingWindowLimiter`），不跨进程共享；Redis 挂掉不影响限流。
- 真实 IP 仅在 `RemoteAddr` 命中 `TRUSTED_PROXY_CIDR` 时才解析 `X-Forwarded-For` / `X-Real-IP`（防伪造绕过）。解析规则：**从 XFF 右端向左取第一个「可解析且非私网」的 IP**（跳过空段/非法/私网）；XFF 无命中时回退 `X-Real-IP`（同样要求非私网）；再回退 `RemoteAddr`。
- 限流命中返回 429，`code="4290"` + `biz_code="RATE_LIMITED"`（词汇终态见 03-api §3，ADR-0008）。

### 6.5 JSON body 上限

| 端点族 | maxBytes |
|---|---|
| `/auth/login`、手机号绑定 / 解绑、补绑、注销 | 4096 |
| `/user/*` | 8192 |
| 家庭 / 邀请相关 | 64KiB（详见 PP-02D） |

超限返回 **413 + `code="4130"`**（`ReadJSONBody` 返回 `*http.MaxBytesError`，由 `middleware.JSONBodyError` 统一映射为 `request body too large`）；JSON 非法返回 400 + `code="4000"`（`invalid request body`）。

### 6.6 统一响应包

源：`backend/internal/middleware/response.go:16`。

字段全集：`{ code, biz_code?, message, data?, extra?, count?, nextCursor?, request_id }`。成功响应只写 `code="0000"`、`message="ok"` 与业务 `data`；`biz_code` 仅失败时出现，`extra`/`count`/`nextCursor` 仅由对应构造器按需携带（`JSONWithExtra` / `JSONWithPagination` / `JSONWithExtraAndCount`）。

- 成功：`code="0000"`、`message="ok"`。
- 失败：`code` 为 §3.1 固定枚举；语义码放 `biz_code`（如 `SESSION_INVALID`、`RATE_LIMITED`，见 03-api §3.2）。
- `request_id`：优先客户端 `X-Request-ID`（经 `util.SanitizeRequestID`），否则生成。

### 6.7 敏感的日志 / 响应约束

- 微信 `secret` 与 `access_token` 打日志前脱敏（`auth/wechat.go:21` `accessTokenLeakRe`、`sanitizeWechatError`）。
- openid / unionid / sessionID 日志使用 `util.MaskID`（保留首尾 3 字符）。
- 响应体不得出现 `session_key/openid/unionid/phone_number`。

### 6.8 后台任务（本域）

- 新用户默认头像 marker 生成：`safe.Go`，超时 `avatar.GenerateMarkerTimeout`（`auth/handler.go:648`）。
- 注销后清理：`safe.Go`，`context.Background()+5min`（`auth/handler.go:473`）。
- 注销后未被清理的物理文件由后台孤儿文件清理任务回收（其它分册）。
- 注销事务提交后才异步删除相关成员 session，提交后数秒内这些 session 仍可能通过校验。
- 本域**没有**独立的定时任务；残留 session 依赖 TTL 自然过期。

## 7. 前端接入

源：`frontend/miniapp/miniprogram/`。

| 页面 / 模块 | 行为 |
|---|---|
| `utils/auth.ts` | `login()`：本地有 session 先 `GET /user/profile` 校验，失败清 session；否则 `wx.login` → `POST /auth/login`（带 `inviter`，超时 20s）；成功存 sessionId、写 `baseInfo`；`newUser` → 前端兜底调 `POST /vip/new-user`（trial 已于注册事务发放，正常流程 409 `TRIAL_VIP_ALREADY_CLAIMED` 吞掉）+ 跳 Guide；有 `pendingLinkId` → 跳 Family |
| `utils/http.ts` | 每个请求加 `Authorization: Bearer <sessionId>` 与 `Accept-Language`；HTTP 401 → `clearSessionId()`（`skipAuthExpire` 场景除外）；private 模式另加 `X-Private-Api-Key` |
| `utils/storage.ts` | sessionId 键 `papafeiji:sessionId`；`pendingLinkId`（7 天过期）；`pendingInviter` |
| `pages/index/index.ts` | `onLoad` 解析 `option.inviter` / `option.scene`；`/invite/resolve` 最多等 1.5s；已有登录态时调 `POST /auth/inviter` 补绑；`ensureLogin` 先用 `/user/profile` 校验 |
| `pages/User/User.ts` | 展示入口（进入即 `request.login`） |
| `pages/Set/Set.ts` | 并行拉 `/auto-record/config`、`/auth/phone`、`/user/profile`；昵称 `PUT /user/nickname`；头像上传 → `PUT /user/avatar`；语言切换 `PUT /user/lang`；解绑 `POST /auth/phone/unbind`；绑定 `POST /auth/phone/bind` |
| `pages/sub/About/About.ts` | 注销：输入昵称 → `DELETE /auth/account`（`deleting` 防重入），成功后本地清理并 `reLaunch` 首页 |
| `config/index.ts` | SaaS 默认直连 `https://pro.papafeiji.cn`；开发环境 `http://localhost:8080`；private 模式经 Worker `https://api.pathmemos.com` |

## 8. 运维与任务

### 8.1 环境变量（本域相关）

| 变量 | 用途 | 默认 / 必填 |
|---|---|---|
| `DATABASE_URL` | PostgreSQL | 必填 |
| `REDIS_ADDR` | Redis（session / token 缓存） | 必填（`NewSessionManager(nil)` 会 panic） |
| `WECHAT_APPID` / `WECHAT_SECRET` | 小程序登录 / 手机号 / 二维码 | SaaS 模式必填（成对回退内置值仅限开源版） |
| `DEPLOYMENT_MODE` | `saas`（默认）/ `open` | 开源版启用 OpenAuth 与自动 migration |
| `TRUSTED_PROXY_CIDR` | 可信代理 CIDR（逗号分隔） | 为空则直接信任 `RemoteAddr`；非法/空段会启动校验失败（`config.validate`） |
| `WORKER_SECRET` | 经 api-worker 中转流量的共享密钥（`X-Worker-Secret`） | **SaaS 必填**（空则启动校验失败）；open 模式必须留空 |
| `OPEN_API_KEY` | 开源版 API Key | open 模式使用 |
| `STORAGE_LOCAL_PATH` | 本地文件根目录 | 默认 `/opt/pathmemos/uploads` |
| `OSS_ACCESS_KEY_ID` / `OSS_ACCESS_KEY_SECRET` / `OSS_ENDPOINT` / `OSS_BUCKET` / `OSS_PUBLIC_URL` | OSS 存储 | all-or-nothing：五项中任一非空（含单独设置 `OSS_PUBLIC_URL`）即要求 `OSS_ACCESS_KEY_ID`/`OSS_ACCESS_KEY_SECRET`/`OSS_ENDPOINT`/`OSS_BUCKET` 四项齐备（否则启动校验失败，见 config.validate）；`OSS_PUBLIC_URL` 为文件对外基址回退（`STORAGE_PUBLIC_BASE_URL` 优先）；均空则本地存储兜底 |

### 8.2 Redis key（本域）

| Key | 值 | TTL |
|---|---|---|
| `session:{id}` | userID | 滑动 30 天 |
| `session:abs:{id}` | 1 | 90 天 |
| `sessions:user:{uid}` | sessionID 集合 | 滑动 30 天 |
| `wechat:access_token:{appID}` | access_token | 110 分钟（`auth/wechat.go:190`） |
| `ai:family_summary:{familyID}` | 家庭汇总缓存（注销时失效） | 由 AI 模块管理 |

### 8.3 运维删除工具

- `deploy/delete-user.sh <phone>`：本地编译 `backend/cmd/admin` 并上传，远端以 app 镜像运行一次性容器执行（`docker compose run --rm --no-deps --entrypoint <admin 二进制> app`，`TARGET_PHONE` 经 `-e` 注入）；默认先备份数据库（可 `--no-backup`），二次确认可 `--force` 跳过。
- `cmd/admin/main.go`：按 `phone_number` 查用户 → `sessions.DeleteAll` → `familyService.DeleteAccount` → 2 分钟超时内 `CleanupAfterAccountDeletion`。删除失败（含 Redis）**不降级、直接退出**。
- `cmd/admin jobs status`：读 Redis `job:last_success:{task}` / `job:last_failure:{task}` / `job:fail_streak:{task}` 打印每任务状态；`cmd/admin job run <name>`：写 `job:trigger:<name>`，由常驻 app 每分钟消费执行同名任务（`KnownJobNames` 共 10 项）。`deploy/delete-user.sh` 未封装这两个子命令。

### 8.4 迁移与门禁

- 迁移基线为 `000001_baseline`（已 squash），当前最大编号 `000011_user_invites_inviter_set_null`。本域相关表（`users`/`user_invites`/`user_invite_codes`/`wx_mp_accounts`）定义均在 000001 基线；000004 仅新增 `client_ops_logs`；000008 变更 `user_invites`（补 `user_open_id` 与部分唯一索引、`user_id` 改 SET NULL 可空）；000010 补 `user_vip_claims.user_id` 可空；000011 将 `user_invites.inviter_id` 改 SET NULL 可空。
- 本域相关表变更须配对 `.down.sql` 并同步 L4 文档；改 SQL 后跑 `make sqlc-generate` + `make check-sqlc-sync`。
