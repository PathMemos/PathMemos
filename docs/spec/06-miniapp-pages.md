# PP-06 小程序页面与交互（L6）

> 层级：L6 前端/交互｜版本：V2.0｜状态：定稿（以当前代码为唯一事实源）
> 上游：PP-01 产品总览｜关联分册：PP-02a~02h
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。
> 关键实现位置：`frontend/miniapp/miniprogram/`（`app.json`、`app.ts`、`config/index.ts`、`pages/`、`components/`、`utils/`、`behaviors/`）；关键处内联源文件路径。
> 其他实现文件：`polyfills/textDecoder.ts`（SSE UTF-8 流解码，位于 miniprogram 根目录）、`utils/markdown.ts`（渲染）、`utils/concurrency.ts`（并发 3）、`pages/NoteDetail/shareCanvas.ts`（离屏分享图）、`lib/dayjs`（时间库）；`app.json` 的 `sitemapLocation` 指向 `sitemap.json`。

## 1. 页面清单（路由 / 用途 / 入口 / 优先级）

页面与分包声明见 `app.json`；页面为自定义导航（`"navigationStyle": "custom"`），无 tabBar。

| 路由 | 分包 | 用途 | 入口 | 优先级 |
|------|------|------|------|--------|
| `pages/index/index` | 主包 | 首页：日记卡片时间轴、统计面板、自动记录开关、编辑/AI/记忆抽屉 | 小程序启动页（`app.json` pages[0]） | P0 |
| `pages/NoteDetail/NoteDetail` | 主包 | 当日日记详情：家庭成员筛选、条目/记忆时间线、封面、分享图 | `index` 的 NoteItem、AIDrawer 卡片（`gotoDetail`） | P0 |
| `pages/Family/Family` | 主包 | 家庭：成员列表、邀请链接分享、退出/移除成员 | `pages/User` → `toFamily`；分享卡片 `?linkId=` | P0 |
| `pages/sub/Vip/Vip` | 分包 `pages/sub` | VIP 中心：商品选择、虚拟支付、免费 VIP 领取 | `User`/`index`(VIP 提示)/`Set`/`AIDrawer`(配额)/`NoteEdit`/`AutoBtn` 等 `navigateTo` | P0 |
| `pages/User/User` | 主包 | 个人中心：VIP 状态、各功能入口 | `index` 导航栏 home 按钮 `goToUser` | P1 |
| `pages/Set/Set` | 主包 | 设置：昵称、头像、主题、语言、手机号、常用地址、服务号、桌面快捷 | `User` → `toSet` | P1 |
| `pages/Guide/Guide` | 主包 | 新用户 4 步引导图 | 新用户登录后 `wx.redirectTo`（`utils/auth.ts`）；Family 页 `toIndex` 时 `needShowXPa` | P1 |
| `pages/Invite/Invite` | 主包 | 个人邀请：邀请小程序码/分享图、受邀列表 | `User` → `toInvite`（仅非私有模式显示） | P1 |
| `pages/sub/Mcp/Mcp` | 分包 | MCP：API Key 生成/换发、配置复制、连接器/HTTP Tab | `User` → `toMcp`；`MemoryEdit` → `goMemoryConfig` | P1 |
| `pages/CommonAddresses/CommonAddresses` | 主包 | 常用地址列表与重命名 | `Set` → `goCommonAddresses`；`NoteEdit` POI 区 → `goCommonAddresses` | P2 |
| `pages/sub/About/About` | 分包 | 关于、协议入口、账号注销 | `User` → `toAbout` | P2 |
| `pages/sub/BackendConfig/BackendConfig` | 分包 | 私有化后端接入（URL + API Key 注册） | `User` → `goBackendConfig`（SaaS 模式也显示） | P2 |
| `pages/sub/WebPage/WebPage` | 分包 | WebView 容器（域名白名单） | `utils/util.ts openUrl`（教程/协议/小红书等） | P2 |
| `pages/sub/UsageGuide/UsageGuide` | 分包 | 使用指南（外链教程 0~7） | `About` → `toGuide` | P3 |

`app.json` 其他全局声明：

