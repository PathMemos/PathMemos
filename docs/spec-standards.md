# 文档规范（spec 体系）

> 仓库根目录 `AGENTS.md` 是第一入口；本文件是文档体系（分层/写作约定/一致性门禁）的细则。
> 核心纪律：**行为变更必须同步文档**，`scripts/spec-check.sh` 在 CI 阻断未同步的提交。

## 一、项目文件映射

| 层 | 文件 | 说明 |
|---|---|---|
| L1 PRD/总览 | `docs/spec/01-product-overview.md` | 业务目标 / 定位 / 功能范围 / 平台边界与已知待办 / 验收与测试覆盖 |
| L2 领域分册 | `docs/spec/02a-auth-account.md`、`02b-diary.md`、`02c-autorecord.md`、`02d-family-invite.md`、`02e-vip-payment.md`、`02f-ai-chat.md`、`02g-file-push.md`、`02h-mcp-open.md` | 每分册自足（背景/决策/流程/数据/契约） |
| L3 接口契约 | `docs/spec/03-api.md` | 认证与请求头/统一响应/错误码词汇表/分页/限流/幂等 + 全量端点登记 |
| L4 数据库 Schema | `docs/spec/04-database.md` | ER/字段级表定义/数据字典（枚举、状态机）/ 迁移变更记录 |
| L5 开发与协作约定 | `docs/spec/05-development-plan.md` | 开发约定/外部依赖/部署契约指针/本地门禁 |
| L6 前端/交互 | `docs/spec/06-miniapp-pages.md` | 小程序页面清单（路由/优先级）/关键交互/全局规则（弹窗/表单/反馈/四态）/App 端适配 |
| L7 验收流程 | `docs/spec/07-acceptance-flows.md` | 人工验收清单（场景/期望/异常核对）+ 覆盖登记表（需求编号 → 场景 / 不需场景的理由） |
| L8 能力索引 | `docs/CAPABILITIES.md` | 全平台能力实时清单（按域分组），新增/变更能力时同步 |
| L9 决策记录 | `docs/decisions/` | ADR：只记「为什么这么做」，索引双向同步 |

判断规则：

- **改现有能力** → 回写对应层文件 + 更新 `CAPABILITIES.md`。
- **新增大模块** → 新建 L2 分册（套 §三 模板）+ 能力索引加一节。
- **bug 修复 / 纯重构** → commit message 写 `spec:nochange` 豁免，不改 spec。
- **文档移动/改名/删除** → 同一 commit 内更新全部引用。
- **一次性运维任务**（如服务器迁移、数据回捞）**不占 spec 层**，执行计划单独维护，不入库 `docs/`。
- **仓库根 `AGENTS.md`** 为运维/流程手册（部署命令、APK 构建、回调补发等操作性内容），随代码评审同步、显式豁免 spec-check 制品清单——其行为契约由对应 spec 分册承载，AGENTS 承载流程与操作，机械校验不可行。

## 二、需求表达与编号约定

- 需求单元使用稳定编号（如 `D-1`、`VP-13`、`A-1`），验收标准以 `AC` 后缀引用（如 `VP-13-AC6`）；这些编号被 L7 覆盖登记表与跨文档引用网使用，**只增不改号**。
- 验收标准必须可人工核对（可观察的期望结果），后端行为类标准还需能被 Go 单测断言。

## 三、领域分册模板（新建大模块时套用）

```
## 1. 背景与目标
## 2. 领域级架构决策
## 3. 核心流程（用户故事 + 时序）
## 4. 数据模型
## 5. API 契约
## 6. 关键实现约束
## 7. 前端接入
## 8. 运维与任务
```

> 以上为**现存分册的实际结构**（02a~02h 一致），新建分册沿用此结构。

## 四、DoD（完成定义）

> **验收策略**：自动化测试仅保留**后端 Go 测试**（`make test`）作为门禁；小程序端到端与 Worker 行为以**人工验收**为准（L7 场景）。存量前端 Jest（套件/用例数以 `frontend/miniapp/test/` 实际为准）与 Worker vitest 保留为资产但**不作为验收门禁**，不要求随新功能新增用例。

