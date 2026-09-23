# PP-02D 家庭与邀请（L2）

> 层级：L2 领域分册｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联 ADR：ADR-0010（邀请链接长期有效无撤销）、ADR-0012（owner 整家合并风险已接受）
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。

## 1. 背景与目标

本域负责「多人共享同一份日记」与「邀请他人加入」：

- 家庭模型：每个用户一个个人家庭（personal family）；加入普通家庭后当前家庭（current family）指向该家庭。
- 家庭成员查看、创建家庭、退出、移除成员、解散。
- 家庭邀请链接（linkId = familyId）的生成与加入（含 owner 整家合并）。
- 个人邀请短码与分享二维码的生成、解析；邀请列表。
- 家庭 / 邀请相关的封面迁移与 AI 家庭汇总缓存失效。

**不属于本域**：

- 登录 / 注册 / 邀请奖励的发放细节（`applyInviteRewardsWithTx`）→ PP-02A；本域只登记「链接加入奖励」`grantJoinReward`（同一奖励规则的另一入口）。
- 家庭共享日记的读取 / 封面刷新 / AI 汇总内容 → 日记、AI 分册。
- 微信公众号关注 / 订阅状态 → wxmp 分册。

**关键实现位置**：

| 文件 | 职责 |
|---|---|
| `backend/internal/family/handler.go` | 家庭 HTTP 路由与错误映射；家庭邀请链接；链接加入奖励入口 |
| `backend/internal/family/service.go` | 家庭事务、分布式锁、封面迁移、注销家庭处理 |
| `backend/internal/family/errors.go` | 家庭 sentinel error 定义 |
| `backend/internal/invite/handler.go` | 个人邀请短码解析 / 列表 / 邀请码加入家庭 / 二维码 |
| `backend/internal/invite/qrcode.go` | 短码生成、微信小程序码拉取、图片合成、文件记录与缓存 |
| `backend/internal/db/sqlc/family.sql`、`invite.sql`、`file.sql` | SQL 语义 |
| `backend/migrations/000001_baseline.up.sql` | families / family_members / family_daily_covers / user_invite_codes / user_invites 表 |
| `frontend/miniapp/miniprogram/pages/Family/Family.ts`、`pages/Invite/Invite.ts`、`pages/index/index.ts` | 前端接入 |

## 2. 领域级架构决策

| # | 决策 | 理由 | 关联 ADR |
|---|------|------|---------|
| D1 | 家庭模型：`families(is_personal)` + `family_members`；`users.personal_family_id` 恒定指向个人家庭，`users.current_family_id` 指向当前家庭 | 个人家庭是退出/解散/注销后的稳定落点 | 无 |
| D2 | `family_members` 对 `(family_id, user_id)` 唯一，并对 `user_id` 有 DEFERRABLE INITIALLY DEFERRED 唯一约束 | 一个用户同时只在一个家庭；延迟约束允许事务内先删后插 | 无 |
| D3 | 家庭成员上限 **6 人**；owner 携原家庭加入时按「源 + 目标总数 ≤ 6」校验 | 控制共享规模 | 无 |
| D4 | 家庭变更使用家庭级 PostgreSQL advisory lock `lock:family:{id}`（ADR-0005，无 TTL），多锁按字典序获取；未抢到立即失败（429） | 成员上限是「先 count 再 insert」的跨请求竞态，RR 重试无法覆盖 | 无 |
| D5 | **owner 携家庭加入他人家庭 = 整家合并**：源家庭全体成员迁入目标、源家庭解散、封面迁移。前端打开链接即自动加入、无二次确认；「owner 被诱导整家合并」的钓鱼风险为**已接受取舍**（ADR-0012，熟人小家庭场景不增加防护） | `/family/invite-link/join`；代码注释声明这是预期行为 | ADR-0012 |
| D6 | 邀请短码 8 位（字符集 `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`），一用户一码、永久有效、可重复解析；解析为纯读（`used_at` 写副作用已移除，列保留待将来过期策略启用） | 稳定分享码，简单优先 | 无 |
| D7 | 家庭邀请链接直接使用 `linkId = familyId`；任何成员均可生成；**长期有效、无撤销**（已接受取舍，ADR-0010） | 无需额外邀请令牌表；familyId 为 UUID v7 | ADR-0010 |
| D8 | 家庭变更 / 加入 / 解散 / 链接奖励事务使用 `WithTxDeferrable`（邀请二维码文件记录替换用 `WithTx`）；缓存失效与物理文件删除在事务提交之后 | 非幂等副作用在事务提交后执行 | 无 |
| D9 | 封面迁移与成员迁移在同一事务内完成 | 保证数据一致，避免孤儿封面 | 无 |

## 3. 核心流程（用户故事 + 时序）

