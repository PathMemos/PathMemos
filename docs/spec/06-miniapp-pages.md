# PP-06 小程序页面与交互（L6）

> 层级：L6 前端/交互｜版本：当前（以代码为唯一事实源）
> 上游：PP-01 产品总览｜关联分册：PP-02a~02h
> 说明：本分册按当前代码实现整理；行内路径指向对应实现位置。
> 关键实现位置：`frontend/miniapp/miniprogram/`（`app.json`、`app.ts`、`config/index.ts`、`pages/`、`components/`、`utils/`、`behaviors/`）；关键处内联源文件路径。
> 其他实现文件：`polyfills/textDecoder.ts`（SSE UTF-8 流解码，位于 miniprogram 根目录）、`utils/markdown.ts`（渲染）、`utils/concurrency.ts`（并发 3）、`utils/appShare.ts`（多端 App 分享图缩略图压缩与用户路径转换，NoteDetail/Invite 分享链路共用）、`utils/request.ts`（http/auth/vip 聚合转发桶文件）、`lib/dayjs`（时间库）、`app.miniapp.json`（多端 App 适配声明，`adapteByMiniprogram.userName` 指向小程序原始 ID `gh_b8e6cc7fb7aa`，与 `utils/pay.ts` 跳转支付目标同源；人工双写、无自动一致性校验（有意取舍：单一小程序原始 ID 变更频率趋零，校验机制成本不抵））；`app.json` 的 `sitemapLocation` 指向 `sitemap.json`。

## 1. 页面清单（路由 / 用途 / 入口 / 优先级）

页面与分包声明见 `app.json`；页面为自定义导航（`"navigationStyle": "custom"`），无 tabBar。

| 路由 | 分包 | 用途 | 入口 | 优先级 |
|------|------|------|------|--------|
| `pages/index/index` | 主包 | 首页：日记卡片时间轴、统计面板、自动记录开关、编辑/AI/记忆抽屉 | 小程序启动页（`app.json` pages[0]） | P0 |
| `pages/NoteDetail/NoteDetail` | 主包 | 当日日记详情：家庭成员筛选、条目/记忆时间线、封面、分享图 | `index` 的 NoteItem、AIDrawer 卡片（`gotoDetail`） | P0 |
| `pages/Family/Family` | 主包 | 家庭：成员列表、邀请链接分享、退出/移除成员 | `pages/User` → `toFamily`；分享卡片 `?linkId=` | P0 |
| `pages/sub/Vip/Vip` | 分包 `pages/sub` | VIP 中心：商品选择、虚拟支付、免费 VIP 领取 | `User`/`index`(VIP 提示)/`Set`/`AIDrawer`(配额)/`NoteEdit`/`AutoBtn` 等 `navigateTo` | P0 |
| `pages/User/User` | 主包 | 个人中心：VIP 状态、各功能入口 | `index` 导航栏 home 按钮 `goToUser` | P1 |
| `pages/Set/Set` | 主包 | 设置：昵称、头像、主题、语言、手机号、常用地址、服务号、桌面快捷、退出登录（底部红色按钮，确认弹窗 → best-effort `POST /auth/logout` → 清本地 → 登录页） | `User` → `toSet` | P1 |
| `pages/Guide/Guide` | 主包 | 新用户 4 步引导图 | 新用户登录后 `wx.redirectTo`（`utils/auth.ts`）；Family 页 `toIndex` 时 `needShowXPa`；首页 onShow `needShowXPa` 兜底 redirectTo（堵家庭卡片冷启动等非登录页入口跳过教程的缺口） | P1 |
| `pages/Login/Login` | 主包 | 登录页：微信一键登录 | 启动登录失败 `app.ts _redirectToLogin` | P1 |
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
| `style` / `componentFramework` | `v2` / `glass-easel`（新一代组件框架，WebView 渲染需基础库 ≥3.8.12，`project.config.json` libVersion 同步；Donut 多端运行时本身基于 glass-easel；各页面 `.json` 均显式声明 `"renderer": "webview"`） |
| `lazyCodeLoading` | `requiredComponents` |
| `preloadRule` | `pages/index/index` → `network: all` 预下载分包 `pages/sub`（消除首次点入分包等待） |
| `requiredPrivateInfos` | `getLocation`、`onLocationChange`、`chooseLocation`、`startLocationUpdate`、`startLocationUpdateBackground` |
| `requiredBackgroundModes` | `location` |
| `permission.scope.userLocation.desc` | 「用于记录日记位置」 |

前端测试位于 `frontend/miniapp/test/*.test.ts`（以目录实际为准，当前 10 套件 / 57 用例），本地用 `make test-frontend` 执行；组件级测试由 `test/jest.setup.ts` 注入 `Component/Page/Behavior` 捕获实现。按 spec-standards §五 验收策略，前端测试为**存量资产、非验收门禁**：不要求随新功能新增用例，前端行为以人工验收（L7）为准；CI（`.github/workflows/ci.yml`）跑三个 job：backend（lint + go test + 各项同步校验）、frontend（`npm run lint`，不含 Jest）、worker（typecheck）——前端/Worker 测试不在 CI。

