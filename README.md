# PathMemos Open 部署指南

## 前置条件

- 一台能运行 Docker 的服务器（本地电脑、VPS、NAS 均可）
- 一个 Cloudflare 账号（免费，用于创建 Tunnel 提供 HTTPS 入口）
- 一个 DeepSeek / OpenAI 等 AI 服务的 API Key
- 一个腾讯地图 Key

## 快速开始（推荐）

在一台空服务器上，一行命令完成全部部署：

```bash
curl -fsSL https://raw.githubusercontent.com/Papafeiji/pathmemos/open/scripts/install.sh | bash
```

国内用户可选 Gitee 镜像：

```bash
PATHMEMOS_MIRROR=gitee bash -c "$(curl -fsSL https://gitee.com/wowproton/path-memos/raw/open/scripts/install.sh)"
```

**前置准备：**

- 一台 Linux 服务器（建议 Ubuntu 22.04+）
- root 权限
- AI API Key（DeepSeek / OpenAI 等）
- 腾讯地图 Key
- Cloudflare API Token（脚本自动创建 Tunnel）

Cloudflare API Token 需要以下权限：

- `Account: Cloudflare Tunnel: Edit`
- `Account: Account: Read`
- `Zone: Zone: Read`（脚本自动列出你的域名供选择）
- `Zone: DNS: Edit`（脚本会自动为自定义域名添加 CNAME 记录）

脚本会自动完成：检查环境 → 安装 Docker → 下载代码 → 运行交互式部署 → 自动创建 Tunnel → 启动公网入口 → 健康检查。

## 分步部署

如果你不想用一键脚本，也可以手动执行：

```bash
# 1. 克隆仓库
git clone -b open https://github.com/Papafeiji/pathmemos.git
cd pathmemos

# 2. 交互式生成配置并启动服务
./scripts/init.sh

# 3. 启动 Cloudflare Tunnel 暴露到公网（可选，一键脚本已包含此步骤）
./scripts/expose.sh
```

执行 `expose.sh` 后，按提示在 Cloudflare Dashboard 创建 tunnel 并粘贴 token。**关键一步**：创建 Public Hostname 时，Service URL 必须填写 `http://nginx:80`，这样 tunnel 才能把流量转发到本机的 nginx 容器。

## 在小程序中切换后端

1. 打开小程序 → 我的 → 开源版本
2. 开启「私有化后端」
3. 填写后端地址和 Open API Key
   - Named Tunnel / 自有域名示例：`https://diary.example.com`
   - Quick Tunnel 示例：`https://xxxxx.trycloudflare.com`
   - `cfargotunnel.com` 只是 CNAME 目标，不能作为后端地址填写
4. 点击「测试连接」→「保存并切换」
5. 重新登录

## 环境变量

必填项：

- `AI_API_KEY`：DeepSeek / OpenAI 等 AI 服务 API Key
- `TENCENT_MAP_KEY`：腾讯地图 Key

可选项：

- `AI_BASE_URL` / `AI_MODEL`：AI 服务配置（服务商由 Base URL 与模型名决定）
- `OPEN_API_KEY`：后端 API Key（`init.sh` 自动生成），在小程序「开源版本」页面填写此 Key 即可切换后端
- `CLOUDFLARE_TUNNEL_TOKEN`：Cloudflare Tunnel token（`install.sh` / `expose.sh` 自动写入）
- `API_HOST`：后端公网地址（`install.sh` / `expose.sh` 自动写入）
- `WORKER_SECRET`：Cloudflare Worker 共享密钥（可选）
- `TRUSTED_PROXY_CIDR`：可信代理 CIDR（`init.sh` 默认写入 172.16.0.0/12、10.0.0.0/8、192.168.0.0/16、127.0.0.0/8 私网段）
- `NGINX_HTTP_PORT`：nginx 宿主机 HTTP 端口（默认 `80`；使用 Caddy HTTPS 时需改为 `8080` 等避免冲突）

## 功能与限制说明

1. **微信支付不可用。** 回调指向 SaaS 官方，开源版无法自动开通会员，小程序内付费入口已隐藏，**免费 VIP 仍可领取**。
2. **公众号消息不可用。** 微信回调 URL 唯一且指向 SaaS，开源版保留代码但无法接收消息。
3. **MCP 接入**：部署后即可使用 `https://<你的后端>/mcp` 连接 MCP 客户端。
4. **数据独立**：切换后端地址后数据独立，与原数据互不相通，SaaS 数据不会自动迁移。
5. **文件存储**：保存在服务器本地磁盘，与 SaaS 的 OSS 不互通。