> 编号前缀 `F-` 稳定不变。

### F-1 查看家庭信息

**故事**：用户进入家庭页，看到当前家庭与成员列表。

时序（源：`family/service.go:77` `GetFamily`、`family/handler.go:57` `GetFamily`）：

1. `GetUserByID` 取 `current_family_id`；为空 → 返回空 `FamilyInfo{}`（200，不是 500）。
2. `GetFamilyByID` 取 `is_personal`。
3. `ListFamilyMembers`：`JOIN users`，`ORDER BY fm.joined_at ASC, fm.user_id ASC`。
4. 逐成员组装 `{userId, avatarUrl, nickName, role, joinedAt}`；`OwnerID` 取 `role='owner'` 成员；若 `is_personal` 则 `OwnerID` 强制为当前用户。
5. handler 输出 `{familyId, ownerId, isPersonal, members}`；`joinedAt` 格式 `2006-01-02T15:04:05-07:00`。

**AC**

- 成员顺序为 `joined_at` 升序、同时间按 `user_id` 升序。
- 个人家庭响应 `isPersonal=true` 且 `ownerId == 当前登录用户 id`。
- `current_family_id` 为空时返回 200 且 `familyId=""`、`members=[]`。
- 头像 URL：`users.avatar` 有值用原值；否则默认头像基址 + userId；基址为空则为 `null`。
- 任何 service 错误（含家庭不存在）→ HTTP 500 `failed to get family info`。

### F-2 创建家庭

**故事**：用户在个人家庭中点击「创建家庭」，得到一个新的共享家庭。

时序（`family/service.go:125` `CreateFamily`）：

1. 生成新 `familyID` / `membershipID`（UUID v7）。
2. `WithTxDeferrable` 内：读用户；若 `current_family_id != personal_family_id` → `ErrAlreadyInFamily`；`CreateFamily(is_personal=false)`；删除个人家庭 membership；`UpsertFamilyMembership(role='owner')`；`UpdateUserCurrentFamily`。
3. 唯一键冲突 → `ErrAlreadyInFamily`。
4. handler 返回 `{familyId}`。

**AC**

- 成功后 `users.current_family_id` = 新家庭 id、`users.personal_family_id` 不变、`family_members` 中新家庭仅一条 owner 记录。
- 已在普通家庭再次调用 → HTTP 409，`code="4090"`，`biz_code="ALREADY_IN_FAMILY"`。
- 该流程**不加分布式锁**（事务内重读 + `user_id` 唯一约束兜底）。

### F-3 生成 / 分享家庭邀请链接

**故事**：任何家庭成员都能把家庭邀请链接分享给好友。

时序（`family/handler.go:180` `CreateInviteLink`）：

1. `GetFamily`；`FamilyID == ""` → 404 `family not found`。
2. 若 `IsPersonal` → 先 `CreateFamily`，`linkID = 新 familyID`；否则 `linkID = 当前 familyID`。
3. 返回 `{linkId}`。

**AC**

- 个人家庭调用 → 自动创建普通家庭并返回其 id，当前家庭随之变为该 id。
- 已在普通家庭 → 直接返回当前 `familyId`，不新建家庭。
- 不限制调用者角色（owner / member 均可）。

### F-4 通过邀请链接加入家庭

**故事**：好友打开分享链接，登录后加入该家庭。

时序（`family/handler.go:215` `JoinByInviteLink` → `family/service.go:187` `JoinFamily`）：

1. body `{linkId}`（≤64KiB）；`linkId` 为空 → 400。
2. `GetFamilyByID` 失败 → `ErrFamilyNotFound`；`is_personal=true` → `ErrTargetIsPersonalFamily`。
3. 当前用户已在目标家庭 → `ErrAlreadyInTargetFamily`。
4. 计算锁集合：`lock:family:{target}`，若源家庭非个人家庭再加 `lock:family:{source}`；`TryLocks` 按字典序（PG advisory lock，ADR-0005）；未抢到 → `ErrOperationInProgress`。
5. `WithTxDeferrable`：重算 current/personal；若 `current==target` 返回 `ErrAlreadyInTargetFamily`；判断是否源家庭 owner（**源家庭为个人家庭时不走 owner 合并路径**：一律按普通成员路径迁移本人，个人家庭不删除、不加 `lock:family:{source}` 锁——每个新用户当前家庭即个人家庭，此豁免是加入主流程的必经分支）；`CountFamilyMembers(target)`；owner 时再取 source count，`target+source > 6` → `ErrFamilyFull`；非 owner 时 `target >= 6` → `ErrFamilyFull`；执行 `joinFamilyTx`。
6. `joinFamilyTx`：
   - **源 owner**：`BatchUpsertFamilyMembership`（role='member'）把源家庭全员迁入目标 → `UpdateUsersCurrentFamily` → 迁移源家庭封面（`MigrateFamilyDailyCovers`，仅 `cover_type != 'default'`）→ `DeleteFamilyDailyCovers` → 删除源 memberships → 删除源家庭。
   - **普通成员**：删除本人在源家庭的 membership → 迁移本人封面（`MigrateUserDailyCoversToFamily`）→ 插入目标 membership（role='member'）→ 更新 current family。