- `utils/logger.ts`：分级日志接口（log/info/warn/error），**所有级别一律写入内存缓冲**（上限 100 条，超出裁掉最旧）；release 环境差异仅是丢弃 `detail` 字段（logger 本身不向 console 输出，无级别过滤）；所有字符串 message 先经 `sanitizeUrlForLog` 剥离 query/hash 再入缓冲，防 URL 参数落日志。
- `utils/opslog.ts`：客户端操作日志本地持久化（key `ops_log_queue`，上限 200、单批 50），打开小程序时批量上报 `POST /ops/client-log`；detail 仅保留计数/标识/错误码等非敏感摘要。

组件（16 个）与 behaviors（5 个）清单：`AIDrawer`（AI 对话抽屉）、`NoteEdit`/`MemoryEdit`/`CoverEdit`（编辑抽屉）、`RecordItem`/`MemoryItem`/`NoteItem`（左滑删除列表项）、`AutoBtn`（日记详情/编辑内的自动记录开关，内含订阅提示联动；首页开关是 `pages/index/index.ts` 的 `toggleAutoRecord`）、`CalendarPicker`（日历选日期，非法输入回退今天）、`TimePicker`（时间选择）、`Cell`（通用设置行）、`PTextarea`（受控多行输入）、`Drawer`/`ConfirmDialog`/`SubscribePrompt`（通用抽屉/确认弹窗/订阅提示）、`navigation-bar`（自定义导航栏）；behaviors：`safeSetData`（三标志安全写入）、`theme`/`i18n`（主题/文案订阅）、`touchSwipe`（左滑手势）、`transition`（动效）。

## 2. base URL 模式与运行时配置

### 2.1 三种后端地址（`config/index.ts`）

| 常量 | 值 | 用途 |
|------|----|------|
| `SAAS_BASE_URL` | `https://pro.papafeiji.cn` | SaaS 默认直连源站 |
| `WORKER_BASE_URL` | `https://api.pathmemos.com` | 私有化/开源版经 Cloudflare Worker 路由（`backend_mode === 'private'`） |
| `DEV_BASE_URL` | `http://localhost:8080` | 开发版本地后端联调（需 Storage 写 `dev_use_local_backend=true`，默认不启用） |
| `OSS_PUBLIC_URL` | `https://ppfj-images.oss-cn-hangzhou.aliyuncs.com` | 教程/引导等公共静态图 |
| `getHelpBaseURL()` | `https://papafeiji.cn` | 帮助/协议/教程站点（私有模式同样指向 SaaS） |
| `EXPORT_GUIDE_URL` | 小红书链接 | 分享导出引导 |
| `NEW_USER_FREE_VIP_ID` | `vip-free-0001` | 免费 VIP 查重 id |
| `ABNORMAL_TEMPLATE_ID` | `i7mcEEMDbhYU1oAC1-E0G0xvIBCmTl6f9c3jOq11m3g` | 异常提醒订阅模板 |

**异常提醒订阅仅小程序通道**：订阅提示组件固定走 `wx.requestSubscribeMessage`（`ABNORMAL_TEMPLATE_ID`）。App 端通知已裁撤（不接开放平台一次性订阅，App 环境不弹订阅授权、不记订阅）。`project.miniapp.json` 的 `mini-android.uselessPermissions` 已登记 `BLUETOOTH`/`BLUETOOTH_ADMIN`（代码零使用、蓝牙扩展未启用），随基座重建从 Android 清单摘除。

### 2.2 地址决策顺序（`getBaseURL()` / `getSSEBaseURL()`，`config/index.ts`）

1. `envVersion === 'develop'` 且 Storage `dev_use_local_backend=true` → `DEV_BASE_URL`（开发版默认走 `SAAS_BASE_URL`，工具联调生产接口）；
2. 否则读 `wx.getStorageSync('backend_mode')`（`STORAGE_KEY_MODE`）：`'private'` → `WORKER_BASE_URL`；
3. 其余（含读取失败）→ `SAAS_BASE_URL`。

`getSSEBaseURL()` 恒等于 `getBaseURL()`。导出常量 `BASE_URL` / `SSE_BASE_URL` 是**模块加载期快照**，动态切换后不会更新，新代码必须调用函数（源码注释明确）。已知限制：`/ai/chat` 仅注册在 SSE Server（独立容器 :8081），本地 develop 直连 `localhost:8080` 时 AI 对话不可用，本地联调需自行反代或直连 8081。

### 2.3 请求头与私有化标识（`utils/http.ts _authHeader`）

| 头 | 取值 | 条件 |
|----|------|------|
| `Authorization` | `Bearer <sessionId>` | 有会话时 |
| `Accept-Language` | `i18n.getLocale()`（zh/en/zh-Hant） | 恒定 |
| `X-Private-Api-Key` | `private_backend_api_key` | `backend_mode === 'private'` 且 Key 非空 |

> 请求超时分布（`utils/http.ts` 等）：常规 GET/POST/PUT/DELETE 20s、上传 30s、自动记录配置/心跳 5s、轨迹上报 10s、opslog 5s、SSE 200s。上传链路有效上限 = 各层最小值：主路径（小程序直连）由客户端 30s 主导（nginx `/` 60s、服务端 TimeoutHandler 5min 为兜底预算）；私有模式 Worker 链路 360s（02h §6.1）。