| 配置 | 值 |
|------|----|
| `window` | `navigationStyle: custom`；导航/页面背景色走主题 token（`@navBgColor` 等）；`initialRenderingCache: dynamic` |
| `darkmode` / `themeLocation` | `true` / `theme.json` |
| `style` / `componentFramework` | `v2` / `exparser` |
| `lazyCodeLoading` | `requiredComponents` |
| `requiredPrivateInfos` | `getLocation`、`onLocationChange`、`chooseLocation`、`startLocationUpdate`、`startLocationUpdateBackground` |
| `requiredBackgroundModes` | `location` |
| `permission.scope.userLocation.desc` | 「用于记录日记位置」 |

前端测试位于 `frontend/miniapp/test/*.test.ts`（以目录实际为准，当前 9 套件 / 49 用例），本地用 `make test-frontend` 执行；组件级测试由 `test/jest.setup.ts` 注入 `Component/Page/Behavior` 捕获实现。按 spec-standards §五 验收策略，前端测试为**存量资产、非验收门禁**：不要求随新功能新增用例，前端行为以人工验收（L7）为准；CI（`.github/workflows/ci.yml`）跑三个 job：backend（lint + go test + 各项同步校验）、frontend（`npm run lint`，不含 Jest）、worker（typecheck）——前端/Worker 测试不在 CI。

- `utils/logger.ts`：分级日志接口（log/info/warn/error），**所有级别一律写入内存缓冲**（上限 100 条，超出裁掉最旧）；release 环境差异仅是丢弃 `detail` 字段（logger 本身不向 console 输出，无级别过滤）；所有字符串 message 先经 `sanitizeUrlForLog` 剥离 query/hash 再入缓冲，防 URL 参数落日志。
- `utils/opslog.ts`：客户端操作日志本地持久化（key `ops_log_queue`，上限 200、单批 50），打开小程序时批量上报 `POST /ops/client-log`；detail 仅保留计数/标识/错误码等非敏感摘要。

组件（16 个）与 behaviors（5 个）清单：`AIDrawer`（AI 对话抽屉）、`NoteEdit`/`MemoryEdit`/`CoverEdit`（编辑抽屉）、`RecordItem`/`MemoryItem`/`NoteItem`（左滑删除列表项）、`AutoBtn`（日记详情/编辑内的自动记录开关，内含订阅提示联动；首页开关是 `pages/index/index.ts` 的 `toggleAutoRecord`）、`CalendarPicker`（日历选日期，非法输入回退今天）、`TimePicker`（时间选择）、`Cell`（通用设置行）、`PTextarea`（受控多行输入）、`Drawer`/`ConfirmDialog`/`SubscribePrompt`（通用抽屉/确认弹窗/订阅提示）、`navigation-bar`（自定义导航栏）；behaviors：`safeSetData`（三标志安全写入）、`theme`/`i18n`（主题/文案订阅）、`touchSwipe`（左滑手势）、`transition`（动效）。

## 2. base URL 模式与运行时配置

### 2.1 三种后端地址（`config/index.ts`）

| 常量 | 值 | 用途 |
|------|----|------|
| `SAAS_BASE_URL` | `https://pro.papafeiji.cn` | SaaS 默认直连源站 |
| `WORKER_BASE_URL` | `https://api.pathmemos.com` | 私有化/开源版经 Cloudflare Worker 路由（`backend_mode === 'private'`） |
| `DEV_BASE_URL` | `http://localhost:8080` | 开发版（`envVersion === 'develop'`） |
| `OSS_PUBLIC_URL` | `https://ppfj-images.oss-cn-hangzhou.aliyuncs.com` | 教程/引导等公共静态图 |
| `getHelpBaseURL()` | `https://papafeiji.cn` | 帮助/协议/教程站点（私有模式同样指向 SaaS） |
| `EXPORT_GUIDE_URL` | 小红书链接 | 分享导出引导 |
| `NEW_USER_FREE_VIP_ID` | `vip-free-0001` | 免费 VIP 查重 id |
| `ABNORMAL_TEMPLATE_ID` | `i7mcEEMDbhYU1oAC1-E0G0xvIBCmTl6f9c3jOq11m3g` | 异常提醒订阅模板 |

