# 决策记录（ADR）索引

> ADR 只记「为什么这么做」，不写操作步骤、不写代码细节。每条是不可变的决策快照。
> 规范见 [`spec-standards.md`](../spec-standards.md) §二 L9；模板见 [`0000-template.md`](0000-template.md)。

## 何时写 ADR

1. 有多种合理方案的取舍（选了 A 没选 B，后人会问为什么）。
2. 代码看起来冗余/过度设计但有意为之。
3. 安全/一致性机制的存在理由无法从代码体现。
4. 被反复问起、反复想改的点。

不写：纯 bug 修复、一眼能懂的实现、事后找补的长叙事。

## 规则

- 命名 `NNNN-<slug>.md`，四位递增，小写连字符；序号可有意留空洞。
- 状态四态：`提议` / `已接受` / `已取代` / `已废弃`。
- 变更形态两可：默认「新增一条 + 旧条标记 `已取代`、新条写 `取代：<旧序号>`」；小幅修正优先直接改写正文并保持结论稳定。
- 本索引与 `decisions/` 目录必须双向同步（`spec-check.sh` 阻断级）。

## 索引

| 序号 | 标题 | 状态 | 一句话结论 |
|------|------|------|-----------|
| [0000](0000-template.md) | 模板 | — | 复制此文件起草新决策 |
| [0001](0001-mcp-cloudflare-worker.md) | MCP 协议层剥离到 Cloudflare Worker | 已接受 | 源站只留 `/internal/mcp/*`，公网入口统一 Worker |
| [0002](0002-oss-image-storage.md) | 图片存储使用阿里云 OSS | 已接受 | 后端保留本地存储兜底，OSS 为生产默认 |
| [0003](0003-multi-agent-worktree.md) | 多 Agent 用 git worktree + deploy.sh 隔离 | 已接受 | 开发隔离、部署互斥、健康检查后自动合入 main |
| [0004](0004-app-sse-dual-container.md) | app 与 sse 双容器共享同一镜像 + Redis | 已接受 | 多进程不共享内存，跨进程共享 Redis |
| [0005](0005-distributed-locks-postgresql.md) | 分布式锁统一使用 PostgreSQL advisory lock | 已接受 | Redis 锁退役；锁仅为并发优化，正确性由 DB 兜底 |
| [0006](0006-drop-sys-configs-env-config.md) | 删除 sys_configs，配置全部环境变量 | 已接受 | 配置唯一来源为环境变量/代码默认值 |
| [0007](0007-api-key-plaintext.md) | MCP API Key 明文存储与永久展示 | 已接受 | 明文供展示、SHA-256 供认证，泄露风险已接受并缓解 |
| [0008](0008-unified-error-code.md) | 错误响应词汇统一 | 已接受 | code 固定枚举，语义码一律 biz_code，同码同 HTTP |
| [0009](0009-open-source-embedded-wechat-secret.md) | 开源版内置加密微信 AppID/Secret | 已接受 | 轻量混淆换取开箱即用 |
| [0010](0010-open-family-invite-link.md) | 家庭邀请链接长期有效且无撤销 | 已接受 | 小规模家庭场景下成本收益不匹配，维持现状 |
| [0011](0011-payment-callback-amount-policy.md) | 支付回调安全模型与金额策略 | 已接受 | 金额不作为漏发闸门（>0 即发货+告警）；无自动对账；仅开发版走沙箱（PAYMENT_ALLOW_SANDBOX 闸门），体验版与正式版现网实付 |
| [0012](0012-family-invite-owner-merge-risk.md) | 家庭邀请 owner 整家合并钓鱼风险 | 已接受 | 熟人小家庭场景下接受该风险，不增加防护 |
| [0013](0013-deleted-image-cache-purge.md) | 已删除图片边缘缓存定期批量收敛 | 已接受 | 删除图片须最终不可达；定期批量 purge，不做同步清理 |
| [0014](0014-pinned-deps-manual-upgrades.md) | 依赖固定版本、关闭自动依赖升级 | 已接受 | 不启用自动依赖更新；锁定版本，按需成套升级 |
| [0015](0015-reliability-baseline-alerting-zerodowntime.md) | 运维可靠性基线：告警随部署装配、零停机切换、Redis 持久化与备份 RPO | 已接受 | 告警 cron 由 deploy.sh 装配 + webhook 未配置响亮警告（不阻断）；分层替换取代全站 rm -f；Redis noeviction+AOF；备份 1h/48 份带心跳告警 |
| [0016](0016-open-id-cross-channel-tombstone-bypass.md) | open_id 首注渠道语义；跨端删号重注册绕过权益墓碑 | 已接受 | open_id=首次注册渠道标识（unionid 归一双端）；跨端对抗性绕过登记为已接受风险，触发条件出现再改 unionid 墓碑键 |
| [0017](0017-family-join-exposes-personal-history.md) | 加入家庭即反向暴露个人全部历史日记 | 已接受 | 家庭功能核心前提；视图语义（退出即收回、在期副本不可追回）；不加确认/预览（过度防御） |
| [0018](0018-oss-versioning-data-retention.md) | OSS 用户图片开启版本化——对象数据保留与防删策略 | 已接受 | 不可再生用户数据的唯一防误删解；deploy.sh 幂等开启；跨区复制/清单备份不做（成本不值）；同机凭证泄露残余已接受 |
| [0019](0019-family-removed-rejoin-cooldown.md) | 家庭移除冷却：被 owner 移出的成员 7 天内禁止经邀请链接重新加入 | 已接受 | families.removed_members jsonb + join 事务内检查（含整家合并路径）；小号重入与误操作恢复残余已登记 |