### 2.4 私有化后端注册/注销（`pages/sub/BackendConfig/BackendConfig.ts`）

- 注册：`POST {WORKER_BASE_URL}/worker/register`，body `{url, apiKey}`，带登录态 Bearer；返回 `code==='0000'` 视为成功。
- 注销：`DELETE {WORKER_BASE_URL}/worker/register`，body `{apiKey}`。
- Worker 错误码映射：`4000` 地址/Key 不合法、`4001` 目标非开源版后端、`4002` 后端不可达、`4003` Key 被目标后端拒绝（`registerFailMessage`）。
- 「测试连接」先临时注册，再直连 Worker `GET /system/config` 校验 `data.mode === 'open'`；失败时经 `_cleanupTestRegistration` 清理/恢复路由（生效 Key 相同则重注册已保存 URL、否则直接注销），成功但 URL 变化时直接用已保存 URL 重注册（`_registerPrivateBackend`）恢复。
- 保存成功后 `clearUserData()` + `wx.reLaunch('/pages/index/index')` 强制重登；页面隐藏时挂起 `_pendingReLogin` 到 `onShow` 执行。
- 存储键：`backend_mode`、`private_backend_url`、`private_backend_api_key`（`utils/storage.ts`）。

### 2.5 能力探测（`GET /system/config`）

`Vip` 页读取 `data.features.payment` / `data.features.freeVip` 与 `data.mode`；私有模式下本地兜底把两者都置 false。服务端 open 模式恒返回 `payment=false, wxmp=false`，`freeVip` 由 `FREE_VIP_ENABLED` 控制（默认 true；`backend/internal/system/handler.go`）。`User` 页在 `getBackendMode()==='private'` 时隐藏「我的邀请」入口。App（多端）环境的支付交互见 02e VP-13-AC6：跳转微信小程序 `Vip` 页完成虚拟支付，返回后本页 `onShow` 刷新权益。

## 3. 关键页面与交互

### 3.1 应用启动与登录（`app.ts` / `utils/auth.ts`）

- `onLaunch`：初始化 i18n 与主题；读 `papafeiji:autoRecordEnabled` 到 `globalData.openAutoRecorded`；参数 `inviter` → `setPendingInviter`；参数 `scene` → `_resolveSceneAndLogin`（`GET /invite/resolve?code=` 大写去符号，短码 <6 位跳过；解析失败走统一错误通道 **toast**（`closeTheErrorMessage=false`）——启动期扫到失效/过期场景码时用户会看到业务错误提示）；随后 `doLogin`。启动后 `checkForUpdate` 检查小程序热更新（有更新弹窗确认重启，失败弹窗提示且不可取消——`onUpdateFailed` 的 `showModal showCancel:false`）。
- 全局异常捕获：`onError` / `onUnhandledRejection` 统一记入 logger 并附当前页路由。
- `doLogin`：`request.login()` → `isLogin()` → 成功后置 `_needRefreshIndexList`、`tryRestoreAutoRecord()`（成功才记 30s 冷却）、`PUT /user/lang`。失败也尝试 `tryRestoreAutoRecord()`，并经 `_redirectToLogin` 跳登录页（当前已在 Login 页或页面栈尚未就绪时跳过）。
- `login()`：已有 session 先 `GET /user/profile` 校验——仅 401 会话过期哨兵（`isSessionExpiredError`）才清 session 重登，取消/网络/5xx 保留现有会话。小程序环境统一 `wx.login` → `POST /auth/login {code, inviter?}`（`closeTheErrorMessage=true, skipAuthExpire=true`）。登录并发单飞（`_loginFlight`）。
- 登录页 `pages/Login/Login`：微信一键登录——小程序环境静默 `wx.login` → `POST /auth/login`；多端 App 环境点按直接 `wx.weixinAppLogin` 拉起微信授权拿 code → `POST /auth/login/app`。整屏为内置教程图 guide-1 主视觉（aspectFill 铺满全屏，按语言切换 `login_guide1{,_en}.png`，品牌文案由图承载），点击任意处即登录；登录中以全屏 loading 反馈。登录成功后延时 400ms 确认页面栈仍停留本页（`_applyLoginResult` 的 Guide/Family 跳转未接管时）再回首页，另有 800ms reLaunch 兜底（仍停留 Login 则强制切首页并上报 `login_step{redirect_relaunch}`）。
- 登录结果（`_applyLoginResult`）：存 sessionId、baseInfo；`GET /auto-record/config` 写 `familyConfig.autoRecordEnabled`；`newUser` → `setNeedShowXPa(true)` 且自动领取新用户 VIP（`claimNewUserFreeVip` → `POST /vip/new-user`）；有 `pendingLinkId` → 若当前页是 Family 就地返回，否则 `redirectTo /pages/Family/Family`；新用户 → `redirectTo /pages/Guide/Guide`。
- `onShow`：距上次恢复 >30s 则 `tryRestoreAutoRecord`；已登录则 `onAppShow()`，VIP 运行时缓存缺失或超 5 分钟轮询间隔（autoRecord 自有层，权威源 02c §7；vipInfo 拉取缓存 60s 见 02e §7）则 `fetchVipInfo()`。启动时 `flushOpsLog()`（补报登录失败滞留的 `login_fail` 等，见 02a §7）。首页 onShow 另有登录门禁：`getLoggedOut() && !getSessionId()` → reLaunch 登录页（主动退出/注销停留标记，清除时机见 02a §7）。
- 场景码解析与首屏并行、最多等 1.5s（`Promise.race`）：归属 `pages/index/index.ts`（`onLoad` 的 `_handleScene` → `_resolveInviterFromShortCode`，`onShow` 中等待）；解析完成后若已登录，由 index 页补调 `POST /auth/inviter` 绑定邀请人（幂等）。`app.ts _resolveSceneAndLogin` 只做场景码解析并经 `setPendingInviter` 把 inviter 并入 `/auth/login` payload（`utils/auth.ts`），不做补调。
- 会话过期：`utils/http.ts` 遇 HTTP 401 清 session 并 toast「会话已过期」，返回哨兵错误；后台请求可传 `skipAuthExpire` 避免误踢。