1. 代码实现 spec 能力，通过门禁（见 §八）在本机验证。
2. 规格已同步：新增写入/变更更新对应层文件（§一 映射）。
3. 后端核心业务逻辑、关键状态机、关键数据访问**必须有 Go 单测**；DTO、薄封装、纯配置等按实际情况决定，不为覆盖率而覆盖。前端/Worker 无自动化测试要求。
4. API 变更同步 L3 契约文档；migration 配对 `.down.sql`；改 SQL 后跑 `make sqlc-generate` + `make check-sqlc-sync`。
5. 新建公共抽象前查复用情况 + ADR，并同步登记。
6. 核心业务链路同步登记/更新 L7 人工验收场景（纯局部改动豁免）。
7. 能力索引（L8）同步更新。
8. **破坏性后端变更（字段删除/语义变更/错误码迁移等）须在分支说明中声明对 N-1 前端的兼容性**——半成功发布形态（新后端 + 旧前端/旧入口）的可用性以此为前提（DEPLOYMENT §4）；无声明视为兼容。

## 五、数据模型变更流程

1. **分配迁移号**：取 `backend/migrations/` 当前最大 +1，命名 `NNNNNN_<slug>.up.sql` / `.down.sql`（6 位编号，如 `000001_baseline`）。
2. **down 可逆性**：不可逆操作在文件头声明 `-- 不可逆：<原因>`。
3. **同步 schema 文档**：同次提交回写 L4 文档的变更记录 + 对应小节字段定义。
4. **机械校验**：`spec-check.sh` 比对 migrations 编号与 L4 文档登记。

> 并行撞号规则：同号不同名允许（slug 全局唯一），改同一张表需 rebase 确认顺序。

## 六、一致性红线

- **新增能力**：先（或同时）写 spec，再写代码；禁止「只写代码不改 spec」。
- **行为变更**：改接口/状态机/错误码/权限必须同步 spec。
- **删除能力**：代码删除时同步删除 spec 描述。
- **发现不一致**：先报告再改，不擅自判定谁对谁错。
- **引用方式**：跨文档引用代码优先「文件 + 函数/符号」（如 `auth/handler.go` `Logout`）；纯行号漂移（符号/语义未变）不要求单独修订文档。
- **待办生命周期**：`01-product-overview.md` §6.2「已知待办与路线」是 docs 唯一许可的过程态区域——每项必须带触发条件/前置或明确「不做」结论；完成即删；禁止「下次批次」式无限期挂起；其余文档不得挂待办。

## 七、关键纪律

1. **验收标准可人工核对**（可观察期望结果），后端行为类标准能被 Go 单测断言。
2. **行为契约 vs 实现锚点**：阈值语义、幂等、数据归属、状态机等**行为契约**的变更必须先改/同步对应 spec；文档中的具体键名、TTL 数值、文件路径、函数参数等是「**当前实现参考**」，代码演进时同步更新即可，不构成不可变约束。文档以所在章节定性质：决策表（D-x）与不变量为约束，实现位置与键值清单为参考。
3. **改共享事实数值须全库 grep 旧值**：阈值/参数类变更（如压缩 quality 调参）极易在多份复述文档中留下旧值——提交前必须以旧值为关键字全库检索（含 06/07/BUSINESS-FLOWS/ARCHITECTURE），逐处更新。
4. **共享事实权威源映射**：同一事实多处出现时以权威源为准，其余位置一律引用不复述数值。当前映射：

   | 共享事实 | 权威源 |
   |---|---|
   | 图片上传/压缩阈值（大小/张数/quality 分档） | 02g §F-1 阈值表（quality 分档数值在 02g §7 http.ts 行） |
   | API 限流值（后端各路由） | 03-api §限流表 |
   | Worker 限流值（bind/mcp 主链路） | 02h §6.1 |
   | 后台任务清单与间隔 | 02c §8 |
   | 数据保留天数 | 04-database 各表小节 |
   | Session TTL | 02a §6.1 |
   | HTTP Server 超时 | 03-api §6.1 服务器超时 |
   | 部署命令与契约 | DEPLOYMENT §1/§4 |
   | 邀请奖励数值 | 02a A-6 |
   | AI 日配额（非 VIP 10 / VIP 100） | 02f（§1 / AI-4） |
   | payment 告警关键字 | `scripts/alert-watch.sh` 的 `KEYWORDS` 清单（文档一律引用不复述；DEPLOYMENT §10 保留通配符类别索引与排障注释——显式豁免，禁逐条枚举全集） |
   | 公众号回调链路（回调处理 / 客服接口分段） | 02f AI-3 |

   L7 验收清单可保留具体数值作为核对基准（人工验收需可数字面核对），但变更时受「全库 grep 旧值」规则约束。