### 2.2 地址决策顺序（`getBaseURL()` / `getSSEBaseURL()`，`config/index.ts`）

1. `envVersion === 'develop'` → `DEV_BASE_URL`；
2. 否则读 `wx.getStorageSync('backend_mode')`（`STORAGE_KEY_MODE`）：`'private'` → `WORKER_BASE_URL`；
3. 其余（含读取失败）→ `SAAS_BASE_URL`。

`getSSEBaseURL()` 恒等于 `getBaseURL()`。导出常量 `BASE_URL` / `SSE_BASE_URL` 是**模块加载期快照**，动态切换后不会更新，新代码必须调用函数（源码注释明确）。已知限制：`/ai/chat` 仅注册在 SSE Server（独立容器 :8081），本地 develop 直连 `localhost:8080` 时 AI 对话不可用，本地联调需自行反代或直连 8081。

### 2.3 请求头与私有化标识（`utils/http.ts _authHeader`）

| 头 | 取值 | 条件 |
|----|------|------|
| `Authorization` | `Bearer <sessionId>` | 有会话时 |
| `Accept-Language` | `i18n.getLocale()`（zh/en/zh-Hant） | 恒定 |
| `X-Private-Api-Key` | `private_backend_api_key` | `backend_mode === 'private'` 且 Key 非空 |

> 请求超时分布（`utils/http.ts` 等）：常规 GET/POST/PUT/DELETE 20s、上传 30s、自动记录配置/心跳 5s、轨迹上报 10s、opslog 5s、SSE 200s。

### 2.4 私有化后端注册/注销（`pages/sub/BackendConfig/BackendConfig.ts`）

- 注册：`POST {WORKER_BASE_URL}/worker/register`，body `{url, apiKey}`，带登录态 Bearer；返回 `code==='0000'` 视为成功。
- 注销：`DELETE {WORKER_BASE_URL}/worker/register`，body `{apiKey}`。
- Worker 错误码映射：`4000` 地址/Key 不合法、`4001` 目标非开源版后端、`4002` 后端不可达、`4003` Key 被目标后端拒绝（`registerFailMessage`）。
- 「测试连接」先临时注册，再直连 Worker `GET /system/config` 校验 `data.mode === 'open'`；失败/成功但 URL 变化时按已保存配置恢复路由（`_cleanupTestRegistration`）。
- 保存成功后 `clearUserData()` + `wx.reLaunch('/pages/index/index')` 强制重登；页面隐藏时挂起 `_pendingReLogin` 到 `onShow` 执行。
- 存储键：`backend_mode`、`private_backend_url`、`private_backend_api_key`（`utils/storage.ts`）。

### 2.5 能力探测（`GET /system/config`）

`Vip` 页读取 `data.features.payment` / `data.features.freeVip` 与 `data.mode`；私有模式下本地兜底把两者都置 false。服务端 open 模式恒返回 `payment=false, wxmp=false`，`freeVip` 由 `FREE_VIP_ENABLED` 控制（默认 true；`backend/internal/system/handler.go`）。`User` 页在 `getBackendMode()==='private'` 时隐藏「我的邀请」入口。

## 3. 关键页面与交互

### 3.1 应用启动与登录（`app.ts` / `utils/auth.ts`）