### 3.2 首页 `pages/index/index`

| 交互 | 行为 / 接口 |
|------|-------------|
| 首屏/下拉刷新/上拉加载 | `GET /diary/info?size=15&cursorDate=`（游标分页，累计上限 `MAX_LIST_SIZE=200`）；下拉刷新原位替换第一页，失败保留旧列表 |
| 统计面板 | `GET /diary/stats` → `firstRecordDate/recordDays/totalEntries/weeklyEntries` |
| 点统计卡/自动记录图标 | `toggleAutoRecord`：非 VIP 弹窗引导 `/pages/sub/Vip/Vip`；VIP 走 `openAutoRecord`/`closeAutoRecord`；开启成功后弹订阅提示（仅小程序订阅通道，见 §2.1） |
| 点「记录」 | `NoteEdit` 抽屉（首次先弹长按引导，写入 `memory_longpress_guide_shown`） |
| 长按「记录」 | `MemoryEdit` 记忆创建抽屉（点击引导内容不关闭） |
| 点「AI助手」 | `AIDrawer` 抽屉 |
| 删除卡片 | `NoteItem` 左滑 → 确认 → `DELETE /diary/info?id=family:<id>:date:<date>` |
| 从详情返回 | 若 `globalData._pendingCoverPoll`，按 0.5s 间隔最多 12 次轮询 `GET /diary/cover-url` 替换封面 |
| 统计卡右上角 | 自动记录状态图标 `location_on.svg`/`location_off.svg` |

### 3.3 日记详情 `pages/NoteDetail/NoteDetail`

- `onLoad` 解析 `baseInfo`（`{id, recordDate, dateName, coverImg, coverImage}`），`id` 形如 `family:<familyId>:date:<YYYY-MM-DD>`；无有效 `recordDate` 时 toast；页面栈深 >1 时 `wx.navigateBack()`，仅栈内只有 1 页时 `redirectTo` 首页。
- 详情：`GET /diary/details?diaryId=&page=&size=20`（offset 分页，`MAX_FULL_LIST=200`）；响应 `extra.coverImg/coverImage/memories`；记忆与条目合并按时间排序（memory 排后）。
- 家庭成员 Tab：按 `familyMemberUserId` 聚合，仅 >1 人时显示；切换按成员过滤。
- 编辑：仅 `familyMemberUserId === currentUserId` 可编辑，否则 toast「不能编辑他人记录」；`NoteEdit` 提交后 `reloadAfterEdit`。
- 记忆：点记忆条 `bindMemoryEdit`、单条删除 `DELETE /diary/details/memory?id=`。
- 封面：`CoverEdit` 抽屉 → `PUT /diary/info {id, coverImage}`；**前端未选中图片时直接关闭抽屉、不发送空 coverImage**，因此「清空封面降级（manual→image→trajectory→default）」仅是后端 API 语义，前端无入口，只能经 API 直接触发；清空时后端锁内先写入 default、轨迹图在锁释放后异步生成，其间封面为 default。
- 分享（服务端出图）：`onShareMoment` 确保二维码（`POST /invite/qrcode {raw:true}`）→ `_buildShareCardRequest` 组装展示串/记录/图片 URL（全部按当前 locale 客户端格式化，服务端是纯「文本→像素」引擎，契约见 `backend/internal/sharecard/render.go` ShareCardRequest）→ `POST /diary/share-card`（70s 超时）得 {poster, thumb} → `wx.downloadFile` 下载两端产物 → 小程序端 `wx.showShareImageMenu`（自带原生预览）；多端 App 端（`wx.miniapp` 存在）先 `wx.previewImage` 全屏预览 + `wx.showModal` 确认（分享前预览，对齐小程序体验），再场景选择弹窗（会话/朋友圈/收藏）→ `wx.miniapp.shareImageMessage {imagePath, thumbPath, scene}`；缩略图下载失败回退原图（客户端 `ensureThumbUnderLimit` 兜底压至 64KB 内）；失败上报 `share_diag`。取舍登记：App 端 `wx.previewImage` 的 `complete` 回调丢失（低概率端上异常）时该次分享流程悬挂且 `_generatingShare` 停留 true，无超时兜底——锁随页面实例重建复位，影响限于当前页实例，按「小概率异常容忍」不加超时竞态。
- 二维码在 `onShow` 预生成；生成中复用 in-flight Promise。
- 整日删除：`DELETE /diary/info`（入口在首页卡片，详情页自身无整日删除按钮）。
- 封面工具：详情页封面上有 `AutoBtn`（自动记录开关）与「更换封面」入口（`bindCircle`）；编辑提交后本页自行 `_startCoverPolling`（0.5s、最多 12 次）刷新，另有首页 `_pendingCoverPoll` 路径。列表渲染上限 `MAX_IMAGE_LIST=50`、`MAX_TAB_LIST=50`，分片写入 `CHUNK=50`。

