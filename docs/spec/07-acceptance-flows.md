# PP-07 人工验收流程（L7）

> 层级：L7 人工验收｜版本：V2.1｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览、PP-02a~02h、PP-06 小程序页面与交互
> **验收策略**：不引入自动化 flow 执行器；端到端验收以**人工**按本清单执行，自动化门禁仅保留后端 Go 测试（`make test`）。场景编号 F1~F12 保持稳定，与 `docs/BUSINESS-FLOWS.md` 对齐。

## 1. 执行约定

| 项 | 约定 |
|----|------|
| 执行方式 | 人工按场景步骤操作小程序 / curl 调接口，逐条核对「期望」；结果记录通过/偏差。发现实现与 spec 不符 → 按 `spec-standards.md` §七 报告，不擅自裁决 |
| 接口级核对 | 错误分支可用 curl 带 `Authorization: Bearer <sessionId>` 直调核对；**无需任何打桩** |
| 外部依赖 | 微信登录/手机号/虚拟支付/公众号/模板消息/腾讯地图/DeepSeek 均按**真实服务**验证；公众号链路用测试公众号验证 |
| 沙箱支付 | `env=1` 仅在 `PAYMENT_ALLOW_SANDBOX=1` 的专用联调环境验证；生产与体验版必然 403（预期，见 02e VP-6-AC6） |
| 账号与数据 | 使用专用测试账号；每个场景可重复执行；破坏性场景（F9 注销、F5 解散）用一次性账号 |
| 环境 | SaaS：`DEPLOYMENT_MODE=saas` + 全套微信/支付/地图/AI 配置；F12 需一套 open 模式自建后端 |

## 2. 人工验收场景

### F1 首次进入与登录

| 项 | 内容 |
|----|------|
| 前置 | 一个从未登录过的微信账号；`WECHAT_APPID/WECHAT_SECRET` 已配置 |
| 涉及接口 | `POST /auth/login`、`GET /user/profile`、`PUT /user/lang`、`GET /auto-record/config`、`POST /vip/new-user`、`POST /auth/inviter`、`GET /invite/resolve` |
| 涉及表 | `users`、`families`、`family_members`、`user_vips`、`user_vip_claims`、`user_invite_codes`、`user_invites` |
| 涉及页面 | `pages/index/index`、`pages/Guide/Guide`、`pages/Family/Family` |

验收步骤：
1. 打开小程序 → 观察请求 `POST /auth/login` 返回 200，`sessionId/newUser=true/userInfo` 齐全；新用户落 Guide 页；小程序内可见 7 天试用 VIP（`GET /user/vip` 的 `isVip=true`）。
2. 杀进程重开 → `newUser=false`，直接进首页（不重复建档；`users` 无新行）。
3. 带邀请场景码（`?scene=<8位短码>` 或 `?inviter=<userId>`）冷启动 → 解析后注册，双方各得奖励（被邀 +3 天、邀请人 +7 天，当月上限 14 天）。
4. 已登录状态下再次进入 → 不重复发奖励（`user_invites`/`user_vip_claims` 无新行）。

异常核对：`code` 为空 → 400；邀请码非法（小写/长度不足）→ `GET /invite/resolve` 400；码不存在 → 404。

### F2 手动记录日记

| 项 | 内容 |
|----|------|
| 前置 | 已登录；已授权定位 |
| 涉及接口 | `GET /location/reverse`、`GET /user/common-addresses`、`POST /file/upload?type=recordImg`、`POST/PUT/DELETE /diary/details`、`GET /diary/info`、`GET /diary/stats` |
| 涉及表 | `diaries`、`diary_entries`、`diary_entry_images`、`files`、`users.image_storage_bytes`、`user_common_addresses` |
| 涉及页面 | `pages/index/index`、`components/NoteEdit/NoteEdit` |

