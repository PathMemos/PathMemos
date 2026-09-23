# 业务主流程

> 目的：以人类可读方式描述 papafeiji SaaS 的端到端业务主流程，与 L7 人工验收场景对应。
> 依据：当前代码实现整理（非新设计）。人工验收清单见 `docs/spec/07-acceptance-flows.md`。
> 状态：以当前代码为唯一事实源的基线。

## 0. 终端与总览

```
微信小程序 ──登录/记录/家庭/VIP/AI──▶ pro.papafeiji.cn (Go app+sse) ──▶ Postgres/Redis
微信公众号 ────────── AI 对话 ──────▶ /wx/callback
第三方 AI 工具 ──MCP──▶ mcp.pathmemos.com (Worker) ──▶ 源站 /internal/mcp/*
私有化/开源版 ──▶ api.pathmemos.com (Worker) ──▶ 自建后端
```

| 流程 | 名称 | 入口 |
|------|------|------|
| F1 | 首次进入与登录 | 打开小程序 / 授权 |
| F2 | 手动记录日记 | 首页 + 记录按钮 |
| F3 | 自动记录 | 首页轨迹采集 |
| F4 | 日记查看/编辑/分享 | 首页列表 → 详情 |
| F5 | 家庭与邀请 | 家庭页 / 邀请页 |
| F6 | VIP 购买与发货 | VIP 页 |
| F7 | AI 对话 | AI 抽屉 / 公众号 |
| F8 | MCP 开放接入 | MCP 页 |
| F9 | 账号注销 | 关于/设置页 |
| F10 | 推送与异常告警 | 后台任务 |
| F11 | 图片上传与配额 | 记录/头像 |
| F12 | 私有化后端接入 | 设置页（开源版） |

---

## F1 首次进入与登录

| 项 | 内容 |
|----|------|
| 触发 | 用户打开小程序 |
| 主流程 | ① `wx.login` 取 code → ② `POST /auth/login` → ③ 后端 `api.weixin.qq.com` 换 openid → ④ 建/更新用户 → ⑤ 返回 sessionId+userInfo，session 写 Redis → ⑥ 前端存 sessionId → ⑦ `GET /auto-record/config` 拉自动记录配置 |
| 涉及 | 表 `users`；前端 `utils/auth.ts`、`pages/index` |
| 期望 | 登录后进入首页；新用户标记 `needShowXPa`，引导页 `pages/Guide`；老用户直接首页 |
| 异常 | code 失效/微信不可达 → 登录失败提示；401 → 清 session 重新登录 |

## F2 手动记录日记

| 项 | 内容 |
|----|------|
| 触发 | 首页点「记录」打开 `NoteEdit` |
| 主流程 | ① 填文字/时间/位置 → ② 选图触发 `POST /file/upload?type=recordImg`（≤10MB/张，并发 3）→ ③ 提交 `POST /diary/details`（编辑走 `PUT /diary/details`）→ ④ 后端落条目 + 图片关联 + 同步评估封面 → ⑤ 前端轮询 `GET /diary/cover-url`（0.5s）等轨迹图就绪替换封面 |
| 涉及 | 表 `diaries`、`diary_entries`、`diary_entry_images`、`files`、`family_daily_covers`；配额 `users.image_storage_bytes` |
| 期望 | 保存成功即时可见；封面优先级 manual > image > trajectory > default |
| 异常 | 图片超限/配额不足 → 明确提示；部分上传失败 → 提示重试（仅补传失败项，已成功图不重传） |

## F3 自动记录

| 项 | 内容 |
|----|------|
| 触发 | 用户开启自动记录（`PUT /auto-record/config`）；前端按策略采集定位 |
| 主流程 | ① 本地累积驻留点（上限 50）→ ② 上报轨迹点 `POST /auto-record/trajectories` → ③ 后台任务聚类 + 内部逆地理（腾讯地图 client，非 `/location/reverse`）→ ④ 后台事务直写日记条目（不经 HTTP；`POST /diary/details/auto` 仅为「首次即时成文」入口）→ ⑤ 配置读写 `GET/PUT /auto-record/config` |
| 涉及 | 表 `auto_record_trajectories`、`diaries`、`diary_entries`；后台任务 auto_record/abnormal_alert/cleanup |
| 期望 | 驻留点自动成文，与手动记录不重复；地址做常用地址替换 |
| 异常 | 网络失败保留本地待下次；本地存储达上限时按失败处理 |