- `onLaunch`：初始化 i18n 与主题；读 `papafeiji:autoRecordEnabled` 到 `globalData.openAutoRecorded`；参数 `inviter` → `setPendingInviter`；参数 `scene` → `_resolveSceneAndLogin`（`GET /invite/resolve?code=` 大写去符号，短码 <6 位跳过）；随后 `doLogin`。启动后 `checkForUpdate` 检查小程序热更新（有更新弹窗确认重启，失败静默弹窗）。
- 全局异常捕获：`onError` / `onUnhandledRejection` 统一记入 logger 并附当前页路由。
- `doLogin`：`request.login()` → `isLogin()` → 成功后置 `_needRefreshIndexList`、`tryRestoreAutoRecord()`（成功才记 30s 冷却）、`PUT /user/lang`。失败也尝试 `tryRestoreAutoRecord()`。
- `login()`：已有 session 先 `GET /user/profile` 校验；失败清 session。否则 `wx.login` → `POST /auth/login {code, inviter?}`（`closeTheErrorMessage=true, skipAuthExpire=true`）。登录并发单飞（`_loginFlight`）。
- 登录结果（`_applyLoginResult`）：存 sessionId、baseInfo；`GET /auto-record/config` 写 `familyConfig.autoRecordEnabled`；`newUser` → `setNeedShowXPa(true)` 且自动领取新用户 VIP（`claimNewUserFreeVip` → `POST /vip/new-user`）；有 `pendingLinkId` → 若当前页是 Family 就地返回，否则 `redirectTo /pages/Family/Family`；新用户 → `redirectTo /pages/Guide/Guide`。
- `onShow`：距上次恢复 >30s 则 `tryRestoreAutoRecord`；已登录则 `onAppShow()`，VIP 缓存缺失或超 5 分钟则 `fetchVipInfo()`。启动时 `flushOpsLog()`。
- 场景码解析与首屏并行、最多等 1.5s（`Promise.race`）：归属 `pages/index/index.ts`（`onLoad` 的 `_handleScene` → `_resolveInviterFromShortCode`，`onShow` 中等待）；解析完成后若已登录，由 index 页补调 `POST /auth/inviter` 绑定邀请人（幂等）。`app.ts _resolveSceneAndLogin` 只做场景码解析并经 `setPendingInviter` 把 inviter 并入 `/auth/login` payload（`utils/auth.ts`），不做补调。
- 会话过期：`utils/http.ts` 遇 HTTP 401 清 session 并 toast「会话已过期」，返回哨兵错误；后台请求可传 `skipAuthExpire` 避免误踢。

### 3.2 首页 `pages/index/index`

| 交互 | 行为 / 接口 |
|------|-------------|
| 首屏/下拉刷新/上拉加载 | `GET /diary/info?size=15&cursorDate=`（游标分页，累计上限 `MAX_LIST_SIZE=200`）；下拉刷新原位替换第一页，失败保留旧列表 |
| 统计面板 | `GET /diary/stats` → `firstRecordDate/recordDays/totalEntries/weeklyEntries` |
| 点统计卡/自动记录图标 | `toggleAutoRecord`：非 VIP 弹窗引导 `/pages/sub/Vip/Vip`；VIP 走 `openAutoRecord`/`closeAutoRecord`；开启成功后弹订阅提示 |
| 点「记录」 | `NoteEdit` 抽屉（首次先弹长按引导，写入 `memory_longpress_guide_shown`） |
| 长按「记录」 | `MemoryEdit` 记忆创建抽屉（点击引导内容不关闭） |
| 点「AI助手」 | `AIDrawer` 抽屉 |
| 删除卡片 | `NoteItem` 左滑 → 确认 → `DELETE /diary/info?id=family:<id>:date:<date>` |
| 从详情返回 | 若 `globalData._pendingCoverPoll`，按 0.5s 间隔最多 12 次轮询 `GET /diary/cover-url` 替换封面 |
| 统计卡右上角 | 自动记录状态图标 `location_on.svg`/`location_off.svg` |

### 3.3 日记详情 `pages/NoteDetail/NoteDetail`

