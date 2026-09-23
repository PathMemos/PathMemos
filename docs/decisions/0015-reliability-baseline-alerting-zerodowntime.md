# 0015: 运维可靠性基线——告警随部署装配、零停机切换、Redis 持久化与备份 RPO

- 状态：已接受
- 日期：2026-09-16

## 背景

对照主流实践的诊断发现四个系统性短板：

1. **告警通道未装配**：`alert-watch.sh`、`accesslog-p95.sh` 均已存在，但 deploy.sh 不安装其 cron，`ALERT_WEBHOOK_URL` 未强制——支付异常、配额退款失败、watchdog 熔断等关键字默认不触达任何人。
2. **每次部署全站硬停机**：`docker rm -f nginx app sse certbot` 后整体 `up -d`，切换期 80/443 完全不可用（秒级~分钟级），且代码中的优雅停机（SIGTERM→`Shutdown` 30s）永不执行。
3. **Redis 静默失败模式**：`volatile-lru` 优先淘汰带 TTL 的 key 而 session 恰好全带 TTL——内存压力表现为"用户陆续莫名掉登录"而非报错；仅 RDB 快照，重启即全员登出。
4. **备份 RPO 24h 且无监督**：backup 容器失败只写自身日志（无告警、watchdog 不覆盖），恢复粒度为备份时间点。

## 决策

我决定建立"最小但闭环"的运维可靠性基线（ADR-0015/ADR-0015）：

1. **告警随部署装配**：deploy.sh 自动安装 4 个 cron（watchdog 每分钟、alert-cron 每 2 分钟、alert-p95 每 15 分钟、cert-check 每小时，均带 marker 去重）；`CFG_ALERT_WEBHOOK_URL` 列为 SaaS 部署必填项；新增 `ai_upstream_error`/`auto_record_failed`/`backup_*`/`redis_mem_high`/`slo_breach`/`cert_expiry_soon`/`uptime_*` 关键字；外部拨测 `uptime-check.sh` 在控制机手工装 cron。
2. **零停机切换**：分层替换（先 `up -d postgres redis backup`，再 `up -d app sse`，sse `stop_grace_period: 35s` 让在途流优雅收尾）；nginx 最后统一处理——未运行才启动、配置相对 `.prev` 变化才经一次性容器 `nginx -t` 校验后 force-recreate，最后总是 `nginx -s reload` 重解析 upstream DNS。
3. **Redis**：`noeviction`（写满显式报错而非静默踢人）+ AOF `everysec`（重启后 session 从 AOF 恢复）+ watchdog 内存水位检查（≥90% 告警）。
4. **备份**：间隔默认 1h（RPO ≤ 1h）、保留 48 份；成功写 `backups/.last_success` 心跳，watchdog 检查心跳与 Redis 水位；证书续期到生效从最长 24h 缩到 1h（cert-check 每小时 reload-on-change）。

配套观测：AI 上游 token 用量以结构化日志记录（`msg="ai chat usage"`，`scripts/ai-cost.sh` 汇总，不落库）；`pg_stat_statements` 扩展 + `scripts/sql-top.sh` 慢 SQL 榜单；`scripts/rehearse-migration.sh` 迁移预演（备份恢复进一次性容器跑迁移）。

## 备选方案

- **蓝绿/双实例滚动发布**：需要多副本 upstream、健康检查代理或 LB，单机 4C8G 与"简单优先"原则下收益不匹配；分层替换已把停机压到 API 秒级瞬断。
- **WAL 归档/PITR**：RPO 进一步缩到分钟级，但引入归档目录管理、恢复演练复杂度；对一个记录类产品 1h RPO 已足够，付费订单可用部署前备份 + 人工补发兜底。
- **Prometheus/OTel 指标体系**：需要时序存储与大盘运维面；当前结构化日志 + 关键字告警 + p95 脚本已覆盖可用性观测，等出现"指标驱动决策"的真实需求再上。
- **Redis keydb/哨兵或多实例**：单机部署下无意义；AOF + noeviction 解决的是"失败模式可见"而非可用性翻倍。

## 后果

### 正面

- 故障发现从"用户投诉"变为分钟级 webhook 触达；支付/AI/备份/证书/任务失联全链路有告警面。
- 部署不再全站停机；SSE 生成中的用户由优雅关闭 + 回放缓存自然续上。
- Redis 重启不再全员掉登录；内存压力成为显式告警事件。
- 最坏丢数据从 24h 缩到 1h；备份失败 ≤2 分钟可发现；迁移上生产前可在"生产形状的数据"上预演。

### 负面 / 代价

- `CFG_ALERT_WEBHOOK_URL` 成为部署硬前置：未配置的存量环境首次部署会被拒绝（一次性配置成本，属有意强制）。
- Redis AOF 增加磁盘写入（384MB 数据量级下可忽略）与 `noeviction` 下写满显式报错的新失败模式（由水位告警前置兜住）。
- 备份保留 48 份增加磁盘占用（按 dump 实际体积评估；`BACKUP_KEEP`/`BACKUP_INTERVAL` 可调）。
- deploy.sh 的 nginx 切换依赖 `nginx.conf.prev` 快照与"reload 重解析 upstream DNS"语义，后续修改 nginx 渲染逻辑时必须保持这两点。
- 拨测脚本在控制机手工装 cron，不随部署自动管理（控制机环境各异，避免 deploy.sh 越权改宿主机 crontab 之外的东西）。

> **注（2026-09 审定）**：决策 1 与「负面/代价」中「`CFG_ALERT_WEBHOOK_URL` 列为 SaaS 部署必填项/部署硬前置」已放宽：现为**未配置时响亮警告但不阻断部署**（`deploy/deploy.sh` 部署前警告块；`deploy/required-runtime-vars.txt` 不含该变量）。告警随部署装配（4 个 cron）的其余内容不变。