7. 事务提交后失效源 / 目标家庭的 `ai:family_summary:{familyID}` 缓存。
8. handler 成功后调用 `grantJoinReward`（见 F-5），返回 200。

**AC**

- 已在目标家庭重复点击 → HTTP 200 幂等。
- 目标为个人家庭 → HTTP 403，`code="4030"`，`biz_code="TARGET_IS_PERSONAL_FAMILY"`。
- 家庭已满 → HTTP 409，`biz_code="FAMILY_FULL"`。
- 未抢到家庭锁 → HTTP 429，`biz_code="OPERATION_IN_PROGRESS"`。
- 源家庭 owner 加入 → 源家庭被删除、源全员 `current_family_id` = 目标家庭、源封面迁移到目标。
- 普通成员加入 → 本人 `current_family_id` = 目标家庭，原家庭其他成员不变。
- 个人家庭用户加入 → 本人迁移至目标家庭，个人家庭保留不删除。
- 加入成功后目标家庭 `family_members` 中该用户 `role='member'`。
- 链接 `linkId` 即目标 `familyId`，自生成起持续有效；任何已登录用户持有该 `linkId` 即可加入，无需邀请人确认（长期有效、无撤销为已接受取舍，见 ADR-0010）。**若当前用户是源家庭 owner，加入即触发整家合并且不可逆**（钓鱼/误点风险已接受，ADR-0012）。

### F-5 链接加入奖励（`grantJoinReward`）

**故事**：新用户通过家庭邀请链接首次加入，双方获得 VIP 天数奖励。

时序（`family/handler.go:254` `grantJoinReward`，在 `JoinByInviteLink` 成功后同步调用）：

1. 若当前用户已有 `user_invites` 记录 → 直接返回（不奖励）。
2. 读用户；`time.Since(created_at) > 7 天` → 返回（与 /auth/inviter 窗口统一：原 5 分钟与 7 天并存无决策依据，且惩罚弱网慢扫码用户）。
3. `GetFamilyOwner(目标家庭)`；失败 / 空 / owner == 自己 → 返回。
4. `WithTxDeferrable`：再次检查 user_invites；确认 owner 存在；插入 `user_invites`（含 `user_open_id`）；先 `MarkInviteeRewarded`（execrows，`WHERE reward_invitee_at IS NULL`；openid 撞 `uq_user_invites_user_open_id` 部分唯一索引 23505 时仅 Info 日志跳过）→ 标记成功（markRows>0）才被邀请人 +3 天 VIP；`LockInviterReward`；`CountInviterMonthlyRewardDays`；`< 14` 时 `MarkInviterRewarded` + 邀请人 +7 天。
5. 任何失败仅 `slog.WarnContext`，不影响主流程 200。

**AC**

- 注册 7 天内首次通过链接加入 → 被邀请人 +3 天（按 openid 终身一次，`user_invites.user_open_id` 部分唯一索引兜底）、邀请人 +7 天（当月上限 14 天）。
- 已有邀请关系 / 注册超过 7 天 / 不存在 owner → 不发放，接口仍 200；被邀请奖励命中 openid 终身已领时跳过 +3，inviter 侧照常。
- 奖励事务失败不回滚家庭加入、不重试，也不向客户端报错。

### F-6 退出家庭

**故事**：普通成员主动退出当前家庭，回到个人家庭。

时序（`family/service.go:403` `LeaveFamily`）：

1. 读用户；`current == personal` → `ErrNotInNormalFamily`。
2. `GetFamilyOwner`；owner == 自己 → `ErrOwnerCannotLeave`；无 owner 记录（`ErrNoRows`）WARN 后放行。
3. 加 `lock:family:{current}`。
4. `WithTxDeferrable` → `leaveToPersonalTx`：删除当前 membership → 若 `personal_family_id` 非空则以 owner 重入个人家庭 → 迁移本人封面到个人家庭 → `UpdateUserCurrentFamily(personal)`。
5. 失效原家庭 summary 缓存。

**AC**

- owner 调用 → HTTP 403，`biz_code="OWNER_CANNOT_LEAVE_FAMILY"`。
- 已在个人家庭 → HTTP 400（无 `biz_code`）。
- 成功后 `current_family_id = personal_family_id`，个人家庭中 role='owner'。
- `personal_family_id` 为空时跳过重入，`current_family_id` 置 NULL（B-1 修复：空串 Valid=true 会命中 families 外键返回 500，与 dissolveFamilyTx 的 NULL 写法一致）。