- `onLoad` 解析 `baseInfo`（`{id, recordDate, dateName, coverImg}`），`id` 形如 `family:<familyId>:date:<YYYY-MM-DD>`；无有效 `recordDate` 时 toast；页面栈深 >1 时 `wx.navigateBack()`，仅栈内只有 1 页时 `redirectTo` 首页。
- 详情：`GET /diary/details?diaryId=&page=&size=20`（offset 分页，`MAX_FULL_LIST=200`）；响应 `extra.coverImg/coverImage/memories`；记忆与条目合并按时间排序（memory 排后）。
- 家庭成员 Tab：按 `familyMemberUserId` 聚合，仅 >1 人时显示；切换按成员过滤。
- 编辑：仅 `familyMemberUserId === currentUserId` 可编辑，否则 toast「不能编辑他人记录」；`NoteEdit` 提交后 `reloadAfterEdit`。
- 记忆：点记忆条 `bindMemoryEdit`、单条删除 `DELETE /diary/details/memory?id=`。
- 封面：`CoverEdit` 抽屉 → `PUT /diary/info {id, coverImage}`；**前端未选中图片时直接关闭抽屉、不发送空 coverImage**，因此「清空封面降级（manual→image→trajectory→default）」仅是后端 API 语义，前端无入口，只能经 API 直接触发；清空时后端锁内先写入 default、轨迹图在锁释放后异步生成，其间封面为 default。
- 分享：`onShareMoment` 确保二维码（`POST /invite/qrcode {raw:true}`）→ 离屏 canvas 生成图片 → `wx.showShareImageMenu`。
- 二维码在 `onShow` 预生成；生成中复用 in-flight Promise。
- 整日删除：`DELETE /diary/info`（入口在首页卡片，详情页自身无整日删除按钮）。
- 封面工具：详情页封面上有 `AutoBtn`（自动记录开关）与「更换封面」入口（`bindCircle`）；编辑提交后本页自行 `_startCoverPolling`（0.5s、最多 12 次）刷新，另有首页 `_pendingCoverPoll` 路径。列表渲染上限 `MAX_IMAGE_LIST=50`、`MAX_TAB_LIST=50`，分片写入 `CHUNK=50`。

### 3.4 家庭 `pages/Family/Family` 与邀请 `pages/Invite/Invite`

- Family `onShow`：有 `pendingLinkId` 时先登录并 `POST /family/invite-link/join {linkId}`（残留 linkId 超 7 天自动清除，防失效邀请反复重放）；否则 `GET /family` 渲染成员；成员非空且无 `inviteLinkId` 时 `POST /family/invite-link` 生成链接。
- `onShareAppMessage`：有 linkId → `/pages/Family/Family?linkId=<id>`；无则回 `/pages/index/index`。
- 退出/移除：本人 `POST /family/leave`，他人 `DELETE /family/members/{userId}`，均需 `ConfirmDialog` 二次确认（danger）。
- Invite `onShow`：登录 → `GET /invite/list`（取前 50）→ `POST /family/invite-link`；`POST /invite/qrcode` 预生成邀请图。
- Invite `onShareAppMessage`：默认分享路径 `/pages/index/index?inviter=<userId>`；家庭分享（`data-share-type="family"`）路径 `/pages/Family/Family?linkId=`。
- 保存邀请图：下载 → `wx.showShareImageMenu`，带 `entrancePath=/pages/index/index?inviter=<userId>`；有缓存路径；保存中防重入。

### 3.5 用户 `User` / 设置 `Set` / 常用地址 `CommonAddresses`

- User `onShow`：`request.login()` → 刷新 `formatVipInfo(getVipInfo())`；入口清单见第 1 节；私有模式隐藏「我的邀请」。
- Set `onShow` 并行 `getFamilyConfig()`（`GET /auto-record/config`）、`GET /auth/phone`、`GET /user/profile`。
- 昵称：`PUT /user/nickname`，前端限 20 字（后端校验在 `validator.ValidateNickname`）；请求体字段名与后端绑定 tag 一致为 `{nickName}`；成功后更新 baseInfo 与 `_needRefreshIndexList`。
- 头像：`onChooseAvatar` → `updateAvatar`（`POST /file/upload?type=avatar` → `PUT /user/avatar {fileId}`）。
- 主题 `onThemeTap`：auto/light/dark（`themeManager`，storage `ppfj_theme_mode`）。
- 语言 `onLanguageTap`：auto/zh/en/zh-Hant（`i18n`，storage `ppfj_lang_mode`），选择后 `PUT /user/lang`。
- 手机号：`getPhoneNumber` → `POST /auth/phone/bind {code}`；已绑定且当天已改 → toast 日限；解绑 `POST /auth/phone/unbind`（无日限，不消耗微信认证）。
- 服务号：未订阅显示二维码弹窗（`getHelpBaseURL()/follow.png`）。
- 桌面快捷：Android 且 `canIUse('addToDesktop')` 直接添加，否则引导；可跳 `wx.openAppAuthorizeSetting`。
- 私有模式（`backend_mode==='private'`）隐藏头像更换入口、手机号绑定/更换、手机号解绑与服务号入口（`Set.wxml` 的 `wx:if="{{!isPrivateBackend}}"`）。
- CommonAddresses `onShow`：`POST /user/common-addresses/refresh` 取回并按 `count` 降序；点地址弹编辑抽屉，`PUT /user/common-addresses/{oldName} {newName}`；名 >100 字/空拦截。

