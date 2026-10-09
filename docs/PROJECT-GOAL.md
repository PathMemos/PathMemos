# 项目总目标

> 本文档是项目的总目标声明：以什么标准交付、哪些关键决策已锁定、代码与文档如何对齐。它是后续一切开发的起点性文件；内容与各分册冲突时以对应分册为权威源。

## 一、总目标

以「**简单优先、文档与代码双向忠实**」为交付标准：每个业务行为都有 spec 分册承载（行为契约），每条文档断言都与代码实现一致（`scripts/spec-check.sh` 门禁与文档-代码一致性核对保障），每个风险都有显式取舍登记（ADR / ARCHITECTURE-INVARIANTS §9「已接受风险与不改清单」）——不留无记载的隐患，也不为小概率场景引入过度防御。

## 二、质量价值观（细则见 AGENTS.md §二，此处为纲领）

1. 小概率异常容忍而非防御，接受重启恢复；锁只给核心业务。
2. 安全只排高危：未鉴权接口、敏感泄露（含服务端出网请求的 SSRF 面、家庭位置数据的敌意重入面）。
3. 性能与稳定性优先；核心接口流畅，非核心允许等待。
4. 改变任何「已接受取舍」须新增 ADR；发现 spec 与代码冲突按 AGENTS.md §五裁决。

## 三、关键决策

| 决策 | 落点 |
|------|------|
| 家庭移除冷却：被 owner 移出成员 7 天内禁止经邀请链接重入（直接加入与整家合并路径均检查；再移除覆写时间戳重新起算）——收敛「移除-循环重入」对位置轨迹共享的敌意面 | [ADR-0019](decisions/0019-family-removed-rejoin-cooldown.md)、02d D10/F-4/F-7、07 F5 |
| 邀请加入的知情语义：加入成功 toast 为「已加入，历史日记将共享给家庭」（不加确认/预览，ADR-0017 立场不变，仅补知情） | 02d §7、ADR-0017 |
| 后台自动成文与手动成文统一失效 MCP 用户缓存（两路径可见性一致，延迟上限=边缘 TTL 1/5 分钟）；新增数据写路径必须调用失效钩子（纪律入 02h §6.2） | 02c AR-8.4、02h §4.3/§6.2 |
| mcp-worker `/health` 按 IP 限流 60/min（防零成本打满 Worker 配额殃及主链路；「不校验方法+回源探测」保留） | 02h §5.2 |
| OSS 配置部署侧 fail-fast：五项全部未配置直接报错退出（无 OSS 的 SaaS 部署在健康门禁必然失败，预检前移发现点）；后端 validate 口径为「任一非空要求四项齐备」 | DEPLOYMENT §2.11/§4 |
| 手机号解绑端点移除（产品规则只可更换不可解除；无调用方的可直调解绑面无保留价值） | 02a A-4、03-api §8.2 |
| spec-check 新增 SR-14：迁移含 contract DDL 时提示分步发布要求（04 §8 硬纪律的机械提示） | spec-standards §八、scripts/spec-check.sh |
| 权威源复述收敛：后台任务表唯一权威源 02c §8（ARCHITECTURE/03-api 改引用）；限流值 02a 引用 03-api；purge 机制登记收敛 ADR-0013 单点 | spec-standards §七、CAPABILITIES |
| OSS 用户图片 bucket 一律开启版本化（deploy.sh 幂等开启，含存量）；跨区复制/清单备份不做 | [ADR-0018](decisions/0018-oss-versioning-data-retention.md)、DEPLOYMENT §5 |
| share-card 服务端抓图 SSRF：黑名单逐 IP 拒绝 + 重定向逐跳同语义校验（上限 3 跳）；DNS rebinding TOCTOU 不设防（登记于 02b §5） | 02b §5 |
| `--miniapp-only` 合入 main 前四路径 merge-base 护栏（backend/deploy/mcp-worker/api-worker 无差异才自动合入） | DEPLOYMENT §4 |
| 公众号回调 AES Key 为 SaaS 必配（强制安全模式）；明文形态仅限开源极简部署 | 02g P-1、DEPLOYMENT §2.4 |
| 回滚安全硬纪律：contract DDL 延迟到旧代码彻底下线后的后续部署（两步序列） | 04 §8 |
| advisory lock 两类边界：会话级任务锁（try-lock fail-fast）与事务级串行化锁（阻塞等待、不占独立连接） | ADR-0005、DEPLOYMENT §1.2 |
| 告警关键字全集权威源为 `alert-watch.sh` KEYWORDS | spec-standards §七.4 |
| 破坏性演练年度一次，结果写入当次发布说明 | ARCHITECTURE-INVARIANTS §6、07 §1.1 |
| AI 单轮输出成本敞口有界（≈上游速率×180s×日配额，量级小），不设代码默认 `max_tokens`（防截断推理模型长回答） | ARCHITECTURE-INVARIANTS §9 |
| restore 恢复全程持共享 watchdog flock | DEPLOYMENT §5 |
| 「退出即收回」仅对聚合视图成立；图片 URL 级访问不随退出失效 | ADR-0017 |
| 01 §6.2 待办触发条件全部可判定（时间/版本号/部署前置） | 01 §6.2 |

以上决策均已实施于当前代码（含 000014 移除冷却迁移、autorecord 的 MCP 失效、mcp-worker 的 /health 限流、deploy.sh 的 OSS fail-fast、auth 的 unbind 端点删除、spec-check 的 SR-14）。

## 四、文档与代码的对齐机制

- 行为契约入 spec（spec-standards §七），数值有权威源映射（引用不复述），提交门禁 `scripts/spec-check.sh` + CI 四 job 拦截漂移。
- docs 只保留当前终态制品：不写过程、审计历史或待确认项；过程性结论一律沉淀为 ADR 的「决策 + 后果」。