### 3.4 家庭 `pages/Family/Family` 与邀请 `pages/Invite/Invite`

- Family `onShow`：有 `pendingLinkId` 时先登录并 `POST /family/invite-link/join {linkId}`（残留 linkId 超 7 天自动清除，pendingInviter 同 7 天 TTL，见 02a §7；join 业务失败带 `error.code`——失效链接/家庭已满/已在家庭/移除冷却——即 `clearPendingLinkId()`，终结失效链接的循环跳转；网络/5xx 保留链接；加入成功 toast 为知情文案「已加入，历史日记将共享给家庭」，见 02d §7）；否则 `GET /family` 渲染成员；成员非空且无 `inviteLinkId` 时 `POST /family/invite-link` 生成链接。
- `onShareAppMessage`：有 linkId → `/pages/Family/Family?linkId=<id>`；无则回 `/pages/index/index`。
- 退出/移除：本人 `POST /family/leave`，他人 `DELETE /family/members/{userId}`，均需 `ConfirmDialog` 二次确认（danger）。
- Invite `onShow`：登录 → `GET /invite/list`（取前 50）→ `POST /family/invite-link`；`POST /invite/qrcode` 预生成邀请图（响应 `thumbUrl` 为服务端确定性 ≤60KB 分享缩略图，旧后端无此字段时回退海报原图，供 App 分享缩略图使用）。
- Invite `onShareAppMessage`：默认分享路径 `/pages/index/index?inviter=<userId>`；家庭分享（`data-share-type="family"`）路径 `/pages/Family/Family?linkId=`。
- 保存邀请图：下载 → 小程序端 `wx.showShareImageMenu`，带 `entrancePath=/pages/index/index?inviter=<userId>`；多端 App 端（与 NoteDetail 分享同型）场景选择 → `wx.miniapp.shareImageMessage`；有缓存路径；保存中防重入。

### 3.5 用户 `User` / 设置 `Set` / 常用地址 `CommonAddresses`

- User `onShow`：`request.login()` → 刷新 `formatVipInfo(getVipInfo())`；入口清单见第 1 节；私有模式隐藏「我的邀请」。
- Set `onShow` 并行 `getFamilyConfig()`（`GET /auto-record/config`）、`GET /auth/phone`、`GET /user/profile`。
- 昵称：`PUT /user/nickname`，前端限 20 字（后端校验在 `validator.ValidateNickname`）；请求体字段名与后端绑定 tag 一致为 `{nickName}`；成功后更新 baseInfo 与 `_needRefreshIndexList`。
- 头像：`onChooseAvatar` → `updateAvatar`（`POST /file/upload?type=avatar` → `PUT /user/avatar {fileId}`）。
- 主题 `onThemeTap`：auto/light/dark（`themeManager`，storage `ppfj_theme_mode`）。
- 语言 `onLanguageTap`：auto/zh/en/zh-Hant（`i18n`，storage `ppfj_lang_mode`），选择后 `PUT /user/lang`。
- 手机号：`getPhoneNumber` → `POST /auth/phone/bind {code}`；已绑定且当天已改 → toast 日限；产品规则「手机号只可更换不可解除」——无解绑入口，后端解绑端点已移除。
- 服务号：未订阅显示二维码弹窗（`getHelpBaseURL()/follow.png`）。
- 桌面快捷：Android 且 `canIUse('addToDesktop')` 直接添加，否则引导；可跳 `wx.openAppAuthorizeSetting`。
- 私有模式（`backend_mode==='private'`）隐藏手机号绑定/更换与服务号入口（服务号行 `Set.wxml` 的 `wx:if="{{!isPrivateBackend}}"`；手机号行叠加 App 环境条件 `wx:if="{{!isPrivateBackend && !isAppEnv}}"`，App 端隐藏见 §3.11 差异表）；头像行不按私有模式隐藏（仅 App 环境隐藏 `open-type=chooseAvatar` 按钮，见 §3.11 多端差异表）。
- CommonAddresses `onShow`：`POST /user/common-addresses/refresh` 取回并按 `count` 降序；点地址弹编辑抽屉，`PUT /user/common-addresses/{oldName} {newName}`；名 >100 字/空拦截。

### 3.6 VIP `pages/sub/Vip/Vip`