验收步骤：
1. 首页点「记录」→ 填文字/时间/位置 → 保存 → 返回 `card`，首页出现该日期卡片，封面同步刷新（轨迹图异步就绪）。
2. 带 1~9 张图片保存 → 图片可见；`users.image_storage_bytes` 增加对应字节。
3. 编辑该条目（改文字/换图）→ 保存后详情与列表一致；被移除图片不再展示。
4. 删除条目 → 当日无其他条目且无记忆时，首页该日卡片消失。
5. `GET /diary/stats` 的 `totalEntries/recordDays/weeklyEntries` 与实际一致。

异常核对：正文 10001 字 → 400 + `biz_code=TEXT_TOO_LONG`；图片第 10 张 → 400；地址 >500 → 400；编辑跨天 recordTime → 400 `record time cannot cross day`；图片配额不足 → `USER_IMAGE_STORAGE_LIMIT_EXCEEDED` 弹升级；部分图片上传失败 → 仅补传失败项，已成功图不重传；删除含图条目/日记后，`users.image_storage_bytes` 同步回退（删空后可继续上传）。

### F3 自动记录

| 项 | 内容 |
|----|------|
| 前置 | VIP 有效（严格、无宽限期）；定位权限「始终允许」 |
| 涉及接口 | `PUT/GET /auto-record/config`、`POST /auto-record/trajectories`、`PUT /auto-record/active`、`POST /diary/details/auto`、`GET /diary/cover-url` |
| 涉及表 | `users`、`auto_record_trajectories`、`diaries`、`diary_entries`、`family_daily_covers`、`user_common_addresses` |
| 后台任务 | 自动记录（默认 5 分钟；锁 `lock:background:auto_record`）；用户级 `lock:auto_record:<userId>`（PG advisory lock，ADR-0005） |
| 涉及页面 | `pages/index/index`、`utils/autoRecord.ts` |

验收步骤：
1. 开启开关（VIP）→ `PUT /auto-record/config {enabled:true}` 成功，图标变亮；关闭 → 先上报积压驻留点再置 false。
2. 真机驻留 ≥10 分钟（300m 内）→ 观察本地驻留点上报；等待后台任务（≤5 分钟）→ 自动出现「（自动记录）」条目，地址为逆地理/常用地址结果。
3. 同一地点当日再次成文 → 不新建（与当日全部自动条目的地址并集集合判重，同 02c AR-7/D4）。
4. 首次开启立即成文：开启后 `POST /diary/details/auto` 返回 `id` 且当日出现条目。
5. 常用地址核对：次日 03:00 任务后 `GET /user/common-addresses` 出现高频地址；后续成文 300m 内命中常用地址名。
6. 前端采集阈值（前台/后台质心窗口、静止 300m、驻留 10 分钟、本地队列 50 上限、精度过滤 iOS 3000m/其它 500m）按 02c §7 人工核对真机行为。

异常核对：非 VIP 开启 → 403 `NOT_VIP`；单批 >50 点（curl）→ 400；坐标越界 → 400；逆地理日配额（200/天）耗尽 → 429 `RATE_LIMITED`；后台与前台并发抢用户锁 → `OPERATION_IN_PROGRESS`。

### F4 日记查看 / 编辑 / 分享

| 项 | 内容 |
|----|------|
| 前置 | 已有若干日记条目、图片与记忆 |
| 涉及接口 | `GET /diary/info`、`GET /diary/details`、`GET /diary/stats`、`POST /diary/info/dates`、`PUT/DELETE /diary/info`、`DELETE /diary/details`、`/diary/details/memory`(POST/PUT/DELETE)、`POST /invite/qrcode` |
| 涉及表 | `diaries`、`diary_entries`、`memories`、`family_daily_covers`、`diary_entry_images`、`files` |
| 涉及页面 | `pages/index/index`、`pages/NoteDetail/NoteDetail`、`components/CoverEdit`、`NoteDetail/shareCanvas.ts` |