### 3.6 VIP `pages/sub/Vip/Vip`

| 交互 | 行为 / 接口 |
|------|-------------|
| 商品加载 | `GET /vip`，过滤 `type!=='free' && type!=='trial'`，取第一条价格 `amount/100`，默认选中第一档 |
| 系统配置 | `GET /system/config`；`features.payment/freeVip` 控制入口；私有模式本地兜底关闭 |
| 无支付能力 | `features.payment===false` 时展示「当前为私有化部署，会员请联系管理员开通」（i18n `vip.openSourceNoPayment`） |
| 购买 | 未勾选协议先弹协议弹窗；`doPay` → `POST /payment/virtual/request {vipId, env}` → `wx.requestVirtualPayment` |
| env | iOS 恒 0；否则 develop/trial → 1，release → 0（`utils/pay.ts getPayEnv`） |
| 轮询 | `GET /payment/virtual/status?outTradeNo=` 每 3s、最多 10 次；`paid` → 成功弹窗 + 刷新 VIP；`closed` → 失败提示；超时 → 超时提示 |
| 免费领取 | `GET /vip/free` → `POST /vip/free/claim {vipId}`；`FREE_VIP_ALREADY_CLAIMED` → 已领取提示 |
| VIP 刷新 | `onShow` 与支付成功均 `fetchVipInfo()`；VIP 信息 60s 内存缓存 + storage |

> 后端实现细节见 `docs/spec/02e-vip-payment.md`（VP-1~VP-13）。前端在支付取消/失败/中止路径 best-effort 调用 `POST /payment/virtual/cancel`（`utils/pay.ts`）。

### 3.7 AI 抽屉 `components/AIDrawer/AIDrawer`

- 打开需登录（`index.openAIDrawer` 先 `ensureLogin`）；`sendPrompt` 输入 ≤2500 字符（与后端 `maxMessageCodePoints` 统一）。
- 请求体仅 `{message, request_id}`（不传 history，上下文由后端组装；本地列表裁剪至 50 条仅为 UI 展示），`eventSource POST {base}/ai/chat`，headers `Authorization`（私有模式加 `X-Private-Api-Key`）。
- 流事件：`data` 增量（120ms 合并 setData）、`done` 结束（Markdown→HTML、解析卡片日期标记）、`error`（`{"code","biz_code","bizCode","message"}`；`eventSource` 同时读取 `biz_code` 与旧 `bizCode`；`biz_code=AI_DAILY_QUOTA_EXCEEDED` → 配额弹窗引导 Vip，否则 toast）。
- 网络类失败自动重连一次（复用同 `request_id`）；重连前清空半截输出。消息上限 `MAX_MESSAGE_COUNT=50`。
- 防护边界：`eventSource` 读缓冲上限 64KB（超限主动 abort 报错）、整体 200s 超时；AIDrawer 用 `_sendSeq` 发送代次防旧流回调复位发送锁（连点防护），支持 `retryLast` 重发上一条。
- 隐藏/销毁：`hide`/`onPageHide`/`detached` 中止 SSE；切后台不取消登录写操作。`detached` 后的 flush 因 `_isDetached` 已置位而成为 no-op，未落盘的半截文本不保留。
- 日记卡片：`POST /diary/info/dates {dates}` 回填，按 `_msgId` 定位，避免列表裁剪错挂。

### 3.8 记忆 `MemoryEdit` / 封面 `CoverEdit` / 条目 `NoteEdit`