### F-7 移除成员（仅 owner）

**故事**：家庭 owner 把某个成员移出家庭。

时序（`family/service.go:447` `RemoveMember`）：

1. `ownerID == targetUserID` → `ErrCannotRemoveSelf`。
2. 读 owner；`owner.current == owner.personal` → `ErrNotInNormalFamily`。
3. `GetFamilyOwner` != ownerID → `ErrNotOwner`。
4. 加家庭锁；`WithTxDeferrable`：读 target；`target.current != owner.current` → `ErrTargetNotInFamily`；`familyOwner == targetUserID` → `ErrCannotRemoveOwner`；`leaveToPersonalTx(target)`。
5. 失效家庭 summary 缓存。

**AC**

- 非 owner 调用 → HTTP 403（无 `biz_code`）。
- 移除自己 → HTTP 403 `biz_code="CANNOT_REMOVE_SELF"`；移除 owner → 403 `biz_code="CANNOT_REMOVE_OWNER"`。
- 目标不在同一家庭 → HTTP 404（`code="4040"`）。
- 被移除成员 `current_family_id` = 其个人家庭。

### F-8 解散家庭（仅 owner）

**故事**：owner 解散普通家庭，所有成员回到各自个人家庭。

时序（`family/service.go:501` `DissolveFamily` → `dissolveFamilyTx`）：

1. 读 owner；`current == personal` → `ErrCannotDissolvePersonal`。
2. `GetFamilyOwner` != ownerID → `ErrNotOwner`。
3. 加家庭锁；`WithTxDeferrable` → `dissolveFamilyTx`：`ListFamilyMemberPersonalFamilies` → 批量 `BatchUpsertFamilyMembershipOwner` 回个人家庭 → `BatchMigrateUsersDailyCoversToPersonal`（先于删除封面）→ `BatchUpdateUsersCurrentFamilyToPersonal` → 对无个人家庭的成员清空 `current_family_id` → `DeleteFamilyDailyCovers` → `ClearTrajectoryFamilyID`（实为清理 **files 表系统文件** 的 `metadata->>'family_id'`，即轨迹图文件的元数据键；`auto_record_trajectories` 表无 family_id 列；该清理匹配全部 `file_type='system'` 且 `family_id` 匹配的文件，含邀请二维码文件——属预期：旧二维码元数据被清后仅影响旧图查询，旧物理文件由 7 天系统文件清理任务兜底）→ `DeleteFamilyMembers` → `DeleteFamily`。
4. 失效家庭 summary 缓存。

**AC**

- owner 无法解散个人家庭 → HTTP 403（无 `biz_code`）。
- 非 owner → HTTP 403。
- 成功后原家庭成员均回到各自个人家庭（role='owner'），普通家庭行被删除。
- 无个人家庭的成员 `current_family_id` 被清空，家庭仍能删除（不被 ON DELETE RESTRICT 阻塞）。

### F-9 个人邀请短码解析

**故事**：好友扫描分享二维码或点击带 scene 的链接，前端解析出邀请人。

时序（`invite/handler.go` `Resolve` → `invite/qrcode.go` `ResolveInviterFromCode`）：

1. 公开路由 `GET /invite/resolve?code=XXXX`，叠加 60 次/小时/IP 的 `resolveLimiter`。
2. `code` 空或不匹配 `^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{8}$` → 400 `invalid invite code`（大小写敏感，必须大写）。
3. `ResolveInviterFromCode`：`strings.ToUpper(TrimSpace(code))`；SQL `SELECT user_id FROM user_invite_codes WHERE short_code=$1 AND (expires_at IS NULL OR expires_at > now())`（纯读，无写副作用）。
4. `ErrNoRows` → 返回空 → 404 `invite code not found`；SQL 错误 → 500。
5. 成功 → 200 `{userId}`。

**AC**

- 8 位合法且存在 → 200 `{userId: 邀请人id}`（纯读，无写副作用）。
- 8 位合法但不存在 / 已过期 → 404。
- 长度或字符集不合法（含小写）→ 400。
- `expires_at` 当前恒为 NULL，故邀请码实际永久有效、可无限次解析。
- 同 IP 1 小时第 61 次 → HTTP 429 `code="4290"` + `biz_code="RATE_LIMITED"`。

### F-10 邀请列表

**故事**：邀请人查看自己邀请过的人及其是否已加入自己的家庭。

时序（`invite/handler.go` `List`）：`ListUserInvitesByInviter(inviterID)`：`user_invites JOIN users`，`ORDER BY created_at DESC LIMIT 100`；`joined` = 被邀请人 `current_family_id` 非空、不等于其 personal、且等于邀请人的 `current_family_id`。