## 使用自有域名和证书（可选）

如果你有自己的域名，可以跳过 Cloudflare Tunnel，使用 Caddy 自动获取 Let's Encrypt 证书：

```bash
# 1. 复制并编辑 Caddyfile
cp Caddyfile.example Caddyfile
# 修改 your-domain.com 为你的域名

# 2. 释放 80 端口给 Caddy：让 nginx 改绑其他端口（如 8080）
echo 'NGINX_HTTP_PORT=8080' >> .env
docker compose up -d nginx

# 3. 启动 Caddy
docker compose --profile https up -d caddy

# 4. 更新 API_HOST
# 编辑 .env，设置 API_HOST=https://your-domain.com
# 然后重建应用容器: docker compose up -d app
```

> 注意：DNS 需先指向本机 IP，且 80/443 端口不被占用。使用 Caddy 后 Cloudflare Tunnel 可关闭。

## 为什么使用 Cloudflare Tunnel

- **免费**：Cloudflare 免费账号即可使用
- **无需注册域名**：Quick Tunnel 自动分配 `*.trycloudflare.com` 临时地址；Named Tunnel 配合自有域名获得固定地址
- **无需证书**：Tunnel 入口自动 HTTPS
- **无需公网 IP**：只要能访问 Cloudflare 即可
- **地址固定**：Named Tunnel + 自有域名，地址永久不变
- **支持 SSE**：AI 对话流式输出正常可用

## 常见问题

### Quick Tunnel（trycloudflare.com）和 Named Tunnel 怎么选？

- **Quick Tunnel**（安装时选「零域名临时隧道」）：零配置、无需 Cloudflare 账号，SSE/AI 对话可用；但**服务器或隧道进程重启后地址会变化**，需重跑安装脚本并在小程序中更新后端地址，仅适合测试。
- **Named Tunnel**（安装时选「自定义域名」）：需要 Cloudflare API Token，地址永久固定，适合长期使用。

测试跑通后建议切换到 Named Tunnel。

### expose.sh 提示 Public Hostname 的 URL 填什么？

必须填 `http://nginx:80`。因为 cloudflared 容器和 nginx 容器在同一个 Docker 网络中，cloudflared 通过 `nginx` 这个主机名访问 nginx 的 80 端口。

### 为什么不需要配置微信小程序 AppID/Secret？

内置了微信凭证，无需填写即可使用。如需换成自己的小程序，参考「更换自己的小程序」一节。

### 切换 OPEN_API_KEY 后需要做什么？

修改 `.env` 中的 `OPEN_API_KEY` 后重启应用（`docker compose up -d app`），启动时会自动同步到数据库。注意：通过 API Key 写入的数据与小程序端操作的数据相互独立。

## 运维

```bash
# 升级（数据库 migration 由 app 启动时自动增量执行）
./scripts/upgrade.sh

# 回滚（回退到上一个稳定版本）
./scripts/rollback.sh

# 查看应用日志
docker compose logs -f app

# 查看 tunnel 日志（Named Tunnel）
docker compose logs -f cloudflared

# 查看 tunnel 日志（Quick Tunnel 临时测试模式）
tail -f /var/log/pathmemos-quicktunnel.log

# 重启应用（配置变更后需重建容器才会生效）
docker compose up -d app

# 备份数据库
docker compose exec postgres pg_dump -U papafeiji -d papafeiji > backup.sql

# 自动备份（后台容器，每 24 小时备份数据库（pg_dump gz）与 uploads 目录，
# 默认保留 14 份（可用 `BACKUP_KEEP` 调整份数、`BACKUP_INTERVAL` 调整间隔秒数），
# 文件在 ./backups/）
```

## 更换自己的小程序（可选）

内置了微信凭证，无需注册即可使用。建议生产环境替换为自己的小程序：

1. 在 [微信公众平台](https://mp.weixin.qq.com/) 注册小程序
2. 获取 AppID 和 AppSecret
3. 在 `.env` 中设置 `WECHAT_APPID` 和 `WECHAT_SECRET`
4. 重启服务：`docker compose up -d app`

注意：更换小程序后，用户需在新小程序中重新登录，原小程序中的数据无法迁移。