- `NoteEdit`：定位（`wx.getLocation gcj02`，15s 超时；`GET /location/reverse` 逆解析并行 `GET /user/common-addresses`，300m 内命中常用地址则替换名称；手动 `wx.chooseLocation` 不替换）；文本 ≤140 字（前端），图片 ≤9 张、单张 ≤10MB；提交 `POST /diary/details`（新建）/`PUT /diary/details`（`form.id` 存在）；上传走 `POST /file/upload?type=recordImg`（并发 3，压缩 quality 50/65/80；部分失败重试仅补传失败项，成功项回写 file id 不重传）。普通业务重复提交仅由 UI loading 拦截。
- `MemoryEdit`：标题 1~50 字、内容 ≤10000 字、时间必填；新建 `POST /diary/details/memory`、编辑 `PUT /diary/details/memory`。
- `CoverEdit`：从当日图片选中 → `PUT /diary/info {id, coverImage}`；未选图直接关闭；核心业务保留 `_submitting` 入口拦截。
- `RecordItem` / `MemoryItem` / `NoteItem`：左滑（位移 >30px）显示删除；确认后分别 `DELETE /diary/details`、`DELETE /diary/details/memory`、`DELETE /diary/info`；删除成功置 `globalData._needRefreshIndexList=true`。

### 3.9 MCP `pages/sub/Mcp/Mcp`

- `onLoad`/`onShow`（从后台返回且非 loading）：`GET /mcp/key` 渲染 `apiKey/apiUrl/memoryUrl/authUrl/mcpConfig`。
- 生成：`POST /mcp/key`；换发：`POST /mcp/key/rotate`（`ConfirmDialog` 确认）；均为核心业务，`_generating` 入口锁。
- `authUrl` 为空时「连接器」Tab 回退到 `mcp`。
- 复制 API Key / 配置 / 绑定链接走 `wx.setClipboardData`。

### 3.10 其他页面

- `Guide`：4 张 OSS 教程图（按语言后缀 `_en`），`next` 到第 4 步写 `setNeedShowXPa(false)` 并 `redirectTo` 首页；入口另有 `pages/User/User.ts` 的 `toGuidePage`（`redirectTo`）。
- `About`：`loadAccountName` 取 baseInfo.nickName；注销需在弹窗内输入与昵称完全一致的文本（`canConfirmDelete`），确认后 `DELETE /auth/account {confirmName}` → 成功清本地/关自动记录/回首页。本地清理不因页面隐藏/销毁跳过，仅成功 toast 受可见性约束。
- `WebPage`：仅允许 `https://` 且 host 命中 `papafeiji.cn` / `xiaohongshu.com`（含子域）；非法 toast 并返回。
- `UsageGuide`：`openUrl` 打开 `/tutorial/tutorial0..7/`。
- `BackendConfig`：见第 2.4 节。

## 4. 全局交互规则（加载 / 错误 / Toast / 四态）

### 4.1 加载（Loading）

- `utils/http.ts` 引用计数式 `_showLoading`/`_hideLoading`（计数归零才 `wx.hideLoading`）；`resetLoading()` 直接清零并隐藏。
- 页面 `onHide`/`onUnload`/`detached` 普遍调用 `resetLoading()`，避免系统调用触发 hide 后 loading 卡死。
- 图片上传/头像更新内部 `wx.showLoading({mask:true})`；列表页首屏多用 `wx.showLoading`。

### 4.2 错误与 Toast（`utils/http.ts`）

| 场景 | 行为 |
|------|------|
| HTTP 401 | `clearSessionId()` + toast `error.sessionExpired`（2s）；`skipAuthExpire=true` 时跳过（后台轮询/自动记录） |
| HTTP 5xx | toast `error.serverError`（带 statusCode）；私有模式下 Worker `code 5020/5030` → `error.privateBackendUnreachable` |
| 业务 `code !== '0000'` | `_handleResponseError` toast（2s）：优先按 `biz_code` 查 `utils/errorMessages.ts` 映射显示三语本地化文案（`error.biz*` 键，与 `pkg/errors/codes.go` 枚举同源），未登记码回退后端 `message/msg`；私有模式 `4031` → `error.privateBackendNotRegistered` |
| 上传失败 | `wx.showModal`（非 toast）；配额超限 `USER_IMAGE_STORAGE_LIMIT_EXCEEDED` 弹升级引导 |
| Toast 节流 | 会话过期与业务错误各 200ms 节流；当前页已销毁/隐藏时不弹 |
| 错误信息 | `getErrorMessage` 优先返回 Error.message/msg/data.msg，内部哨兵不直接展示 |