**AC**

- 返回 `{list: [{userId, nickName, avatarUrl, joined}]}`，最多 100 条、按邀请时间倒序。
- 被邀请人已加入邀请人当前家庭 → `joined=true`；否则 `false`。
- DB 错误 → 500。

### F-11 个人邀请二维码生成

**故事**：用户在小程序内生成带二维码的分享图并保存 / 分享。

时序（`invite/handler.go` `QRCode` → `invite/qrcode.go` `generate`）：

1. body 允许为空（`ReadJSONBodyAllowEmpty`，≤64KiB），`{raw?: bool}`。
2. 读 Redis 缓存 `invite:qrcode:{userID}`（raw 时加 `:raw` 后缀），TTL 6 天；命中直接返回 URL。
3. 未命中：抢 `invite:qrcode:gen:{userID}` PG advisory lock（ADR-0005，无 TTL）；未抢到则轮询缓存 10 次 × 100ms 后 fallthrough。
4. `ensureShortCode`：`GetUserInviteCode` 命中即复用；否则 `util.NewShortCode(8)` 插入；`user_id` 唯一冲突则回查已有码复用；`short_code` 唯一冲突则换码重试（最多 3 次）。
5. `fetchWxaCode`：POST `getwxacodeunlimit`，`scene=短码`、`page="pages/index/index"`、`is_hyaline=true`、`width=800`；响应体上限 2MB；微信 `errcode` 40001/42001 时 `ClearAccessToken` 后重试 1 次，其它错误直接失败。
6. `raw=true` 直接保存 PNG；否则 `composite` 合成到背景图 `/app/assets/invite-share-cover.png`（二维码边长 = 背景宽 × 0.20，最小 120px；margin 40、右下内缩 300；JPEG quality 90）。
7. `storage.SaveSystemWithName(..., 短码)` 保存；事务内查询同 raw 类型的旧文件记录 → `CreateFile(file_type='system', metadata={family_id, raw})` → `BatchDeleteFiles` 删除旧记录；提交后删除旧物理文件（跳过与新建同路径的文件）。
8. 写回 Redis URL 缓存（6 天）；返回 `{url}`。

**AC**

- 首次调用生成并缓存；6 天内再次调用返回相同 URL（不再调微信）。
- 生成失败（微信 / 合成 / 存储）→ HTTP 500 `failed to generate invite qrcode`。
- raw 与 合成图 各自独立缓存与文件记录，互不影响。
- 同一用户并发生成不会互删对方刚写入的文件（按用户锁 + 跳过同路径）。
- 二维码在线 URL 约 7 天有效（system 文件清理任务回收超过 7 天未更新的文件）；Redis URL 缓存 6 天先期过期，过期后访问重新生成新图。
- 文件记录 `file_type='system'`、`metadata.family_id` 存在，用于与日记封面文件区分。

### F-12 家庭配置（现状说明）

**现状（代码事实）**：产品不需要家庭级配置；后端**没有**家庭级配置端点，也没有任何 `familyConfig` 数据模型。前端 `utils/auth.ts` 的 `getFamilyConfig` 调用 `GET /auto-record/config`（自动记录开关，属 PP-02C，用户级而非家庭级）并写入 `baseInfo.familyConfig`；`pages/Set/Set.ts` 调用后忽略返回值，不存在 `data.familyConfig`，界面未渲染该值。

**约定**

- 不存在形如 `/family/config` 的路由。
- `/auto-record/config` 返回 `{enabled}`，依据 `users.auto_record_enabled` 与 VIP 状态，与家庭无关。
- 无家庭级配置能力。

## 4. 数据模型

源：`backend/migrations/000001_baseline.up.sql`。

### 4.1 `families`

| 字段 | 类型 | 约束 / 说明 |
|---|---|---|
| `id` | text | PK；UUID v7 |
| `is_personal` | boolean | 默认 false；true = 个人家庭 |
| `created_at` | timestamptz | 默认 now() |

### 4.2 `family_members`

| 字段 | 类型 | 约束 / 说明 |
|---|---|---|
| `id` | text | PK |
| `family_id` | text | FK `families(id) ON DELETE CASCADE` |
| `user_id` | text | FK `users(id) ON DELETE CASCADE`；**UNIQUE**（`uq_family_members_user_id`，DEFERRABLE INITIALLY DEFERRED） |
| `role` | text | 默认 `'member'`；CHECK ∈ {owner, member} |
| `joined_at` | timestamptz | 默认 now() |

其他约束：`UNIQUE(family_id, user_id)`（`family_members_family_id_user_id_key`）；索引 `idx_family_members_family_role (family_id, role)`。