验收步骤：
1. 首页列表（游标分页，单页 15、累计上限 200）与详情（offset 分页，size 20）数据一致；下拉刷新/上拉加载正常。
2. 详情页家庭成员筛选（>1 人时出现 Tab）：切换成员仅显示该成员条目；本人可编辑，他人条目不可编辑（toast）。
3. 编辑条目（`PUT /diary/details`）→ 列表与详情同步；删除条目 → 计数与封面联动。
4. 新建/编辑/删除记忆（长按记录入口；`/diary/details/memory`）→ 标题 1~50、内容 ≤10000 约束生效；改期产生新日期卡片并清理旧空卡片。
5. 设置手动封面（CoverEdit → `PUT /diary/info {id, coverImage}`）→ 封面立即变为所选图；优先级 manual > image > trajectory > default（删掉被引用图后降级）。未选图直接关闭抽屉不发请求（「清空封面」仅为后端 API 语义，前端无入口）。
6. 删除整日（首页卡片左滑 → `DELETE /diary/info`）→ 仅删除本人当日数据，其他成员同日数据不受影响。
7. 分享：详情页分享生成含二维码的图片并可保存。
8. `POST /diary/info/dates`（AI 卡片回填）→ 按日期返回卡片。

异常核对：diaryId 非法 → 400；非本家庭成员访问 → 403；编辑/删除他人条目 → 403。

### F5 家庭与邀请

| 项 | 内容 |
|----|------|
| 前置 | 两个测试账号 A（owner）/ B（新用户）；A 已创建共享家庭 |
| 涉及接口 | `GET /family`、`POST /family`、`POST /family/invite-link`、`POST /family/invite-link/join`、`POST /family/leave`、`DELETE /family/members/{userId}`、`DELETE /family`、`GET /invite/resolve`、`GET /invite/list`、`POST /invite/qrcode` |
| 涉及表 | `families`、`family_members`、`family_daily_covers`、`user_invites`、`user_invite_codes` |
| 涉及页面 | `pages/Family/Family`、`pages/Invite/Invite`、`pages/index/index` |

验收步骤：
1. A 生成邀请链接（`linkId == familyId`）→ B 打开分享卡片自动加入 → B `current_family_id` 指向 A 的家庭；双方看到同一份日记视图。
2. B 重复加入同一家庭 → 200 幂等。
3. B 退出（`POST /family/leave`）→ 回到个人家庭；A（owner）退出 → 403 `OWNER_CANNOT_LEAVE_FAMILY`。
4. A 移除 B（`DELETE /family/members/{userId}`，需二次确认）→ B 回个人家庭；移除自己/owner → 403。
5. A 解散家庭（`DELETE /family`，一次性账号执行）→ 全员回各自个人家庭，家庭删除，封面迁移正确。
6. 个人邀请：B 的邀请页生成小程序码/分享图 → 新用户 C 扫码注册 → `GET /invite/list` 出现 C；C 与 B 各得奖励（B 为邀请人时受月度 14 天上限约束；注册 7 天内加入/补绑均有效（两入口统一窗口）；同一微信注销重注册后被邀请奖励不再发放（openid 墓碑））。
7. `POST /invite/qrcode`（raw 与合成图）→ 各自缓存 6 天内复用同一 URL。
8. 已知风险（已接受，ADR-0012）：**owner 打开他人邀请链接会触发整家合并且不可逆**——人工验收时用一次性账号验证该行为符合预期即可，不做防护断言。

异常核对：家庭满（第 7 人）→ 409 `FAMILY_FULL`；目标为个人家庭 → 403 `TARGET_IS_PERSONAL_FAMILY`；已在目标家庭 → 200 幂等；并发家庭操作 → 429 `OPERATION_IN_PROGRESS`；短码非法/不存在 → 400/404。

### F6 VIP 购买与发货

| 项 | 内容 |
|----|------|
| 前置 | SaaS 模式；虚拟支付五项密钥已配置（`WECHAT_VIRTUAL_OFFER_ID`/`APP_KEY_PRODUCTION`/`APP_KEY_SANDBOX`/`CALLBACK_TOKEN`/`CALLBACK_AES_KEY`）；有付费商品（`vip-month-0001`/`vip-year-0001`） |
| 涉及接口 | `GET /user/vip`、`GET /vip`、`POST /payment/virtual/request`、`GET /payment/virtual/status`、`POST/GET /api/prod/payment/virtualPayNotify`、`POST /vip/new-user`、`GET /vip/free`、`POST /vip/free/claim`、`GET /vip/free/check`、`POST /payment/virtual/cancel` |
| 涉及表 | `vips`、`user_vips`、`user_vip_claims`、`orders`、`users` |
| 涉及页面 | `pages/sub/Vip/Vip`、`utils/pay.ts`、`utils/vip.ts` |