| 交互 | 行为 / 接口 |
|------|-------------|
| 商品加载 | `GET /vip`，过滤 `type!=='free' && type!=='trial'`，取第一条价格 `amount/100`，默认选中第一档 |
| 系统配置 | `GET /system/config`；`features.payment/freeVip` 控制入口；私有模式本地兜底关闭。注：本页 `data.backendMode` 承载 `/system/config` 返回的部署形态（`open`/`saas`，`onLoad` 私有兜底置 `open`，`Vip.wxml` 以 `!paymentAvailable && backendMode==='open'` 判定无支付提示），与 `utils/storage.ts getBackendMode()` 的连接模式（`saas`/`private`）同名不同义，行为正确、命名差异为既有 WXML 判定键 |
| 无支付能力 | `features.payment===false` 时展示「当前为私有化部署，会员请联系管理员开通」（i18n `vip.openSourceNoPayment`） |
| 购买 | 未勾选协议先弹协议弹窗；`doPay` → `POST /payment/virtual/request {vipId, env}` → `wx.requestVirtualPayment` |
| env | iOS 恒 0；否则 develop → 1，trial/release → 0（现网实付，02e VP-13-AC1；`utils/pay.ts getPayEnv`） |
| 轮询 | `GET /payment/virtual/status?outTradeNo=` 每 3s、最多 10 次；`paid` → 成功弹窗 + 刷新 VIP；`closed` → 失败提示；超时 → 超时提示；401 会话过期（哨兵判定）→ `loginExpiredTip` 并终止轮询 |
| 免费领取 | `GET /vip/free` → `POST /vip/free/claim {vipId}`；`FREE_VIP_ALREADY_CLAIMED` → 已领取提示 |
| VIP 刷新 | `onShow` 与支付成功均 `fetchVipInfo()`；VIP 信息 60s 内存缓存 + storage |

> 后端实现细节见 `docs/spec/02e-vip-payment.md`（VP-1~VP-13）。前端在支付取消/失败/中止路径 best-effort 调用 `POST /payment/virtual/cancel`（`utils/pay.ts`）。

### 3.7 AI 抽屉 `components/AIDrawer/AIDrawer`

- 打开需登录（`index.openAIDrawer` 先 `ensureLogin`）；`sendPrompt` 输入 ≤2500 字符（与后端 `maxMessageCodePoints` 统一）。
- 请求体仅 `{message, request_id}`（不传 history，上下文由后端组装；本地列表裁剪至 50 条仅为 UI 展示），`eventSource POST {base}/ai/chat`，headers `Authorization`（私有模式加 `X-Private-Api-Key`）。
- 流事件：`data` 增量（120ms 合并 setData）、`notice` 非终止性提示（`type=truncated` → 回文尾部追加 i18n `aiDrawer.truncatedSuffix`「（回答不完整）」，三语资源同步）、`done` 结束（Markdown→HTML、解析卡片日期标记）、`error`（`{"code","biz_code","bizCode","message"}`；`eventSource` 同时读取 `biz_code` 与旧 `bizCode`；`biz_code=AI_DAILY_QUOTA_EXCEEDED` → 配额弹窗引导 Vip，否则 toast）。
- 网络类失败自动重连一次（复用同 `request_id`）；重连前清空半截输出。消息上限 `MAX_MESSAGE_COUNT=50`。
- 防护边界：`eventSource` 读缓冲上限 64KB（超限主动 abort 报错）、整体 200s 超时；AIDrawer 用 `_sendSeq` 发送代次防旧流回调复位发送锁（连点防护），支持 `retryLast` 重发上一条。
- 隐藏/销毁：`hide`/`onPageHide`/`detached` 中止 SSE；切后台不取消登录写操作。`detached` 后的 flush 因 `_isDetached` 已置位而成为 no-op，未落盘的半截文本不保留。
- 日记卡片：`POST /diary/info/dates {dates}` 回填，按 `_msgId` 定位，避免列表裁剪错挂。

### 3.8 记忆 `MemoryEdit` / 封面 `CoverEdit` / 条目 `NoteEdit`

- `NoteEdit`：定位（`wx.getLocation gcj02`，15s 超时；`GET /location/reverse` 逆解析并行 `GET /user/common-addresses`，300m 内命中常用地址则替换名称；手动 `wx.chooseLocation` 不替换）；文本 ≤140 字（前端），图片 ≤9 张、单张 ≤10MB；提交 `POST /diary/details`（新建）/`PUT /diary/details`（`form.id` 存在）；上传走 `POST /file/upload?type=recordImg`（并发 3，压缩 quality 分档见 02g §F-1（数值在 02g §7）；部分失败重试仅补传失败项，成功项回写 file id 不重传）。普通业务重复提交仅由 UI loading 拦截。
- `MemoryEdit`：标题 1~50 字、内容 ≤10000 字、时间必填；新建 `POST /diary/details/memory`、编辑 `PUT /diary/details/memory`。
- `CoverEdit`：从当日图片选中 → `PUT /diary/info {id, coverImage}`；未选图直接关闭；核心业务保留 `_submitting` 入口拦截。
- `RecordItem` / `MemoryItem` / `NoteItem`：左滑 >100px 揭示删除（30px 仅用于滚动抑制与防误触判定）；仅本人内容可左滑（`NoteItem` 需 `isMyDiaryInfo`，`RecordItem` 需 `familyMemberUserId === currentUserId`）；确认后分别 `DELETE /diary/details`、`DELETE /diary/details/memory`、`DELETE /diary/info`；删除成功置 `globalData._needRefreshIndexList=true`。