### 4.3 `family_daily_covers`

| 字段 | 类型 | 约束 / 说明 |
|---|---|---|
| `family_id` | text | PK 组成；FK `ON DELETE CASCADE`（000009 补齐） |
| `record_date` | date | PK 组成 |
| `cover_file_id` | text | FK `files(id) ON DELETE SET NULL` |
| `cover_type` | text | 默认 `'default'`；CHECK ∈ {image, trajectory, default, manual} |
| `manual_cover_file_id` | text | FK `files(id) ON DELETE SET NULL` |
| `updated_at` | timestamptz | 默认 now() |

### 4.4 `user_invite_codes` / `user_invites`

字段定义与约束见 PP-02A §4.2 / §4.3（同一批迁移）。本域使用要点：

- `user_invite_codes`：一人一码、`short_code` 唯一、`expires_at` 恒 NULL、`used_at` 列保留但不再写入（解析已改纯读）。
- `user_invites`：`user_id` 唯一（一人一个邀请人），`reward_inviter_at` / `reward_invitee_at` 控制奖励幂等；`inviter_id` 可空、FK `SET NULL`（000011：邀请人注销保留被邀请人墓碑行，`user_open_id` 部分唯一索引继续阻断重复领取被邀请奖励）。

### 4.5 `files`（邀请二维码使用）

| 字段 | 说明 |
|---|---|
| `file_type='system'` | 邀请二维码文件类型 |
| `metadata` | JSON：`{family_id, raw:"true"|"false"}`；用于区分 raw / 合成图并清理旧图 |
| `created_by` | 邀请人的 userId |

### 4.6 数据字典

| 名称 | 取值 | 实现位置 |
|---|---|---|
| 成员角色 `role` | `owner` / `member` | migration CHECK |
| 家庭类型 | `is_personal` true / false | `families` |
| 封面类型 `cover_type` | `image` / `trajectory` / `default` / `manual` | migration CHECK |
| 邀请短码 | 8 位，字符集 `ABCDEFGHJKLMNPQRSTUVWXYZ23456789` | `pkg/util/short_code.go:10`、`invite/handler.go` |
| 家庭成员上限 | 6 | `family/service.go` 两处 `> 6` / `>= 6` |
| 家庭锁 | PostgreSQL advisory lock（无 TTL，ADR-0005） | `family/service.go` |
| 二维码生成锁 | PostgreSQL advisory lock（无 TTL，ADR-0005） | `invite/qrcode.go` |
| 二维码缓存 TTL | 6 天 | `invite/qrcode.go` |
| 链接加入奖励窗口 | 注册后 7 天（与 /auth/inviter 统一） | `family/handler.go` |
| 邀请人月度奖励事务锁 | PG 事务级 advisory lock `hashtext('inviter_reward:'||inviterID)` | `invite.sql`、`family/handler.go` |
| 每日封面触发器 | `trg_family_daily_covers_fix_type` 调 `fix_cover_type_on_null_fk()`，按 `cover_type` 与实际文件一致性降级 | `migrations/000001_baseline`、`000002` |
| 解析限流 | 60 次 / 小时 / IP | `invite/handler.go` |

## 5. API 契约

> 全部路径为 Go 路由注册路径；家庭与邀请均为 Session 鉴权（`/invite/resolve` 除外）。
> 统一响应包见 PP-02A §6.6。