验收步骤：
1. VIP 页展示月/年档位与价格 → 选择支付 → 真实支付 1 单 → 轮询（3s×10）→ 成功弹窗，`GET /user/vip` 到期时间 = `GREATEST(now, 原到期)+时长`。
2. 已是 VIP 再购 → 叠加不缩短。
3. 支付取消/失败 → best-effort 关单；`GET /payment/virtual/status` 返回 `closed`。
4. 幂等：用 AGENTS.md 的回调重放命令重放已支付订单的回调 → 不重复发货（`user_vips` 不变）。
5. 新用户试用：新账号注册即得 trial（无需购买）；重复调 `POST /vip/new-user` → 409 `TRIAL_VIP_ALREADY_CLAIMED`。
6. 免费 VIP：`GET /vip/free` → `POST /vip/free/claim` → 成功一次，再领 → 409 `FREE_VIP_ALREADY_CLAIMED`；`GET /vip/free/check` 返回 `claimed=true`。
7. 体验版/开发版预期：`env=1` 在未开启 `PAYMENT_ALLOW_SANDBOX` 的环境必然 403 `sandbox payment is disabled`——**预期行为**（trial 由注册自动发放、不可购买；沙箱联调走专用环境）。

异常核对：`env∉{0,1}`（curl）→ 400；他人订单 status/cancel → 404 `ORDER_NOT_FOUND`；明文回调（无 encrypt）→ 固定成功 JSON 且不发货；金额非正数 → 业务拒绝；**回调事务中途失败（如人工注入 DB 抖动）→ 微信重试后 VIP 最终到账且不重复叠加**（paid 严格等价已交付，见 02e §6 事务原子性）。

### F7 AI 对话（小程序 + 公众号）

| 项 | 内容 |
|----|------|
| 前置 | 已登录；`AI_API_KEY/AI_BASE_URL/AI_MODEL` 就绪；公众号测试号可用 |
| 涉及接口 | `POST /ai/chat`（SSE，:8081）、`GET/POST /wx/callback`、`GET /system/config` |
| 涉及表 | `ai_dialog_logs`、`ai_daily_quota_usage`、`wx_mp_accounts` |
| 涉及页面 | `components/AIDrawer/AIDrawer`、`utils/eventSource.ts` |

验收步骤（小程序）：
1. AI 抽屉发送「你好」→ 流式增量输出、`done` 结束；回答内容与日记上下文相关（如提问「我上周去了哪」能引用日记）。
2. 断网重连（或快速重发同一条）→ 同 `request_id` 命中回放，不重复扣配额（`ai_daily_quota_usage.used` 不变）。
3. 配额：非 VIP 一天第 11 次 → 配额弹窗引导 VIP 页；VIP 第 101 次 → 同样拦截。
4. 输入 2501 字 → 400；`request_id` 非法 → 400。
5. 并发与在途：同用户开第 3 个 AI 连接（第三会话/设备）→ 429 envelope（`code=4290`、`biz_code=RATE_LIMITED`）；同 `request_id` 在途期间并发第二次请求 → `OPERATION_IN_PROGRESS`（在途不回落生成）；退款注入失败 4 次 → 日志出现 `alert:ai_quota_refund_failed`。

验收步骤（公众号，人工覆盖 02f AI-3 / 02g P-1~P-3）：
6. 关注公众号 → 收到欢迎语 + 小程序卡片；`wx_mp_accounts.subscribed=true`。
7. 发送文本 → 先回「正在思考」，随后收到分段客服消息（≤2000 字节/段）；同一秒重复发送同一句 → 60s 内去重不重复触发 AI。
8. 发送语音（可识别）→ 按文本处理；无法识别 → 语音失败提示。
9. 未绑定用户（未登录过小程序的微信号）发消息 → 回引导文案 + 小程序卡片。
10. 取关再关注 → 订阅状态联动；配额用尽 → 「当天额度已用完」文案。

