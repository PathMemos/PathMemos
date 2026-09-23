# 0014: 依赖固定版本、关闭自动依赖升级

- 状态：已接受
- 日期：2026-09
- 取代：无

## 背景

项目为小规模私有 SaaS，运行时依赖 Go 模块与 npm 包。曾短暂启用 Dependabot 每周版本更新，产生多个升级分支；其中多项 major 升级会直接破坏现有能力：

- `pgx v5.11` 新增 `Rows.TypeMap`，`pgxmock v4.9.0` 未实现 → 测试编译失败；
- `golang.org/x/image v0.46` / `x/sync v0.23` 要求 Go ≥1.26（级联 `x/text v0.42`），项目锁定 Go 1.25；
- `typescript 7` 超出 `@typescript-eslint/parser` 的 peer 约束（`<6.1`）；
- `eslint 10` 不再支持仓库使用的 `.eslintrc.js`。

自动升级带来的不可控破坏风险，大于「依赖始终最新」的收益。

## 决策

**我决定**：不启用自动依赖版本更新（移除 `.github/dependabot.yml`），依赖固定版本，升级改为按需、成套、受控流程：

- Go：`go.mod` 精确版本 + `go.sum` 哈希；只用 `go get` 手工升级。
- npm：以 `package-lock.json` 为准，CI 一律 `npm ci`；api-worker / mcp-worker / frontend/miniapp 三个包目录 `.npmrc` 设 `save-exact=true`，新依赖写精确版本（website 依赖钉死由 `package-lock.json` + `npm ci` 达成）。
- GitHub Actions：钉到 commit SHA（附版本注释），不跟随可移动 tag。
- 升级触发条件（任一）：影响本项目的安全公告、需要新版本能力/兼容性、年度体检。升级必须成套、通过全部本地门禁（`lint-go`/`lint-frontend`/`lint-worker`/`check-sqlc-sync`/`test`/`spec-check`）并部署验证。

## 备选方案

- **保留 Dependabot**：否决——持续产生分支/PR，且 major 升级仍需人工兜底，与「稳定优先、避免升级损坏功能」冲突。
- **保留 Dependabot 但忽略已知 major、只自动吃 minor/patch**：暂不采纳——本项目对 npm 工具链（TS/ESLint/worker 类型）兼容敏感，升级仍希望由人确认。

## 后果

### 正面

- 构建与部署可复现；不会因自动升级引入破坏。
- 仓库保持单分支，无机器人噪音。

### 负面 / 代价

- 上游安全补丁不会自动浮现，需定期（建议至少每年一次）人工复查 `go list -m -u all` / `npm outdated`，并关注依赖安全公告。
- 长期不升级会积累版本债；需要时按本决策的「成套升级」流程处理。