### 3.9 MCP `pages/sub/Mcp/Mcp`

- `onLoad`/`onShow`（从后台返回且非 loading）：`GET /mcp/key` 渲染 `apiKey/apiUrl/memoryUrl/authUrl/mcpConfig`。
- 生成：`POST /mcp/key`；换发：`POST /mcp/key/rotate`（`ConfirmDialog` 确认）；均为核心业务，`_generating` 入口锁。
- `authUrl` 为空时「连接器」Tab 回退到 `mcp`。
- 复制 API Key / 配置 / 绑定链接走 `wx.setClipboardData`。

### 3.10 其他页面

- `Guide`：4 张 OSS 教程图（按语言后缀 `_en`），`next` 到第 4 步写 `setNeedShowXPa(false)` 并 `redirectTo` 首页；入口另有 `pages/User/User.ts` 的 `toGuidePage`（`redirectTo`）。
- `About`：`loadAccountName` 取 baseInfo.nickName；注销需在弹窗内输入与昵称完全一致的文本（`canConfirmDelete`），确认后先 `closeAutoRecord()`（末批驻留点上报依赖有效会话）→ `DELETE /auth/account {confirmName}` → 成功 `resetVipCache()` + 清本地 + 置 `logged_out` 停留标记 + reLaunch 登录页（不回首页——防注销场景静默重登重建幽灵账号，见 02a §7）。本地清理不因页面隐藏/销毁跳过，仅成功 toast 受可见性约束。页内另有「更多产品」姊妹小程序入口（`toFamilyApp`：`wx.navigateToMiniProgram` 跳转爬爬家庭助手 `wx66f181d33f61a691`，用户取消静默；Set 页「更多产品」行同款入口）。
- `WebPage`：仅允许 `https://` 且 host 命中 `papafeiji.cn` / `xiaohongshu.com`（含子域）；非法 toast 并返回。
- `UsageGuide`：`openUrl` 打开 `/tutorial/tutorial0..7/`。
- `BackendConfig`：见第 2.4 节。

## 3.11 多端应用（Donut App）差异适配

官方「需适配的组件汇总」：多端 App 内 `button` 支持但 **open-type 涉及的微信开放能力均不支持**。现行适配（`globalData.isAppEnv` 判定，app.ts onLaunch 注入）：

| 能力 | 小程序端 | 多端 App 端 |
|---|---|---|
| 登录 | 静默 `wx.login` → `/auth/login` | `wx.weixinAppLogin` → `/auth/login/app`（donut/code2verifyinfo，无中间页） |
| 修改头像（open-type=chooseAvatar） | 支持 | **隐藏入口**——头像由微信登录资料带出（code2verifyinfo headimgurl）与默认 marker 并发写入、无先后协调（最终头像取决于完成顺序，权威口径 02a A-9） |
| 绑定/更换手机号（open-type=getPhoneNumber） | 支持 | **隐藏入口**——官方替代为本机号码一键登录/短信验证码组件（身份服务手机号体系，待接入） |
| 解绑手机号 | 已移除入口 | 同左（产品规则：手机号不可解除，只可更换；后端解绑端点已移除） |
| 转发（open-type=share） | 支持 | `wx.miniapp.shareMiniProgramMessage`（Family 邀请卡/Invite 两种卡片，参数与 onShareAppMessage 同构） |

已知其余差异（待验证/待适配）：`type="nickname"` 输入在 App 端退化为普通输入；`show-menu-by-longpress` 不支持；web-view 需 jssdk ≥1.6.2。定位与虚拟支付的 App 端适配已落地：`utils/appPermission.ts` 兼容层替代不可用的 `wx.getSetting/openSetting/authorize`——`getAppAuthorizeSetting` 查态**仅作 `location_diag` 诊断日志、不做流程门卫**（前置查态在真机上会导致链路悬挂），流程门卫是前台 `getLocation` 触发系统授权弹窗，`openAppAuthorizeSetting` 引导系统设置用于 NoteEdit 定位请求与 Set 页；`NoteEdit` 定位请求、确认弹窗与自动记录开启流程均按 App/小程序双轨分发；支付跳转小程序（02e VP-13-AC6）。

**App 构建配置**（`project.miniapp.json`，version/versionCode 以文件为准；`i18nFilePath: "i18n"` 指向 `frontend/miniapp/i18n/` 的 App 外壳层多语言资源）：`qmapAPIKey`（腾讯位置服务 Key）+ `usePluginSdk.map`（地图插件）+ `useExtendedSdk.lbs`（定位 JSAPI）供 `chooseLocation`/定位；`mini-android.permissions` 在清单基础上补 `ACCESS_BACKGROUND_LOCATION`（后台自动记录），`mini-android.privateDescriptions` 声明六项运行时权限用途（打进 `assets/miniapp-permission.json`）；`mini-android.privacy` 指向根目录 `miniapp-privacy.json` 隐私政策提示框模板（未确认前定位等涉个人信息 JSAPI 被腾讯 SDK 合规前置拦截）；android/ios 段 `enableVConsole: "close"`（正式包关闭诊断控制台）。