## F4 日记查看/编辑/分享

| 项 | 内容 |
|----|------|
| 触发 | 首页按日期/家庭筛选点开日记 |
| 主流程 | ① `GET /diary/info`（cursor 分页）→ ② 详情 `GET /diary/details`（offset 分页）→ ③ 统计 `GET /diary/stats`、日期 `POST /diary/info/dates` → ④ 编辑/删除 `PUT/DELETE /diary/details` 或 `DELETE /diary/info` → ⑤ 分享生成图片（含二维码） |
| 涉及 | 前端 `pages/index`、`pages/NoteDetail`、`components/NoteItem/RecordItem` |
| 期望 | 列表/详情一致，删除后计数与封面同步 |
| 异常 | 详情页 offset 分页在并发增删下可能重复/漏条 |

## F5 家庭与邀请

| 项 | 内容 |
|----|------|
| 触发 | 家庭页创建/加入家庭；邀请页分享 |
| 主流程 | ① `GET /family` 看配置 → ② `POST /family/invite-link` 生成链接/二维码 `POST /invite/qrcode` → ③ 被邀请人 `GET /invite/resolve` → `POST /family/invite-link/join` → ④ 成员管理 `DELETE /family/members/{userId}`、退出 `POST /family/leave` |
| 涉及 | 表 `families`、`family_members`、`user_invites`、`user_invite_codes` |
| 期望 | 家庭成员共享同一份日记视图；邀请关系可追溯 |
| 异常 | 已在家庭/链接无效 → 明确提示（邀请链接长期有效、无撤销，为已接受取舍，ADR-0010） |

## F6 VIP 购买与发货

| 项 | 内容 |
|----|------|
| 触发 | VIP 页点购买 |
| 主流程 | ① `GET /vip` / `GET /user/vip` 查权益 → ② `POST /payment/virtual/request`（env=0 现网）→ ③ `wx.requestVirtualPayment` → ④ 微信回调 `POST /api/prod/payment/virtualPayNotify`（安全模式验签+解密）→ ⑤ 幂等发货叠加 VIP → ⑥ 前端轮询 `GET /payment/virtual/status` |
| 涉及 | 表 `orders`、`vips`、`user_vips`、`user_vip_claims`；新用户试用 `POST /vip/new-user`、免费领取 `POST /vip/free/claim` |
| 期望 | 支付成功即到账；重复回调不重复发货；金额 > 0 校验（与标价不一致仅告警照发，ADR-0011） |
| 异常 | 沙箱 `env=1` 生产已禁用（403）；回调失败可按 AGENTS.md 重放 |

## F7 AI 对话

| 项 | 内容 |
|----|------|
| 触发 | 小程序 AI 抽屉输入；或公众号发消息 |
| 主流程 | ① 小程序 `POST /ai/chat`（SSE，`enableChunked`）→ ② 后端经 DeepSeek 流式返回 → ③ 事件 `data` 增量、`done` 结束；断线自动重连一次（复用 `request_id` 幂等）|
| 涉及 | 后端 `internal/ai` + Redis（配额/幂等/缓存）；前端 `components/AIDrawer`、`utils/eventSource.ts` |
| 期望 | 流式输出；配额按日限制；公众号走 `/wx/callback` |
| 异常 | 上游超时/配额超限以 SSE `error` 事件返回；HTTP 非 200 直接终止 |

## F8 MCP 开放接入