### F8 MCP 开放接入

| 项 | 内容 |
|----|------|
| 前置 | SaaS 模式；`MCP_WORKER_SECRET/MCP_PUBLIC_URL` 已配置；Worker 可用；有 MCP 客户端（Cursor/Kimi 等） |
| 涉及接口 | `POST/GET/DELETE /mcp/key`、`POST /mcp/key/rotate`；Worker `https://mcp.pathmemos.com/mcp`、`/mcp/diary`、`/mcp/memories`、`/auth`、`/auth/bind`；源站 `/internal/mcp/*` |
| 涉及表 | `api_keys`、`memories`、`diaries` |
| 涉及页面 | `pages/sub/Mcp/Mcp` |

验收步骤：
1. MCP 页生成 Key（32 位、永不过期）→ 复制 mcpConfig 配入 Cursor → 工具调用 `query_memories` 能读日记/回忆；`store_memory` 写入后小程序内可见。
2. `POST /mcp/key/rotate` → 旧 Key 回源鉴权立即失效（客户端 401），新 Key 可用；边缘缓存最长 5 分钟自然收敛（keycheck 正缓存 60s、GET 缓存 1~5min）。
3. 绑定页：浏览器打开 `https://mcp.pathmemos.com/auth` → 粘贴 Key → 得到 `?t=<token>` URL → MCP 客户端配置该 URL 可用（人工覆盖 02h MCP-3）。
4. `GET /mcp/diary?limit=0`（curl）→ 200 空数组；跨度 >180 天 → 400；无效 Key → 401；Key 30/min 限流 → 429 `RATE_LIMITED`。
5. 直连源站 `/internal/mcp/diary`（无 `X-Worker-Secret`）→ 403；`MCP_WORKER_SECRET` 未配置 → saas 启动失败（健康门禁不通过），配置后恢复。

### F9 账号注销

| 项 | 内容 |
|----|------|
| 前置 | 一次性测试账号，写入 1 条日记、1 张图片；`confirmName` 等于当前昵称 |
| 涉及接口 | `DELETE /auth/account`（IP 限流 5 次/小时） |
| 涉及表 | `users` 及全部级联表；`orders.user_id` 置空；`user_invites`/`user_vip_claims` 置 NULL 保留（领取/邀请墓碑）；`api_keys`/`user_invite_codes`/`user_common_addresses`/`ai_daily_quota_usage` 应用层清理 |
| 涉及页面 | `pages/sub/About/About` |

验收步骤：
1. About 页输入错误昵称 → 400 不注销。
2. 输入正确昵称 → 200；本地清空回首页；旧 session 调接口 → 401；数据库 `users` 无该行；文件进入孤儿清理窗口。
3. 并发注销（双端同时确认）→ 一端 429 `OPERATION_IN_PROGRESS`。
4. 注销后同一微信重新登录 → 新用户（新 id），无旧数据；重注册成功且 trial 不再发放（openid 墓碑跳过，登录不被 500 阻断）。

### F10 推送与异常告警

| 项 | 内容 |
|----|------|
| 前置 | 测试账号开启自动记录且 VIP 有效；已订阅异常提醒（`POST /subscribe/record`）或已关注服务号 |
| 涉及接口 | `POST /subscribe/record` |
| 涉及表 | `users.abnormal_subscribe_accepted/abnormal_alert_sent_at/last_active_at`、`wx_mp_accounts`、`auto_record_trajectories` |
| 后台任务 | 异常告警（默认 5 分钟；仅 8:00–22:00；`last_active_at/最后轨迹 < now-1h`；每日至多一次） |
| 涉及页面 | `components/SubscribePrompt`、`pages/index/index` |