## 4. 全局交互规则（加载 / 错误 / Toast / 四态）

### 4.1 加载（Loading）

- `utils/http.ts` 引用计数式 `_showLoading`/`_hideLoading`（计数归零才 `wx.hideLoading`）；`resetLoading()` 直接清零并隐藏。
- 页面 `onHide`/`onUnload`/`detached` 普遍调用 `resetLoading()`，避免系统调用触发 hide 后 loading 卡死。
- 图片上传/头像更新内部 `wx.showLoading({mask:true})`；列表页首屏多用 `wx.showLoading`。

### 4.2 错误与 Toast（`utils/http.ts`）

| 场景 | 行为 |
|------|------|
| HTTP 401 | `clearSessionId()` + toast `error.sessionExpired`（2s）；`skipAuthExpire=true` 时跳过（后台轮询/自动记录） |
| HTTP 5xx | http 层构造本地化 Error（message 带 `error.serverError` 文案与 statusCode）交调用方呈现——页面回调多用自身 fallback 文案，`closeTheErrorMessage` 的静默后台请求不提示（401 的「会话已过期」toast 同样被其抑制，与 `skipAuthExpire` 并列为两个抑制例外）；私有模式下 Worker `code 5020/5030` → `error.privateBackendUnreachable` |
| 业务 `code !== '0000'` | `_handleResponseError` toast（2s）：优先按 `biz_code` 查 `utils/errorMessages.ts` 映射显示三语本地化文案（`error.biz*` 键，与 `pkg/errors/codes.go` 枚举同源），未登记码回退后端 `message/msg`；私有模式 `4031` → `error.privateBackendNotRegistered`。**例外**：`closeTheErrorMessage=true` 的后台调用（如 `fetchVipInfo` 的 `/vip/free/check`、自动记录轮询）不 toast、静默降级由调用方兜底 |
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


## App 端 API 适配与合规清单

多端 App 与小程序共码，官方「需适配的 API 汇总」所列差异点在本项目的落地口径（全部按官方替代方案实现，无自造路径）：

| 官方适配项 | 本项目落地 | 判定 |
|---|---|---|
| `wx.login`（App 端不适用） | 显式环境分支：App 走 `wx.weixinAppLogin` → 服务端 `code2Verifyinfo`；小程序走 `wx.login` → `code2Session`（`pages/Login/Login.ts` 双轨互斥） | ✅ |
| `wx.showShareXXX` → `wx.miniapp.shareXXXX` | 分享图片：App `wx.miniapp.shareImageMessage`（服务端缩略图 thumbUrl）；分享小程序卡片：App `wx.miniapp.shareMiniProgramMessage`；小程序分支照旧 | ✅ |
| App 支付（`wx.requestPayment` → `wx.miniapp.requestPayment`） | 虚拟支付依赖小程序 session_key（App 端瞬拒）→ App 端 `wx.miniapp.launchMiniProgram` 跳小程序支付（官方新 API 的合规变通）；小程序端 `wx.requestVirtualPayment` 照旧 | ✅（合规变通；官方 App 原生支付为可选演进） |
| `wx.requestSubscribeMessage`（App 端无法支持） | 异常提醒订阅裁撤 App 通道：仅小程序弹订阅授权，App 环境不弹不记 | ✅（App 无通知） |
| 权限三件套 `getSetting`/`openSetting`/`authorize`（App 端不可用） | `utils/appPermission.ts` 替换实现：`getAppAuthorizeSetting` 查态 + `openAppAuthorizeSetting` 引导系统设置 | ✅ |
| 系统/设备信息（参数调整，无需替换） | 新拆分 API（`getWindowInfo`/`getDeviceInfo`/`getAppBaseInfo`）为主，`getSystemInfoSync` 仅低版本回退（`utils/util.ts`） | ✅ |
| `getMenuButtonBoundingClientRect` | 直接使用（官方明确无需替换，返回值兼容） | ✅ |
| canvas（Android 需 XWEB SDK） | 分享图服务端化（`POST /diary/share-card`），客户端 canvas 全量退役（XWEB 在基座中已无消费方）；`useExtendedSdk.xweb`/`xwebEmbed` 暂保留 true——匹配在用缓存基座，云构建额度恢复前本地出包不被基座重建阻塞，待基建包重建窗口摘除（与 RECORD_AUDIO uselessPermissions 同批） | ✅ |
| web-view | `pages/sub/WebPage` 经 `utils/util.ts openUrl` 动态导航（About/UsageGuide 的教程/协议/小红书链接在用）；web-view 是基础组件、不依赖 XWEB 扩展 SDK | ✅ 无影响 |
| 定位族（LBS SDK + qmapAPIKey + 权限描述 + 隐私模板） | 全部已配（`chooseLocation` 鸿蒙不支持=平台限制，无鸿蒙包） | ✅ |

已接受取舍（App 端）：订阅下发通道未实现（客户端授权记录成功；服务端接口待接入，见 01 §6.2）；`RECORD_AUDIO` 权限暂保留（代码零使用，随基建包重建经 `uselessPermissions` 摘除，见 01 §6.2）。