### 5.1 端点登记

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/family` | Session | 返回 `{familyId, ownerId, isPersonal, members[]}` |
| POST | `/family` | Session | 创建普通家庭，返回 `{familyId}` |
| POST | `/family/invite-link` | Session | 生成 / 复用家庭邀请链接，返回 `{linkId}` |
| POST | `/family/invite-link/join` | Session | body `{linkId}`（≤64KiB）；加入家庭 |
| POST | `/family/leave` | Session | 退出当前家庭 |
| DELETE | `/family/members/{userId}` | Session | owner 移除成员 |
| DELETE | `/family` | Session | owner 解散家庭 |
| GET | `/invite/resolve?code=` | 公开（60/h/IP） | 解析 8 位邀请短码，返回 `{userId}` |
| GET | `/invite/list` | Session | 邀请列表，返回 `{list:[...]}`（≤100） |
| POST | `/invite/qrcode` | Session | body 可空 `{raw?:bool}`（≤64KiB）；返回 `{url}` |

### 5.2 响应字段

- `GET /family`：`members` 项为 `{userId, avatarUrl, nickName, role, joinedAt}`。
- `POST /family`：`{familyId}`。
- `POST /family/invite-link`：`{linkId}`（等于 familyId）。
- `GET /invite/list`：`list` 项为 `{userId, nickName, avatarUrl, joined}`。
- `POST /invite/qrcode`：`{url}`（本地/CDN 图片地址）。

### 5.3 家庭错误码映射（`family/handler.go`）

| 场景 | 触发 | HTTP | `code` | `biz_code` |
|---|---|---|---|---|
| 创建 / 已在普通家庭 | `ErrAlreadyInFamily` | 409 | 4090 | `ALREADY_IN_FAMILY` |
| 加入：家庭不存在 | `ErrFamilyNotFound` | 404 | 4040 | `FAMILY_NOT_FOUND` |
| 加入：目标是个人家庭 | `ErrTargetIsPersonalFamily` | 403 | 4030 | `TARGET_IS_PERSONAL_FAMILY` |
| 加入：已在目标家庭 | `ErrAlreadyInTargetFamily` | 200 | 0000 | — （幂等成功） |
| 加入：家庭已满 | `ErrFamilyFull` | 409 | 4090 | `FAMILY_FULL` |
| 退出：非普通家庭 | `ErrNotInNormalFamily` | 400 | 4000 | — |
| 移除：非普通家庭 | `ErrNotInNormalFamily` | 403 | 4030 | — （与 `ErrNotOwner` 合并） |
| 退出：owner 不能退出 | `ErrOwnerCannotLeave` | 403 | 4030 | `OWNER_CANNOT_LEAVE_FAMILY` |
| 移除：不能移除自己 | `ErrCannotRemoveSelf` | 403 | 4030 | `CANNOT_REMOVE_SELF` |
| 移除：不能移除 owner | `ErrCannotRemoveOwner` | 403 | 4030 | `CANNOT_REMOVE_OWNER` |
| 移除 / 解散：非 owner | `ErrNotOwner` | 403 | 4030 | — |
| 移除：目标不在家庭 | `ErrTargetNotInFamily` | 404 | 4040 | — |
| 解散：不能解散个人家庭 | `ErrCannotDissolvePersonal` | 403 | 4030 | — |
| 任一并发锁竞争 | `ErrOperationInProgress` | 429 | 4290 | `OPERATION_IN_PROGRESS` |

> `code` / `biz_code` 词汇见 03-api §3（ADR-0008）；对应实现：`backend/pkg/errors/codes.go`、`backend/internal/family/errors.go`。

### 5.4 邀请错误码映射（`invite/handler.go`）

| 端点 / 场景 | HTTP | `code` | `biz_code` |
|---|---|---|---|
| `GET /invite/resolve` 码格式非法 | 400 | 4000 | — |
| `GET /invite/resolve` 码不存在 / 已过期 | 404 | 4040 | — |
| `GET /invite/resolve` DB 错误 | 500 | 5001 | — |
| `GET /invite/resolve` 限流（60/h/IP） | 429 | 4290 | `RATE_LIMITED` |
| `GET /invite/list` DB 错误 | 500 | 5001 | — |
| `POST /invite/qrcode` 生成失败 | 500 | 5001 | — |

## 6. 关键实现约束

### 6.1 锁

| 锁 | Key | 机制 | 说明 |
|---|---|---|---|
| 家庭级锁 | `lock:family:{familyID}` | PG advisory lock（无 TTL） | 加入 / 退出 / 移除 / 解散；`TryLocks` 内部按字典序获取（死锁防护） |
| 邀请二维码生成锁 | `invite:qrcode:gen:{userID}` | PG advisory lock（无 TTL） | 串行化同一用户的二维码生成 |
| 用户级注销锁 | `lock:delete_account:{userID}` | PG advisory lock（无 TTL） | 注销家庭关系处理（见 PP-02A） |

- 所有 `defer Unlock` 使用 `context.WithoutCancel`，确保请求上下文取消也能释放；连接断开锁自动释放（ADR-0005）。
- 未抢到锁不阻塞等待，直接 `ErrOperationInProgress`（429）。
- 加入流程在加锁前读取 user 快照，事务内复用该 user 快照（不锁后重读 user 行）；**成员数与成员清单在事务内以 tx 句柄重读**（`CountFamilyMembers`/`ListFamilyMembers`）；并发一致性由 `family_members.user_id` 唯一约束与 RepeatableRead 事务保证。

### 6.2 事务

- 家庭变更 / 加入 / 解散 / 链接奖励：`db.WithTxDeferrable`（RepeatableRead + Deferrable，40001 重试 3 次）。
- 二维码文件记录替换：`db.WithTx`。
- 封面迁移与成员迁移必须同事务；`DeleteFamilyDailyCovers` 必须在封面迁移之后。
- 批量操作使用 `unnest()` / `ANY($1)`，禁止逐成员循环（N+1）。

### 6.3 家庭成员上限

- 普通成员加入：目标家庭 `COUNT(*) >= 6` → 拒绝。
- 源 owner 加入：`目标数 + 源数 > 6` → 拒绝。
- 上限判定在事务内、锁保护下执行。

### 6.4 缓存失效

- 家庭变更（加入 / 退出 / 移除 / 解散 / 注销）后删除 `ai:family_summary:{familyID}`（源 + 目标 + 受影响成员当前/个人家庭）。
- 失效在**事务提交之后**执行；删除失败仅 WARN。

### 6.5 二维码与缓存

| Key | 值 | TTL |
|---|---|---|
| `invite:qrcode:{userID}` | 合成图 URL | 6 天 |
| `invite:qrcode:{userID}:raw` | 原始 PNG URL | 6 天 |

> 生成锁 `invite:qrcode:gen:{userID}` 为 PG advisory lock，非 Redis key（ADR-0005）。

- 旧物理文件删除在 DB 提交后执行，跳过与新文件同路径的文件（本地存储 relPath 为 `YYYY/MM/{shortCode}.{ext}`，OSS 对象 key 为 `uploads/YYYY/MM/{shortCode}.{ext}`）。

### 6.6 body 上限

| 端点 | maxBytes |
|---|---|
| `/family/invite-link/join`、`/invite/qrcode` | 65536（64KiB） |
| 家庭其它端点 | 无 body 解析 |

### 6.7 封面迁移规则

- 只迁移 `cover_type != 'default'` 的封面；目标已存在同日期非 default 封面时不覆盖（`ON CONFLICT ... WHERE family_daily_covers.cover_type = 'default'`）。
- 成员回个人家庭时只迁移该成员有日记记录的日期（`JOIN diaries`）。

## 7. 前端接入

源：`frontend/miniapp/miniprogram/`。

| 页面 / 模块 | 行为 |
|---|---|
| `pages/Family/Family.ts` | `onLoad` 有 `option.linkId` → `setPendingLinkId`；`onShow` 必要时登录后 `handlePendingInvite` → `POST /family/invite-link/join`；`fetch` → `GET /family`；`createInviteLink` → `POST /family/invite-link`；退出自己 `POST /family/leave`，移除他人 `DELETE /family/members/{userId}`；`onShareAppMessage` 路径 `/pages/Family/Family?linkId=...`；成员列表展示取 `familyList.slice(0, 50)` |
| `pages/Invite/Invite.ts` | `fetch` → `GET /invite/list`；`createInviteLink` → `POST /family/invite-link`；`ensureQRCode` → `POST /invite/qrcode`；保存分享图（`wx.downloadFile` + `wx.showShareImageMenu`）；分享路径 `/pages/index/index?inviter={userId}`；列表展示取 `(data.list || []).slice(0, 50)` |
| `pages/index/index.ts` | `option.inviter` → `setPendingInviter`；`option.scene` → 提取短码后 `GET /invite/resolve`；已有登录态时 `POST /auth/inviter` 补绑 |
| `utils/storage.ts` | `pendingLinkId`（7 天过期）、`pendingInviter` |
| `utils/auth.ts` | 登录成功后若存在 `pendingLinkId` 且当前页是 Family → 保持停留交由家庭页处理，否则 `redirectTo` 家庭页 |

## 8. 运维与任务

### 8.1 Redis key（本域）

| Key | 用途 | TTL |
|---|---|---|
| `ai:family_summary:{familyID}` | AI 家庭汇总缓存（家庭变更时失效） | 由 AI 模块管理 |
| `invite:qrcode:{userID}` / `:raw` | 邀请二维码 URL 缓存 | 6 天 |

> 锁（`lock:family:{familyID}`、`invite:qrcode:gen:{userID}`）为 PostgreSQL advisory lock，非 Redis key、无 TTL（ADR-0005）。

### 8.2 静态资源与依赖

- 邀请分享背景图固定路径：`/app/assets/invite-share-cover.png`（`invite/handler.go` `defaultInviteBgPath`）。文件缺失时合成失败 → 500。
- 依赖微信 `getwxacodeunlimit` 接口，使用 `WECHAT_APPID` / `WECHAT_SECRET` 换取的 access_token（缓存见 PP-02A §8.2）。
- 文件存储走本地 / OSS（`storage.SaveSystemWithName`），受 OSS 相关环境变量控制。

### 8.3 后台任务

- 本域无独立定时任务；邀请二维码旧物理文件在事务后同步删除，失败留孤儿文件，由通用孤儿文件清理任务（`jobs` 分册）兜底。
- 家庭锁、二维码生成锁均为 PostgreSQL advisory lock，无 TTL/续期任务（ADR-0005）。

### 8.4 迁移

- 相关表：`families`、`family_members`、`family_daily_covers`、`user_invite_codes`、`user_invites`；变更须配对 `.down.sql` 并同步 L4 文档。