## 八、校验分工（`scripts/spec-check.sh` 门禁）

| 级别 | 项 | 内容 |
|---|---|---|
| 阻断 | 1 | spec 制品齐备（spec-standards + L1~L7 分册 + 能力索引存在且非空） |
| 阻断 | 2 | migration 配对（up/down 成对） |
| 阻断 | 3 | ADR 索引双向（文件 ↔ `decisions/README.md`） |
| 阻断 | 4 | migration 编号 ↔ L4 文档变更记录（migration→文档 阻断；L4 孤儿登记为提示） |
| 阻断 | 5 | 安全红线（`.env` 不入库、无私钥块；上传白名单存在且不含危险类型） |
| 阻断 | 6 | spec 随代码变更：改了 `backend/`、`api-worker/`、`mcp-worker/` 而 spec/能力索引无同次变更，且 commit 无 `spec:nochange` → 失败（`--warn` 降级为提示；无 git 基线时跳过） |
| 提示 | 7 | 路由 ↔ L3 契约覆盖（`rpc.go` 路由内联排除） |
| 提示 | 8 | 错误码词汇表 ↔ `backend/pkg/errors` 常量一致性（以代码常量为准、spec 词汇表为登记——与 ADR-0008「先改 codes.go 并回写本表」的代码先行方向一致） |
| 提示 | 9 | 新增后端实现文件所在包带同包测试（同目录存在 `*_test.go`） |
| 提示 | 10 | down 不可逆/破坏性操作声明 |
| 提示 | 11 | 能力索引反向差异（分册 ↔ 索引双向，按分册号粒度） |
| 提示 | 12 | spec 内 file:line 引用在行数范围内（SR-12） |
| 阻断 | 13 | docs 无过程产物/评审残留（R-xx、plans/、第三步整改、XX-Pn-nn 形态字样；spec-standards.md 自身豁免；SR-13） |
| 提示 | SR-11 | MCP 静态方法双源对账：`mcp-worker/src/lib.ts` 与 `backend/internal/mcp/server.go` 的 tools/prompts 名称+描述与 prompt 正文首行一致（`scripts/check_mcp_static_sync.py`） |
| 提示 | SR-14 | 迁移 up 脚本含 contract DDL（`DROP COLUMN`/`DROP TABLE`/`ALTER COLUMN ... TYPE`/`SET|DROP NOT NULL`）时提示分步发布要求（04 §8：部署失败窗口的旧代码兼容性依赖 expand-only 前提，contract 类须待旧代码下线后的后续部署） |

| 类型 | 手段 | 负责 |
|---|---|---|
| 硬约束（阻断） | `scripts/spec-check.sh` | 上表阻断级 |
| 温和提示 | `scripts/spec-check.sh` | 上表提示级 |
| 语言级门禁 | `make lint-go` / `make lint-frontend` / `make lint-worker` / `make check-sqlc-sync` | 本地全跑；CI 三个 job：backend（golangci-lint + `go test -race` + check-sqlc-sync + MCP 双源对账 + 连接预算脚本测试 + `spec-check.sh`）、frontend（`npm run lint`）、worker（typecheck） |
| 语义一致性 | 代码评审 | spec↔代码是否对齐 |
| 业务链路 | L7 flow（人工验收） | 端到端验证 |

> 工具项口径与本文档如有出入：属「发现不一致」，按 §六 先报告再改，不擅自改。