验收步骤：
1. 首页开启自动记录 → 弹订阅提示 → 同意订阅 → `users.abnormal_subscribe_accepted=true`。
2. 停用 1 小时以上（不再上报）→ 收到小程序订阅消息/服务号告警「后台定位已被系统暂停」；当天不重复推送。
3. 次日重复失联 → 再次告警。
4. 新地点提醒：新增一个与历史不同的地点自动成文 → 服务号收到新地点模板消息（6:00–23:00 内）。
5. 微信错误码联动：在小程序订阅管理中拒收后 → 下轮自动置 `abnormal_subscribe_accepted=false`，不再打扰。

### F11 图片上传

| 项 | 内容 |
|----|------|
| 前置 | 已登录；准备 <1MB png、>10MB png、非图片文件（如 .exe） |
| 涉及接口 | `POST /file/upload?type=recordImg|avatar`、`PUT /user/avatar`、`GET /file/download/{fileId}`、`DELETE /file/{fileId}` |
| 涉及表 | `files`、`users.image_storage_bytes`、`users.avatar_file_id`、`user_avatar_markers` |
| 涉及页面 | `components/NoteEdit/NoteEdit`、`pages/Set/Set` |

验收步骤：
1. 正常上传 1 张 png → 返回 `fileId/url`，URL 可在浏览器打开；配额字节增加。
2. 更换头像（`type=avatar` → `PUT /user/avatar`）→ 头像更新，地图 marker 生成（75px 圆形白边）。
3. 下载 URL（`GET /file/download/{fileId}`）→ 本人/同家庭成员 200；他人 → 403。
4. 删除未被引用的文件（`DELETE /file/{fileId}`）→ 配额回退、URL 失效；已被条目引用的文件 → 400 `file is still in use`。

异常核对：.exe → 400 `INVALID_FILE_TYPE`；>10MB 单文件 / >50MB 总量 → 400 `FILE_SIZE_EXCEEDED`；第 10 个文件 → 400；配额超限 → `USER_IMAGE_STORAGE_LIMIT_EXCEEDED`；前端压缩（quality 50/65/80，>10MB 拒绝）与并发 3 按真机核对。

### F12 私有化后端接入

| 项 | 内容 |
|----|------|
| 前置 | 一套 open 模式自建后端（`mode=open`、`OPEN_API_KEY` 已配）；Worker `api.pathmemos.com` 可用 |
| 涉及接口 | `POST/DELETE {WORKER_BASE_URL}/worker/register`、`GET {WORKER_BASE_URL}/system/config` |
| 涉及存储 | 小程序 `backend_mode/private_backend_url/private_backend_api_key`；Worker KV 路由 |
| 涉及页面 | `pages/sub/BackendConfig/BackendConfig` |

验收步骤：
1. BackendConfig 填入自建后端 URL + API Key → 测试连接通过（握手探测 `/system/config` mode=open + `/vip` 非 401）→ 保存后清本地并强制重登。
2. 切换后首页/详情/AI 均走 `api.pathmemos.com` 且数据来自自建后端；`GET /system/config` 返回 `mode=open`、`features.payment=false`（VIP 页隐藏支付入口）。
3. 故意填错误 URL/非 open 后端/被拒 Key → 分别得到 4002/4001/4003 对应文案。
4. 关闭私有化（注销映射）→ 回 SaaS；自建后端关机后请求 → 「私有后端不可达」文案（Worker 5020/5030）。
5. 自建后端直连核对：对直连请求伪造 `X-Worker-Secret`/`X-Forwarded-Host` 不生效（open 后端 `WorkerAuth` 短路，见 02h §6.2）。

## 3. 覆盖登记表（需求编号 → 场景 / 不需场景的理由）

> 编号来自各 L2 分册；02d 与 02g 都存在 `F-n`，引用时带分册前缀消歧。

