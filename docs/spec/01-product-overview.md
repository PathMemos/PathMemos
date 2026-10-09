# PP-01 产品总览（L1）

> 层级：L1 PRD/总览｜版本：当前（以代码为唯一事实源）
> 上游：无｜关联 ADR：ADR-0003、ADR-0004

## 1. 定位与用户

papafeiji（拍拍照记记）是一款以「时间轴 + 地图轨迹」自动记录生活、并可用 AI 对话检索/回忆的微信生态应用。

| 终端 | 用户 | 交互 |
|------|------|------|
| 微信小程序 | 主用户 | 手动/自动记录日记、家庭共享、VIP、AI 对话 |
| 微信公众号/服务号 | 订阅用户 | 纯 AI 对话 |
| MCP / 开放 API | 第三方 AI 工具（Cursor/Kimi 等） | 日记与回忆读写 |
| 多端应用（Android APK） | 小程序同主体用户 | 与小程序同构（登录经 Donut 微信授权，unionid 归一）；测试包经官网分发 |
| 多端应用（iOS） | — | **暂不交付**：无构建/签名/分发链路；代码保留 `mini-ios` 段与 iOS 行为差异描述（如 iOS vConsole）以备未来启用 |

两种形态：**SaaS 版**（官方托管，默认直连 `pro.papafeiji.cn`）与**开源版**（自部署：可直连自建后端，公网暴露可经 cloudflared tunnel 或 caddy；亦可经 api-worker 路由）。同一份小程序代码，接口不存在时优雅降级。

## 2. 业务目标

> 本节回答「为什么做、做成什么样」。

| 目标 | 对应能力 | 验收 flow |
|------|---------|-----------|
| O1 记录留存 | 手动记录、自动记录、家庭共享 | F2 / F3 |
| O2 商业变现 | VIP、虚拟支付、邀请 | F6 |
| O3 AI 使用率 | AI 对话、公众号入口 | F7 |
| O4 开放生态 | MCP / API Key | F8 |

## 3. 功能范围

- 时间轴日记（文字/图片/位置），手动与自动记录。
- 家庭共享与邀请，成员可查看同一份记录。
- VIP 权益与微信虚拟支付。
- AI 对话（小程序 SSE + 公众号），日记/回忆检索。
- MCP/API Key 开放接口。
- 推送：模板消息 / 服务号客服消息。

## 4. 非功能需求

| 维度 | 要求 |
|------|------|
| 性能（观测项，非验收项） | 期望核心接口 P95 < 500ms、AI 首字节尽快；当前无指标采集基建，**不作验收断言**（观测口径见 DEPLOYMENT §10）；SSE 长连接为行为要求 |
| 安全 | 鉴权接口全部过 Session/OpenAuth；密钥不落仓库；上传类型/大小白名单；错误响应词汇统一（ADR-0008）；`WORKER_SECRET` 生产必填 |
| 兼容 | 小程序基础库低版本守卫；SaaS 与开源版共用前端，接口缺失优雅降级 |
| 可用 | 单机容器化；Redis 故障分层口径——健康探针不判死（`/health/ready` 200 degraded），服务面鉴权 fail-closed（受保护接口 500，见 INVARIANTS I2）；数据库故障经 `/health/ready` 503 + watchdog 自愈，告警日志关键字见 DEPLOYMENT §10 |
| 可维护 | 文档与代码同步（`scripts/spec-check.sh` 门禁）；migration 配对；sqlc 同步 |

## 5. 验收与测试覆盖

| 项 | 现状 |
|----|------|
| 端到端验收（L7） | F1–F13 **人工验收清单**（`docs/spec/07-acceptance-flows.md`）；无自动化 flow 执行器 |
| 自动化测试门禁 | 仅后端 Go `go test -race`；存量前端 Jest（以 `frontend/miniapp/test/` 实际为准，当前 10 套件 / 57 用例）与 Worker vitest（40+21）保留为资产、**非门禁**；`spec-check` |
| 人工验收 | 小程序页面交互、公众号链路、Worker 路由按 L7 场景人工执行 |

## 6. 平台边界与已知待办

### 6.1 平台边界（现行约束）

- 多端应用 iOS 暂不交付（见 §1）。
- `wx.requestSubscribeMessage` 为 App 端「无法支持」API → 异常提醒订阅已裁撤 App 通道：仅小程序弹订阅授权，App 环境不弹不记。
- 虚拟支付依赖小程序 session_key（App 端瞬拒）→ App 端跳转小程序支付（见 02e）。
- `chooseLocation` 鸿蒙不支持（无鸿蒙包）。

### 6.2 已知待办与路线

> 本节是 docs 唯一许可的待办区（生命周期规则见 spec-standards §六）：每项必须带触发条件/前置或明确「不做」结论，完成即删。

| 项 | 说明 |
|----|------|
| 技术债（已评估暂缓） | `GET /auto-record/config` 读用户 DB 失败降级 200 `{enabled:false}`（PUT 失败为 500）的语义再评估。**触发**：出现因 200 降级导致前端误判开关状态或排障困扰的实际案例；无案例维持现状（02c §5 已登记该降级行为） |
| sprintf-js ReDoS（无上游补丁） | jest/eslint 工具链传递依赖（devDeps，无运行时攻击面）；上游未发布补丁版本。**触发**：上游发布修复版时随工具链升级 |
| 旧版 bizCode 兼容移除 | `backend/internal/ai/upstream.go` 同时下发 snake/camelCase bizCode；移除 camel 分支。**触发**：2027-01-01 复查——届时线上最低 versionCode 高于下发 camelCase 的版本引入号即移除，否则顺延一年再复查 |
| 腾讯地图 Key 配额 | `chooseLocation` 依赖的 explore 接口日配额紧张，需在腾讯位置服务控制台提额或新建专用 Key。**触发**：explore 接口报 121（当日额度耗尽）时提额或新建专用 Key |
