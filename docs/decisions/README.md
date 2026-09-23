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
- **已接受不可改写**：改了决策要新增一条，并把旧条标记为 `已取代`、新条写 `取代：<旧序号>`。
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
| [0011](0011-payment-callback-amount-policy.md) | 支付回调安全模型与金额策略 | 已接受 | 金额不作为漏发闸门（>0 即发货+告警）；无自动对账；体验版 403 为预期 |
| [0012](0012-family-invite-owner-merge-risk.md) | 家庭邀请 owner 整家合并钓鱼风险 | 已接受 | 熟人小家庭场景下接受该风险，不增加防护 |
| [0013](0013-deleted-image-cache-purge.md) | 已删除图片边缘缓存定期批量收敛 | 已接受 | 删除图片须最终不可达；定期批量 purge，不做同步清理 |
| [0014](0014-pinned-deps-manual-upgrades.md) | 依赖固定版本、关闭自动依赖升级 | 已接受 | 不启用自动依赖更新；锁定版本，按需成套升级 |
| [0015](0015-reliability-baseline-alerting-zerodowntime.md) | 运维可靠性基线：告警随部署装配、零停机切换、Redis 持久化与备份 RPO | 已接受 | 告警 cron 由 deploy.sh 装配 + webhook 必填；分层替换取代全站 rm -f；Redis noeviction+AOF；备份 1h/48 份带心跳告警 |