| 需求编号 | 需求来源 | 覆盖场景 | 说明 / 不需场景的理由 |
|----------|----------|-----------|--------------------------|
| PP-01 O1 记录留存 | 01 | F2、F3 | 手动+自动记录 |
| PP-01 O2 商业变现 | 01 | F6 | 虚拟支付与 VIP |
| PP-01 O3 AI 使用率 | 01 | F7 | 小程序+公众号 AI 对话 |
| PP-01 O4 开放生态 | 01 | F8 | MCP/API Key |
| 02a A-1 微信登录 | 02a | F1 | — |
| 02a A-2 登出 | 02a | 不需场景 | 小程序无登出 UI 调用点；后端有 Go 单测 |
| 02a A-3 手机号绑定 | 02a | 不需场景（接口级） | 非主链路；后端有 Go 单测，可 curl 抽查日限 |
| 02a A-4 手机号解绑 | 02a | 不需场景（接口级） | 同上 |
| 02a A-5 补绑邀请人 | 02a | F1 | scene/inviter 解析 + `POST /auth/inviter` |
| 02a A-6 邀请奖励规则 | 02a | F1、F5 | 3/7 天与月封顶 14 天 |
| 02a A-7 用户资料/语言 | 02a | 不需场景 | 昵称/语言为普通 CRUD；Set 页人工顺带核对 |
| 02a A-8 账号注销 | 02a | F9 | — |
| 02b D-1 首页时间线 | 02b | F4 | — |
| 02b D-2 按日期取卡片 | 02b | F4 | — |
| 02b D-3 日记详情 | 02b | F4 | — |
| 02b D-4 手动新增条目 | 02b | F2 | — |
| 02b D-5 编辑条目 | 02b | F2、F4 | 含跨天拒绝 |
| 02b D-6 删除条目 | 02b | F4 | — |
| 02b D-7 自动记录成文 | 02b | F3 | — |
| 02b D-8 记忆 CRUD | 02b | F4 | — |
| 02b D-9 设置/清除手动封面 | 02b | F4 | — |
| 02b D-10 删除整日 | 02b | F4 | — |
| 02b D-11 封面降级 | 02b | F4 | manual>image>trajectory>default |
| 02b D-12 封面异步+轮询 | 02b | F2、F3、F4 | 前端 0.5s×12 次 |
| 02b D-13 日记统计 | 02b | F4 | — |
| 02c AR-1 开关 | 02c | F3 | VIP 校验 |
| 02c AR-2 前端采集 | 02c | F3 | 真机人工核对 |
| 02c AR-3 轨迹上报入库 | 02c | F3 | ≤50 点/批 |
| 02c AR-4 驻留点聚类 | 02c | F3 | 300m/30 分钟 |
| 02c AR-5 逆地理编码 | 02c | F3 | 日配额 |
| 02c AR-6 常用地址替换 | 02c | F3 | 300m 命中 |
| 02c AR-7 同点去重 | 02c | F3 | 与当日全部自动条目的地址并集集合判重（同 02c AR-7/D4） |
| 02c AR-8 后台成文 | 02c | F3 | 5 分钟任务 |
| 02c AR-9 首次即时成文 | 02c | F3 | `POST /diary/details/auto` |
| 02c AR-10 新地点提醒 | 02c | F10 | — |
| 02c AR-11 异常告警 | 02c | F10 | 8–22 点 + 1h 阈值 |
| 02c AR-12 常用地址摘要 | 02c | F3（步骤 5） | 次日 03:00 任务后人工核对 |
| 02c AR-13 轨迹清理 | 02c | 不需场景 | 后台 7 天窗口，DB 抽查即可 |
| 02d F-1 查看家庭 | 02d | F5 | — |
| 02d F-2 创建家庭 | 02d | F5 | — |
| 02d F-3 邀请链接 | 02d | F5 | linkId=familyId |
| 02d F-4 链接加入 | 02d | F5 | — |
| 02d F-5 链接加入奖励 | 02d | F5 | 注册 7 天内（统一窗口） |
| 02d F-6 退出家庭 | 02d | F5 | owner 禁止 |
| 02d F-7 移除成员 | 02d | F5 | 步骤 4 |
| 02d F-8 解散家庭 | 02d | F5（步骤 5） | 破坏性；一次性账号执行 |
| 02d F-9 个人邀请短码 | 02d | F1、F5 | `/invite/resolve` |
| 02d F-10 邀请列表 | 02d | F5 | — |
| 02d F-11 个人邀请二维码 | 02d | F5、F4 | `/invite/qrcode` |
| 02d F-12 家庭配置 | 02d | F1 | `GET /auto-record/config`（用户级开关） |
| 02e VP-1 VIP 状态查询 | 02e | F6 | — |
| 02e VP-2 付费商品查询 | 02e | F6 | — |
| 02e VP-3 新用户试用 | 02e | F1、F6 | 注册自动发放；不可购买 |
| 02e VP-4 免费 VIP 领取/查重 | 02e | F6 | — |
| 02e VP-5 种子商品 | 02e | 不需场景 | migration 数据基线，由 `spec-check` 校验 |
| 02e VP-6 下单 | 02e | F6 | — |
| 02e VP-7 轮询 | 02e | F6 | — |
| 02e VP-8 取消订单 | 02e | F6 | 前端 `utils/pay.ts` 已接入 |
| 02e VP-9 回调验签解密 | 02e | F6 | 重放核对幂等 |
| 02e VP-10 幂等发货 | 02e | F6 | — |
| 02e VP-11 订单关闭 | 02e | F6 | 后台 24h |
| 02e VP-12 补发 | 02e | 不需场景 | 运维脚本 `grant_vip_by_phone.sh`，人工执行 |
| 02e VP-13 前端支付交互 | 02e | F6 | — |
| 02f AI-1 小程序 SSE | 02f | F7 | — |
| 02f AI-2 重连幂等 | 02f | F7 | request_id + reply 缓存 |
| 02f AI-3 公众号对话 | 02f | F7（步骤 6~10） | 公众号链路人工覆盖 |
| 02f AI-4 配额 | 02f | F7 | 10/100 每日 |
| 02f AI-5 上下文落库 | 02f | F7 | — |
| 02f AI-6 上游异常 | 02f | F7 | 断上游人工演练 |
| 02g F-1 图片上传 | 02g | F11 | — |
| 02g F-2 存储配额 | 02g | F11 | 1GiB/5GiB |
| 02g F-3 文件下载 | 02g | F11 | — |
| 02g F-4 文件删除引用计数 | 02g | F11（步骤 4） | — |
| 02g F-5 头像更新 | 02g | F11 | — |
| 02g F-6 存储与 URL | 02g | F11 | 本地/OSS |
| 02g F-7 孤儿文件清理 | 02g | 不需场景 | 后台 7 天任务，DB 抽查即可 |
| 02g P-1 公众号验证 | 02g | F7（步骤 6~10） | 公众号链路人工覆盖 |
| 02g P-2 消息去重 | 02g | F7（步骤 6） | Redis 60s |
| 02g P-3 客服消息 | 02g | F7（步骤 6） | — |
| 02g P-4 异常告警推送 | 02g | F10 | — |
| 02g P-5 新地点提醒 | 02g | F10 | — |
| 02g P-6 订阅记录 | 02g | F10 | — |
| 02g P-7 access_token | 02g | 不需场景 | 平台凭据缓存；后端有 Go 单测 |
| 02h MCP-1 管理 API Key | 02h | F8 | — |
| 02h MCP-2 SaaS Worker 接入 | 02h | F8 | — |
| 02h MCP-3 绑定页 /auth | 02h | F8（步骤 3） | 浏览器人工验证 |
| 02h MCP-4 工具调用 | 02h | F8 | store_memory/query_memories |
| 02h MCP-5 REST 三件套 | 02h | F8 | curl 核对 |
| 02h MCP-6 开源直连 /mcp/* | 02h | F12（open 环境） | DEPLOYMENT_MODE=open |
| 02h MCP-7 api-worker 私有化路由 | 02h | F12 | — |
| 02h MCP-8 Worker 缓存 | 02h | F8（可选） | 重复查询观察 X-Worker-Cache 头 |

## 4. 残留说明

- 本文件为**人工验收契约**：无执行器、无自动断言；「异常核对」条目可用 curl 抽查。
- 后台任务类行为（关单、清理、摘要）不单独设场景，按窗口到期后 DB/日志抽查。
- Redis 故障注入、Worker KV 故障等破坏性演练不在常规清单内，需要时临时执行（不变量见 `ARCHITECTURE-INVARIANTS.md` §6）。