| 项 | 内容 |
|----|------|
| 触发 | 用户在 MCP 页生成 API Key，配置到 Cursor/Kimi 等 |
| 主流程 | ① `POST /mcp/key`（或 rotate）→ ② 展示 `mcpConfig`（Worker 地址）→ ③ 客户端请求 `mcp.pathmemos.com/mcp` → ④ Worker 透传源站 `/internal/mcp/*`（`X-Worker-Secret`）→ ⑤ 按 `key_hash` 校验读写日记/回忆 |
| 涉及 | 表 `api_keys`、`memories`；Worker `mcp-worker` |
| 期望 | 绑定页 `/auth` 生成 `?t=token`；SaaS 源站不暴露公开 `/mcp/*` |
| 异常 | saas 模式缺 `MCP_WORKER_SECRET` 启动即失败；源站保留空值 500 防御分支（启动校验通过后不可达）；静态方法鉴权现状见 02h |

## F9 账号注销

| 项 | 内容 |
|----|------|
| 触发 | 关于页输入昵称确认注销 |
| 主流程 | ① `DELETE /auth/account`（body `confirmName` 必须等于昵称）→ ② 删 session → ③ DB 事务删用户数据 → ④ 踢家庭成员 → ⑤ 异步清文件 → ⑥ 前端清本地并回首页 |
| 期望 | 注销后无法再用原账号数据；失败可重试 |
| 异常 | 昵称不匹配 400；DB 失败用户需重新登录重试 |

## F10 推送与异常告警

| 项 | 内容 |
|----|------|
| 触发 | 用户订阅后，后台任务按条件触发 |
| 主流程 | ① `POST /subscribe/record` 记录订阅 → ② 后台 abnormal_alert 任务扫描（8:00–22:00）→ ③ 模板消息/客服消息下发 |
| 期望 | 仅订阅用户收到；异常告警限频 |
| 异常 | 发送失败记录日志，下轮重试 |

## F11 图片上传与配额

| 项 | 内容 |
|----|------|
| 触发 | 写日记选图 / 更换头像 |
| 主流程 | ① 前端压缩（quality 按大小 50/65/80）→ ② `POST /file/upload?type=recordImg\|avatar`（并发 3，单文件 ≤10MB、总量 ≤50MB、≤9 张）→ ③ 配额原子扣减 `users.image_storage_bytes` → ④ 返回 `{fileId,url}` |
| 期望 | 白名单 jpg/jpeg/png/gif/webp；DB 记录与配额同事务；任一分片失败整请求回滚 |
| 异常 | 类型/大小超限 → 400（biz_code=INVALID_FILE_TYPE/FILE_SIZE_EXCEEDED）；配额不足 → USER_IMAGE_STORAGE_LIMIT_EXCEEDED 弹升级；超时 → 原始包 `{"code":"5001"}` |

## F12 私有化后端接入（开源版）

| 项 | 内容 |
|----|------|
| 触发 | 用户在设置页配置自部署后端（BackendConfig 页） |
| 主流程 | ① `POST api.pathmemos.com/worker/register {url, apiKey}` → ② Worker 握手探测（`/system/config` mode=open + `/vip` 非 401）→ ③ KV 写 `backend_api_key:<sha256>` → ④ 小程序切 `backend_mode=private`，请求带 `X-Private-Api-Key` 经 Worker 路由到自建后端 |
| 期望 | 注册成功后清本地强制重登；`/system/config` features 按目标后端返回（payment=false） |
| 异常 | Worker 错误码 4000/4001/4002/4003 → 对应文案；未注册 4031 不回退 SaaS |

---

## 覆盖登记（需求编号 → flow）

| 需求 | flow | 说明 |
|------|------|------|
| PP-01 O1 记录留存 | F2/F3 | 手动+自动记录 |
| PP-01 O2 商业变现 | F6 | 虚拟支付 |
| PP-01 O3 AI 使用率 | F7 | 小程序+公众号 |
| PP-01 O4 开放生态 | F8 | MCP/API Key |
| PP-02a 账号 | F1/F9 | 登录与注销 |
| PP-02d 家庭 | F5 | 家庭与邀请 |
| PP-02g 推送 | F10 | 模板/客服消息 |
| PP-02g 文件 | F11 | 上传/配额/头像 |
| PP-02h 私有化 | F12 | api-worker 路由 |

> 各 flow 对应 `docs/spec/07-acceptance-flows.md` 场景 F1~F12。