### 4.3 四态约定（列表/详情类页面）

| 状态 | 实现 | 示例 |
|------|------|------|
| 加载态 | `loading/_loading` 字段 + `wx.showLoading` / 底部「加载中」 | `index.loading`、`NoteDetail._loading` |
| 空态 | `empty` 视图（`image/empty.png` + 文案） | `index` 空列表、`NoteDetail.isEmpty` |
| 内容态 | 列表/时间线渲染；大数据量分片写入（单次 ≤50 条） | `index` 增量写、`NoteDetail` `_chunkSetFilteredList` |
| 错误态 | 请求失败 toast + 保留/清空策略；下拉刷新失败保留旧内容 | `index.fetch` 失败 toast、`handlePullRefresh` 回滚游标 |

### 4.4 弹窗 / 抽屉 / 表单

- 统一 `Drawer`（底部抽屉）、`ConfirmDialog`（`cancelText/confirmText/confirmType`）、`wx.showModal`。
- `onHide` 保留编辑类抽屉（编辑/记忆/封面），关闭非编辑类（订阅提示、loading、确认弹窗）；系统调用（`chooseLocation`/`chooseMedia`/`chooseAvatar`）也会触发 `onHide`。
- 写操作（保存/上传/登录/支付/家庭操作）的 CancelToken 不在 `onHide` 取消，仅在 `onUnload`/`detached`/完成时取消。
- 防重点击锁普遍存在且按页面命名：`_submitting`（CoverEdit）、`_generating`（Mcp）、`_changing`（index 自动记录开关，onHide 不复位，防系统调用返回后重复开关）、`_paying`/`_claiming`/`_showingAgreement`（Vip）、`_savingImage`（Invite）、`deleting`（About）、`loading`（CommonAddresses/Mcp 防重拉）。

### 4.5 生命周期与数据写入约定

- 所有 UI 状态写入经 `_safeSetData`（隐藏时暂存 `_pendingSetData`，`show` 时 `_applyPendingSetData` flush）或 `_forceSetData`（`onHide`/`onUnload` 必须持久化时，豁免 hidden 检查但短路已销毁/已脱离）。
- `behaviors/safeSetData.ts` 统一管理 `_isDetached`（`detached`）与 `_isHidden`（`hide`/`show`）；`_isDestroyed` 由各页/组件在 `onUnload`/`detached` 自行置位。
- `onUnload`/`detached` 取消全部读请求 token、清定时器、`unsubscribeTheme`。
- 页面持有 AIDrawer 时在 `onHide` 调用其 `onPageHide()`。

### 4.6 主题与国际化

- `behaviors/theme.ts` 订阅 `themeManager`；`behaviors/i18n.ts` 订阅 `i18n`，暴露 `$t` 与扁平的 `_i18n`（数组不拍平，JS 用 `i18n.t` 直取）。
- 语言：`auto/zh/en/zh-Hant`；`auto` 依 `wx.getAppBaseInfo().language` 映射；文案缺失回退 `zh`，`{{var}}` 插值。
- 主题：`auto/light/dark`；系统主题变化仅在 `auto` 时跟随；`wx.onThemeChange` 触发时同步清 `getSystemInfo` 缓存，防运行期主题不一致。

## 5. 响应式与适配规则

- 样式以 `rpx` 为主；自定义导航栏按胶囊位置计算内边距与高度，兼容无胶囊的模拟器/多端环境。
- `utils/util.ts getSystemInfo` 优先新 API（`getWindowInfo/getDeviceInfo/getAppBaseInfo`），回退 `getSystemInfoSync`；计算 `statusBarHeight/navbarHeight/bottomSafeHeight/capsuleInfo` 并缓存，`wx.onWindowResize` 清缓存。
- 深色模式：`theme.json` + `app.less` CSS 变量；`navigation-bar` 背景/文字随主题。
- 低版本基础库：`eventSource` 对 `onHeadersReceived/onChunkReceived/off*` 做存在性守卫；`themeManager` 检测 `wx.onThemeChange`。
- 响应式布局边界：AI 抽屉消息 50 条；首页列表单页 15、累计上限 200；详情列表全量上限 200（分片写入 50）；图片 ≤9 张。
