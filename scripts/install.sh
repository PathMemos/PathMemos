#!/bin/bash
# 爬爬记忆助手【PathMemos】一键安装脚本
#
# 用法：
# curl -fsSL https://raw.githubusercontent.com/Papafeiji/pathmemos/open/scripts/install.sh | bash
# 国内用户可选：
#   PATHMEMOS_MIRROR=gitee bash -c "$(curl -fsSL https://gitee.com/wowproton/path-memos/raw/open/scripts/install.sh)"
#
# 前置准备：
#   - 一台 Linux/macOS 服务器（建议 Ubuntu 22.04+）
#   - root 权限（脚本会检测并使用 sudo）
#   - AI API Key（DeepSeek / OpenAI 等）
#   - 腾讯地图 Key
#   - Cloudflare API Token（仅「自定义域名」模式需要；「临时隧道」无需 Cloudflare 账号）
#
# 脚本会自动完成：检查环境 → 安装 Docker → 下载代码 → 交互式配置 →
# 配置 Cloudflare Tunnel（自定义域名 或 零域名临时隧道）→ 启动公网入口 → 健康检查。
#
# Cloudflare API Token 权限要求（自定义域名模式）：
#   - Account: Account: Read
#   - Account: Cloudflare Tunnel: Edit
#   - Zone: Zone: Read
#   - Zone: DNS: Edit

set -eo pipefail

# ---------- 配置 ----------
PATHMEMOS_REPO="https://github.com/Papafeiji/pathmemos.git"
PATHMEMOS_BRANCH="open"
PATHMEMOS_DIR="${PATHMEMOS_DIR:-/opt/pathmemos}"
GUM_VERSION="${PATHMEMOS_GUM_VERSION:-0.15.2}"

# 国内镜像：设置 PATHMEMOS_MIRROR=gitee 后，所有 GitHub 下载自动 fallback 到 Gitee
if [[ "${PATHMEMOS_MIRROR:-github}" == "gitee" ]]; then
  PATHMEMOS_REPO="https://gitee.com/wowproton/path-memos.git"
  PATHMEMOS_BRANCH="open"
fi

USE_SUDO=""

# 支持 curl | bash 管道执行：此时 $0 不是脚本路径，本地无 lib/ui.sh，从仓库下载后加载。
_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo ".")"
if [[ -f "${_script_dir}/lib/ui.sh" ]]; then
  source "${_script_dir}/lib/ui.sh"
else
  _ui_tmp="$(mktemp)"
  # 脚本退出时统一清理下载的 ui.sh 临时文件
  trap 'rm -f "$_ui_tmp"' EXIT
  _ui_raw_url="https://raw.githubusercontent.com/Papafeiji/pathmemos/${PATHMEMOS_BRANCH}/scripts/lib/ui.sh"
  _ui_fallback_url="https://gitee.com/wowproton/path-memos/raw/${PATHMEMOS_BRANCH}/scripts/lib/ui.sh"
  if curl -fsSL --retry 2 --connect-timeout 5 --max-time 20 "$_ui_raw_url" -o "$_ui_tmp"; then
    source "$_ui_tmp"
  elif [[ "$_ui_raw_url" != "$_ui_fallback_url" ]] && curl -fsSL --retry 2 --connect-timeout 5 --max-time 20 "$_ui_fallback_url" -o "$_ui_tmp"; then
    source "$_ui_tmp"
  else
    echo "无法下载远程代码仓库（$_ui_raw_url）。" >&2
    echo "请改用手动方式：git clone -b ${PATHMEMOS_BRANCH} ${PATHMEMOS_REPO} && cd pathmemos && ./scripts/install.sh" >&2
    exit 1
  fi
fi

GUM="${GUM:-}"

require_root() {
  if [[ "$EUID" -eq 0 ]]; then
    USE_SUDO=""
    return 0
  fi
  if command -v sudo >/dev/null 2>&1; then
    USE_SUDO="sudo"
    return 0
  fi
  ui_error "需要 root 权限或 sudo"
  exit 1
}

run_as_root() {
  if [[ -n "$USE_SUDO" ]]; then
    $USE_SUDO "$@"
  else
    "$@"
  fi
}

# 持久化安装 gum 到本地路径（install.sh 专用，与 lib/ui.sh 的临时版不同）
bootstrap_gum() {
  if ! is_tty; then GUM=""; return 1; fi
  if command -v gum >/dev/null 2>&1; then
    GUM="gum"
    return 0
  fi
  if ! command -v tar >/dev/null 2>&1; then GUM=""; return 1; fi

  local os arch asset base tmpdir gum_path
  os=$(uname -s); arch=$(uname -m)
  case "$os" in Darwin) os="Darwin" ;; Linux) os="Linux" ;; *) GUM=""; return 1 ;; esac
  case "$arch" in
    x86_64|amd64) arch="x86_64" ;;
    arm64|aarch64) arch="arm64" ;;
    i386|i686) arch="i386" ;;
    armv7l|armv7) arch="armv7" ;;
    *) GUM=""; return 1 ;;
  esac

  asset="gum_${GUM_VERSION}_${os}_${arch}.tar.gz"
  base="https://github.com/charmbracelet/gum/releases/download/v${GUM_VERSION}"
  tmpdir=$(mktemp -d)
  # 触发后自我清除：RETURN trap 不清除会在函数返回后残留为全局 trap
  trap "rm -rf \"$tmpdir\"; trap - RETURN" RETURN

  ui_info "正在下载安装程序..."
  if ! curl -fsSL --retry 2 --connect-timeout 5 --max-time 20 "${base}/${asset}" -o "${tmpdir}/${asset}"; then
    ui_warn "安装程序下载失败，将使用基础交互模式"; GUM=""; return 1
  fi
  if ! tar -xzf "${tmpdir}/${asset}" -C "$tmpdir" >/dev/null 2>&1; then
    ui_warn "安装程序解压失败，将使用基础交互模式"; GUM=""; return 1
  fi
  gum_path=$(find "$tmpdir" -type f -name gum 2>/dev/null | head -n1)
  [[ -z "$gum_path" ]] && { ui_warn "安装程序二进制未找到"; GUM=""; return 1; }
  chmod +x "$gum_path"

  local install_dir="/usr/local/bin"
  if [[ -d "$install_dir" && -w "$install_dir" ]]; then
    if ! run_as_root cp -f "$gum_path" "${install_dir}/gum"; then
      ui_warn "安装程序复制失败，将使用基础交互模式"; GUM=""; return 1
    fi
  else
    # 用户级安装目录必须在 PATHMEMOS_DIR 之外：clone_repo 的 git clone 要求目标目录不存在或为空
    install_dir="${HOME:-${TMPDIR:-/tmp}}/.local/bin"
    if ! mkdir -p "$install_dir" 2>/dev/null || ! cp -f "$gum_path" "${install_dir}/gum" 2>/dev/null; then
      ui_warn "安装程序复制失败，将使用基础交互模式"; GUM=""; return 1
    fi
  fi
  # 成功前验证二进制可执行，避免假成功导致后续调用失败
  if [[ ! -x "${install_dir}/gum" ]]; then
    ui_warn "安装程序二进制不可执行，将使用基础交互模式"; GUM=""; return 1
  fi
  GUM="${install_dir}/gum"
  return 0
}

# ---------- 步骤 ----------
check_prerequisites() {
  if ! command -v curl >/dev/null 2>&1; then
    ui_error "需要 curl，请先安装: apt-get install curl / yum install curl"
    exit 1
  fi

  if ! command -v awk >/dev/null 2>&1; then
    ui_error "需要 awk，请先安装"
    exit 1
  fi

  # 磁盘空间检查：建议至少 10G 可用
  local avail_gb
  avail_gb=$(df -P -BG / | awk 'NR==2 {print int($4)}')
  if [[ "$avail_gb" -lt 10 ]]; then
    ui_warn "根分区仅剩 ${avail_gb}G，建议至少 10G 可用空间"
    if ! _gum_confirm "空间不足，是否继续？" --default no; then
      exit 0
    fi
  fi

  # 端口检查：80 是否被占用
  if command -v ss >/dev/null 2>&1; then
    if ss -tln | awk '{print $4}' | grep -qE '(:80$|:80\b)'; then
      ui_warn "本机 80 端口已被占用"
      if ! _gum_confirm "80 端口被占用，是否继续？（继续可能导致 Nginx 启动失败）" --default no; then
        exit 0
      fi
    fi
  fi
}

detect_os() {
  case "$(uname -s)" in
    Linux) echo "linux" ;;
    Darwin) echo "macos" ;;
    *) echo "unknown" ;;
  esac
}

get_mem_gb() {
  local key="$1"
  awk "/^${key}:/ {print int(\$2/1024/1024)}" /proc/meminfo
}

create_swap_if_needed() {
  # 仅 Linux 有 /proc/meminfo 与 mkswap/swapon；macOS 跳过（内存由 Docker Desktop 管理），
  # 否则 get_mem_gb 读不到 /proc/meminfo 会在 set -e 下中止，挡住后面给 macOS 的专门指引。
  if [[ "$(detect_os)" != "linux" ]]; then
    return 0
  fi
  local mem_gb swap_gb needed_swap target_swap swapfile avail_gb
  mem_gb=$(get_mem_gb "MemTotal")
  swap_gb=$(get_mem_gb "SwapTotal")

  if [[ "$mem_gb" -ge 4 ]]; then
    ui_success "物理内存 ${mem_gb}G，无需 swap"
    return 0
  fi

  ui_warn "物理内存 ${mem_gb}G，小于 4G"

  needed_swap=$((4 - mem_gb))
  target_swap=$((needed_swap - swap_gb))
  if [[ "$target_swap" -lt 2 ]]; then
    target_swap=2
  fi

  if [[ "$swap_gb" -gt 0 ]]; then
    ui_info "当前 swap ${swap_gb}G，还需补充 ${target_swap}G"
  fi

  swapfile="/swapfile-pathmemos"
  if [[ -f "$swapfile" ]]; then
    ui_info "已有 ${swapfile}，尝试启用..."
    run_as_root swapon "$swapfile" 2>/dev/null || true
    return 0
  fi

  # 检查磁盘空间（取 / 可用 GB）
  avail_gb=$(df -BG / | awk 'NR==2 {print int($4)}')
  if [[ "$((avail_gb - 2))" -lt "$target_swap" ]]; then
    target_swap=$((avail_gb - 2))
    if [[ "$target_swap" -lt 1 ]]; then
      ui_error "根分区空间不足，无法创建 swap"
      exit 1
    fi
    ui_warn "磁盘空间有限，swap 调整为 ${target_swap}G"
  fi

  ui_info "创建 ${target_swap}G swap 文件（位于 ${swapfile}），可能需要几分钟..."
  if command -v fallocate >/dev/null 2>&1; then
    run_as_root fallocate -l "${target_swap}G" "$swapfile" || {
      ui_warn "fallocate 失败，尝试 dd..."
      run_as_root dd if=/dev/zero of="$swapfile" bs=1M count=$((target_swap * 1024)) status=progress
    }
  else
    run_as_root dd if=/dev/zero of="$swapfile" bs=1M count=$((target_swap * 1024)) status=progress
  fi

  run_as_root chmod 600 "$swapfile"
  run_as_root mkswap "$swapfile"
  run_as_root swapon "$swapfile"
  if ! grep -q "$swapfile" /etc/fstab; then
    echo "$swapfile none swap sw 0 0" | run_as_root tee -a /etc/fstab >/dev/null
  fi

  ui_success "swap 创建完成: ${target_swap}G"
}

install_git() {
  if command -v git >/dev/null 2>&1; then
    ui_success "git 已安装"
    return 0
  fi
  ui_info "正在安装 git..."
  local os
  os=$(detect_os)
  if [[ "$os" == "linux" ]]; then
    if command -v apt-get >/dev/null 2>&1; then
      run_as_root apt-get update -qq
      run_as_root apt-get install -y -qq git
    elif command -v dnf >/dev/null 2>&1; then
      run_as_root dnf install -y -q git
    elif command -v yum >/dev/null 2>&1; then
      run_as_root yum install -y -q git
    elif command -v apk >/dev/null 2>&1; then
      run_as_root apk add --no-cache git
    else
      ui_error "无法自动安装 git，请手动安装后重试"
      exit 1
    fi
  elif [[ "$os" == "macos" ]]; then
    if ! command -v git >/dev/null 2>&1; then
      ui_error "macOS 上请安装 Xcode Command Line Tools: xcode-select --install"
      exit 1
    fi
  else
    ui_error "不支持的操作系统"
    exit 1
  fi
  ui_success "git 安装完成"
}

ensure_jq() {
  if command -v jq >/dev/null 2>&1; then
    ui_success "jq 已就绪"
    return 0
  fi

  ui_info "正在安装 jq..."
  local os
  os=$(detect_os)
  if [[ "$os" == "linux" ]]; then
    if command -v apt-get >/dev/null 2>&1; then
      run_as_root apt-get update -qq
      run_as_root apt-get install -y -qq jq
    elif command -v dnf >/dev/null 2>&1; then
      run_as_root dnf install -y -q jq
    elif command -v yum >/dev/null 2>&1; then
      run_as_root yum install -y -q jq
    elif command -v apk >/dev/null 2>&1; then
      run_as_root apk add --no-cache jq
    else
      ui_error "无法自动安装 jq，请手动安装后重试"
      exit 1
    fi
  elif [[ "$os" == "macos" ]]; then
    if command -v brew >/dev/null 2>&1; then
      run_as_root brew install jq
    else
      ui_error "macOS 上请手动安装 jq: https://jqlang.github.io/jq/download/"
      exit 1
    fi
  else
    ui_error "不支持的操作系统"
    exit 1
  fi
  ui_success "jq 安装完成"
}

docker_is_ready() {
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1
}

# 后续所有 docker compose 命令均不带 sudo：非 root 用户若无 docker socket 权限，
# 与其在每处包装 sudo，不如在此一次性拦截并给出明确指引（简单优先）。
ensure_docker_socket_access() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi
  if [[ -n "$USE_SUDO" ]]; then
    ui_error "当前用户无 Docker 权限（不在 docker 组）"
    ui_info "请改用 root 运行本脚本：sudo -i 后重新执行"
    ui_info "或将当前用户加入 docker 组后重新登录：sudo usermod -aG docker \$USER"
    exit 1
  fi
  return 0
}

install_docker_via_official() {
  ui_info "尝试官方源安装 Docker..."
  local tmp_script
  tmp_script=$(mktemp)
  trap 'rm -f "$tmp_script"; trap - RETURN' RETURN

  if ! curl -fsSL --retry 2 --retry-delay 1 --connect-timeout 10 --max-time 60 -o "$tmp_script" https://get.docker.com; then
    ui_warn "官方安装脚本下载失败，准备切换国内源"
    return 1
  fi

  # 即使安装脚本返回非零，只要 docker 可用就算成功（系统其他包损坏时常见）
  sh "$tmp_script" || true

  if docker_is_ready; then
    return 0
  fi
  ui_warn "官方源安装后 Docker 不可用，准备切换国内源"
  return 1
}

install_docker_via_aliyun() {
  ui_info "使用阿里云镜像源安装 Docker..."

  if command -v apt-get >/dev/null 2>&1; then
    install_docker_via_aliyun_apt
  elif command -v dnf >/dev/null 2>&1; then
    install_docker_via_aliyun_dnf
  elif command -v yum >/dev/null 2>&1; then
    install_docker_via_aliyun_yum
  else
    ui_error "无法识别包管理器，无法使用国内源安装"
    return 1
  fi
}

install_docker_via_aliyun_apt() {
  local codename os_id
  codename=$(. /etc/os-release && echo "$VERSION_CODENAME")
  os_id=$(. /etc/os-release && echo "$ID")
  if [[ -z "$codename" ]]; then
    ui_error "无法获取系统版本代号（VERSION_CODENAME）"
    return 1
  fi

  # 阿里云仓库区分 ubuntu / debian
  local repo_id="ubuntu"
  case "$os_id" in
    debian) repo_id="debian" ;;
    ubuntu|*) repo_id="ubuntu" ;;
  esac

  run_as_root apt-get update -y || true
  run_as_root apt-get install -y ca-certificates curl gnupg || true
  run_as_root install -m 0755 -d /etc/apt/keyrings || true
  curl -fsSL --retry 2 --retry-delay 1 --connect-timeout 10 --max-time 30 "https://mirrors.aliyun.com/docker-ce/linux/${repo_id}/gpg" \
    | run_as_root gpg --dearmor -o /etc/apt/keyrings/docker.gpg || true
  run_as_root chmod a+r /etc/apt/keyrings/docker.gpg || true
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://mirrors.aliyun.com/docker-ce/linux/${repo_id} ${codename} stable" \
    | run_as_root tee /etc/apt/sources.list.d/docker.list >/dev/null || true
  run_as_root apt-get update -y || true
  # 允许 apt 因系统其他包损坏而返回非零，后续以 docker 是否可用为准
  run_as_root apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || true

  if docker_is_ready; then
    return 0
  fi
  return 1
}

install_docker_via_aliyun_dnf() {
  run_as_root dnf install -y dnf-plugins-core || true
  run_as_root dnf config-manager --add-repo https://mirrors.aliyun.com/docker-ce/linux/centos/docker-ce.repo || true
  run_as_root dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || true

  if docker_is_ready; then
    return 0
  fi
  return 1
}

install_docker_via_aliyun_yum() {
  run_as_root yum install -y yum-utils || true
  run_as_root yum-config-manager --add-repo https://mirrors.aliyun.com/docker-ce/linux/centos/docker-ce.repo || true
  run_as_root yum install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || true

  if docker_is_ready; then
    return 0
  fi
  return 1
}

start_docker_service() {
  if ! command -v systemctl >/dev/null 2>&1; then
    if command -v service >/dev/null 2>&1; then
      run_as_root service docker start >/dev/null 2>&1 || true
    fi
    return 0
  fi

  # docker.socket 的 SocketGroup=docker，若 docker 组被误删会导致 socket 启动失败
  if ! getent group docker >/dev/null 2>&1; then
    ui_info "docker 用户组不存在，正在重建..."
    run_as_root groupadd docker || true
    run_as_root systemctl daemon-reload || true
  fi

  # 如果是通过 sudo 运行的，把原用户加入 docker 组，方便后续免 sudo 使用 docker
  if [[ -n "${SUDO_USER:-}" ]] && id "$SUDO_USER" >/dev/null 2>&1; then
    if ! id -nG "$SUDO_USER" | grep -qw docker; then
      ui_info "将 ${SUDO_USER} 加入 docker 组..."
      run_as_root usermod -aG docker "$SUDO_USER" || true
    fi
  fi

  run_as_root systemctl enable docker.socket >/dev/null 2>&1 || true
  run_as_root systemctl start docker.socket >/dev/null 2>&1 || true
  run_as_root systemctl enable docker.service >/dev/null 2>&1 || true
  run_as_root systemctl start docker.service >/dev/null 2>&1 || true
}

configure_docker_mirrors() {
  local daemon_file="/etc/docker/daemon.json"
  local mirrors='[
    "https://docker.m.daocloud.io",
    "https://docker.cloud.tencent.com",
    "https://hub-mirror.c.163.com",
    "https://mirror.baidubce.com"
  ]'

  # 先探测 Docker Hub 是否能正常拉取
  if docker pull hello-world >/dev/null 2>&1; then
    ui_success "Docker Hub 可正常访问，无需配置镜像"
    return 0
  fi

  ui_warn "Docker Hub 访问超时，配置国内镜像..."

  if [[ -f "$daemon_file" ]]; then
    if grep -q "registry-mirrors" "$daemon_file"; then
      ui_info "daemon.json 已配置 registry-mirrors，跳过"
      return 0
    fi
    # 备份现有配置
    run_as_root cp -f "$daemon_file" "${daemon_file}.bak.$(date +%s)"
    if command -v jq >/dev/null 2>&1; then
      run_as_root jq --argjson mirrors "$mirrors" '. += {"registry-mirrors": $mirrors}' "$daemon_file" > "/tmp/daemon.json.tmp"
      run_as_root mv "/tmp/daemon.json.tmp" "$daemon_file"
    else
      # 没有 jq 时，备份后重写一个仅含镜像的配置
      ui_warn "未找到 jq，备份原配置后重写 daemon.json"
    fi
  fi

  if [[ ! -f "$daemon_file" ]] || ! grep -q "registry-mirrors" "$daemon_file"; then
    run_as_root tee "$daemon_file" >/dev/null <<EOF
{
  "registry-mirrors": ${mirrors}
}
EOF
  fi

  run_as_root systemctl daemon-reload || true
  run_as_root systemctl restart docker || true

  # 验证镜像是否生效
  if docker pull hello-world >/dev/null 2>&1; then
    ui_success "国内镜像配置成功"
    return 0
  fi

  ui_warn "国内镜像配置后仍无法拉取，将尝试继续"
  return 1
}

install_docker() {
  if docker_is_ready; then
    ensure_docker_socket_access
    ui_success "Docker 和 Docker Compose 已就绪"
    return 0
  fi

  ui_info "正在安装 Docker..."
  local os
  os=$(detect_os)
  if [[ "$os" == "linux" ]]; then
    if install_docker_via_official; then
      ui_success "Docker 已通过官方源安装"
    elif install_docker_via_aliyun; then
      ui_success "Docker 已通过阿里云镜像源安装"
    else
      ui_error "Docker 安装失败，官方源和国内源均不可用"
      ui_info "请手动安装 Docker 后重试: https://docs.docker.com/engine/install/"
      exit 1
    fi

    start_docker_service
    configure_docker_mirrors || true

    # 验证，给 daemon 一点启动时间
    ui_info "等待 Docker daemon 启动..."
    local i
    for i in $(seq 1 10); do
      if docker_is_ready; then
        break
      fi
      sleep 2
    done

    if ! docker_is_ready; then
      ui_error "Docker 安装后验证失败"
      ui_info "请检查日志: journalctl -u docker.service -n 50"
      ui_info "常见原因: docker 用户组缺失、docker.socket 未启动、containerd 异常"
      exit 1
    fi
    ensure_docker_socket_access
  elif [[ "$os" == "macos" ]]; then
    ui_error "macOS 请手动安装 Docker Desktop: https://www.docker.com/products/docker-desktop/"
    exit 1
  else
    ui_error "不支持的操作系统"
    exit 1
  fi

  # Docker 拉取镜像可用性校验：用 docker pull alpine:3.20 实测
  # 国内镜像已在上方尝试过，仍失败则尝试将 shell 代理配置到 Docker daemon
  local docker_pull_ok=false
  if timeout 30 docker pull alpine:3.20 >/dev/null 2>&1; then
    docker_pull_ok=true
  fi

  if ! $docker_pull_ok; then
    ui_info "镜像加速器无效，尝试将 shell 代理配置到 Docker daemon..."
    local proxy_src=""
    if [[ -n "${http_proxy:-}" ]]; then
      proxy_src="$http_proxy"
    elif [[ -n "${https_proxy:-}" ]]; then
      proxy_src="$https_proxy"
    fi
    if [[ -n "$proxy_src" ]]; then
      local proxy_conf="/etc/systemd/system/docker.service.d/proxy.conf"
      run_as_root mkdir -p "$(dirname "$proxy_conf")"
      if run_as_root tee "$proxy_conf" >/dev/null <<EOF
[Service]
Environment="HTTP_PROXY=${proxy_src}"
Environment="HTTPS_PROXY=${proxy_src}"
Environment="NO_PROXY=${no_proxy:-localhost,127.0.0.1,::1}"
EOF
      then
        run_as_root systemctl daemon-reload
        run_as_root systemctl restart docker || true
        local j
        for j in $(seq 1 12); do
          if docker info >/dev/null 2>&1; then break; fi
          sleep 2
        done
        if timeout 30 docker pull alpine:3.20 >/dev/null 2>&1; then
          docker_pull_ok=true
          ui_success "Docker daemon 代理已生效"
        fi
      fi
    fi
  fi

  if ! $docker_pull_ok; then
    ui_error "Docker 无法拉取镜像，请检查网络环境后重试"
    ui_info "如果使用了代理，请确保 Docker daemon 能通过代理访问 Docker Hub"
    exit 1
  fi
  ui_success "Docker 安装完成"
}

clone_repo() {
  if [[ -d "$PATHMEMOS_DIR/.git" ]]; then
    ui_info "目录已存在，尝试更新..."
    if ! git -C "$PATHMEMOS_DIR" pull origin "$PATHMEMOS_BRANCH"; then
      ui_warn "更新代码失败，当前目录 $PATHMEMOS_DIR 已保留（含 .env、uploads、backups）"
      ui_info "请检查网络或 origin $PATHMEMOS_BRANCH 是否可访问后重新运行本脚本"
      exit 1
    else
      ui_success "代码已就绪"
      return 0
    fi
  fi

  local parent
  parent=$(dirname "$PATHMEMOS_DIR")
  if [[ ! -d "$parent" ]]; then
    run_as_root mkdir -p "$parent"
  fi

  ui_info "下载爬爬记忆助手【PathMemos】（可能需几分钟，卡住请检查网络）..."
  # 180 秒超时，避免 GitHub 被墙或网络差时无限挂起
  if ! timeout 180 git clone -b "$PATHMEMOS_BRANCH" --depth 1 "$PATHMEMOS_REPO" "$PATHMEMOS_DIR"; then
    # 非 root 用户首次创建目录可能权限不足，尝试 sudo
    ui_info "可能因目录权限不足，尝试以 root 权限重试..."
    run_as_root mkdir -p "$PATHMEMOS_DIR"
    if ! timeout 180 run_as_root git clone -b "$PATHMEMOS_BRANCH" --depth 1 "$PATHMEMOS_REPO" "$PATHMEMOS_DIR"; then
      ui_error "代码下载失败"
      ui_info "请检查服务器能否访问 GitHub，或尝试:"
      ui_info "  PATHMEMOS_MIRROR=gitee bash -c \"\$(curl -fsSL https://gitee.com/wowproton/path-memos/raw/open/scripts/install.sh)\""
      exit 1
    fi
    run_as_root chown -R "$(whoami)" "$PATHMEMOS_DIR" 2>/dev/null || true
  fi
  ui_success "代码已就绪（浅克隆，执行 git fetch --unshallow 可拉取完整历史以启用回滚）"
}

preflight_inputs() {
	_gum_style_box "请提前准备好：\n· AI API Key（DeepSeek / OpenAI 等）\n· 腾讯地图 Key\n· 自定义域名（可选；没有则使用临时 trycloudflare.com 隧道，地址每次会变）"

	if ! _gum_confirm "是否已准备好以上项？"; then
		ui_info "请准备好后重新运行本脚本"
		exit 0
	fi
}

cleanup_data_volumes() {
	# 按 compose label 查找本项目的 postgres 数据卷，不硬编码卷名（compose 项目名 = 安装目录 basename）
	local project_name old_pg_vol="" vol
	project_name=$(basename "$PATHMEMOS_DIR")
	for vol in $(docker volume ls -q --filter label=com.docker.compose.volume=postgres_data 2>/dev/null); do
		if [[ "$(docker volume inspect "$vol" --format '{{ index .Labels "com.docker.compose.project" }}' 2>/dev/null)" == "$project_name" ]]; then
			old_pg_vol="$vol"
			break
		fi
	done
	if [[ -n "$old_pg_vol" ]]; then
		ui_warn "检测到旧的数据卷，新密码可能与之不匹配"
		ui_info "正在清理旧数据卷（PostgreSQL 与 Redis 数据会丢失，如要保留请提前备份）..."
		cd "$PATHMEMOS_DIR"
		docker compose down -v >/dev/null 2>&1 || true
		docker volume rm "$old_pg_vol" >/dev/null 2>&1 || true
		ui_success "旧数据卷已清理"
	fi
}
run_init() {
	cd "$PATHMEMOS_DIR"
	local should_init=0
	if [[ -f .env ]]; then
		if ! _gum_confirm "检测到已有配置，重新配置将删除 PostgreSQL/Redis 数据卷（保留 uploads、backups 目录），是否继续？" --default no; then
			ui_info "使用现有配置继续"
			return 0
		fi
		should_init=1
	else
		should_init=1
	fi

	if [[ "$should_init" -eq 1 ]]; then
		cleanup_data_volumes
		rm -f .env
		./scripts/init.sh
	fi
}

cf_api() {
  local method="$1"
  local path="$2"
  local data="${3:-}"
  local args=(
    -sS --retry 3 --retry-delay 1
    --connect-timeout 10 --max-time 30
    -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN"
    -H "Content-Type: application/json"
    -X "$method"
  )
  if [[ -n "$data" ]]; then
    args+=(-d "$data")
  fi
  # 不启用 -f，HTTP 错误码也会返回 body，便于统一解析 .success
  curl "${args[@]}" "https://api.cloudflare.com/client/v4$path" || echo '{"success":false,"errors":[{"message":"请求失败，请检查网络或 Token"}]}'
}

fetch_cf_zones() {
	local zones_resp
	zones_resp=$(cf_api GET "/zones?per_page=50")
	if ! echo "$zones_resp" | jq -e '.success' >/dev/null 2>&1; then
		echo ""
		return 0
	fi
	echo "$zones_resp" | jq -r '.result // [] | .[] | "\(.name) \(.id)"'
}

count_cf_zones() {
	local zones_resp count
	zones_resp=$(cf_api GET "/zones?per_page=50")
	count=$(echo "$zones_resp" | jq -r 'try (.result | length) // 0' 2>/dev/null || echo "0")
	echo "${count:-0}"
}

setup_cloudflare_tunnel_custom_domain() {
	_gum_style_box "请提前准备好：1. Cloudflare API Token\n并配置好以下权限：\n  Account > Account: Read\n  Account > Cloudflare Tunnel: Edit\n  Zone > Zone: Read\n  Zone > DNS: Edit"

	CLOUDFLARE_API_TOKEN=$(_gum_input --placeholder "Cloudflare API Token" --password)
	if [[ -z "$CLOUDFLARE_API_TOKEN" ]]; then
		ui_error "API Token 不能为空"
		exit 1
	fi

	ui_info "验证 API Token..."
	local verify_resp
	verify_resp=$(cf_api GET "/user/tokens/verify")
	if ! echo "$verify_resp" | jq -e '.success' >/dev/null 2>&1; then
		ui_error "API Token 验证失败"
		exit 1
	fi
	ui_success "API Token 验证通过"

	ui_info "获取 Cloudflare 账号..."
	local accounts_resp account_count account_id account_name
	accounts_resp=$(cf_api GET "/accounts?per_page=50")
	account_count=$(echo "$accounts_resp" | jq '.result | length')
	if [[ "$account_count" -eq 0 ]]; then
		ui_error "该 Token 无法访问任何 Cloudflare 账号"
		exit 1
	elif [[ "$account_count" -eq 1 ]]; then
		account_id=$(echo "$accounts_resp" | jq -r '.result[0].id')
		account_name=$(echo "$accounts_resp" | jq -r '.result[0].name')
	else
		local opts=() ids=() i
		for i in $(seq 0 $((account_count - 1))); do
			ids+=("$(echo "$accounts_resp" | jq -r ".result[$i].id")")
			opts+=("$(echo "$accounts_resp" | jq -r ".result[$i].name") (${ids[$i]})")
		done
		local chosen
		chosen=$(_gum_choose --header "选择 Cloudflare 账号" "${opts[@]}")
		for i in "${!opts[@]}"; do
			if [[ "${opts[$i]}" == "$chosen" ]]; then
				account_id="${ids[$i]}"
				account_name=$(echo "$accounts_resp" | jq -r ".result[$i].name")
				break
			fi
		done
	fi
	ui_success "使用账号: $account_name"

	# 从 Cloudflare 列出用户的所有域名，让用户选择，无需手动输入完整域名
	ui_info "获取 Cloudflare Zone 列表..."
	local zone_count
	zone_count=$(count_cf_zones)
	if [[ "$zone_count" -eq 0 ]]; then
		ui_error "当前 Cloudflare 账号下没有域名"
		ui_info "请前往 https://dash.cloudflare.com 确认："
		ui_info "  1. 已添加域名（Add a site）并将 NS 指向 Cloudflare"
		ui_info "  2. 域名状态为 Active（Pending 需等待 NS 生效）"
		ui_info "  3. API Token 需包含 Zone > Zone: Read 权限（在 Token 详情页添加）"
		exit 1
	fi

	local zones_data zone_names=() zone_ids=() zone_name zone_id_entry
	while IFS=' ' read -r zone_name zone_id_entry; do
		[[ -z "$zone_name" ]] && continue
		zone_names+=("$zone_name")
		zone_ids+=("$zone_id_entry")
	done <<< "$(fetch_cf_zones)"

	local chosen_zone
	chosen_zone=$(_gum_choose --header "选择你要使用的域名" "${zone_names[@]}")
	local zone_id zone_idx
	for zone_idx in "${!zone_names[@]}"; do
		if [[ "${zone_names[$zone_idx]}" == "$chosen_zone" ]]; then
			zone_id="${zone_ids[$zone_idx]}"
			break
		fi
	done
	ui_success "使用域名: $chosen_zone"

	# 询问子域名前缀，留空则使用根域名
	local prefix
	prefix=$(_gum_input --placeholder "子域名前缀（留空则使用根域名 ${chosen_zone}）" --value "memos")

	local hostname
	if [[ -z "$prefix" ]]; then
		hostname="$chosen_zone"
	else
		hostname="${prefix}.${chosen_zone}"
	fi
	ui_info "将配置域名: $hostname"

	# 先收集并校验域名/Zone，全部通过后再创建 Tunnel，
	# 避免中途退出留下孤儿 Tunnel（tunnel 名含时间戳，重跑会累积）
	ui_info "创建 Cloudflare Tunnel..."
	local tunnel_name tunnel_resp tunnel_id tunnel_token
	tunnel_name="pathmemos-open-$(date +%s)"
	tunnel_resp=$(cf_api POST "/accounts/$account_id/cfd_tunnel" "{\"name\":\"$tunnel_name\",\"config_src\":\"cloudflare\"}")
	if ! echo "$tunnel_resp" | jq -e '.success' >/dev/null 2>&1; then
		local err_msg
		err_msg=$(echo "$tunnel_resp" | jq -r '.errors[0].message // "未知错误"')
		ui_error "创建 Tunnel 失败: $err_msg"
		exit 1
	fi
	tunnel_id=$(echo "$tunnel_resp" | jq -r '.result.id')
	tunnel_token=$(echo "$tunnel_resp" | jq -r '.result.token')
	ui_success "Tunnel 创建成功"

	ui_info "配置 Tunnel ingress -> http://nginx:80 ..."
	local config_resp
	config_resp=$(cf_api PUT "/accounts/$account_id/cfd_tunnel/$tunnel_id/configurations" "{\"config\":{\"ingress\":[{\"hostname\":\"${hostname}\",\"service\":\"http://nginx:80\",\"originRequest\":{}},{\"service\":\"http_status:404\"}]}}")
	if ! echo "$config_resp" | jq -e '.success' >/dev/null 2>&1; then
		local err_msg
		err_msg=$(echo "$config_resp" | jq -r '.errors[0].message // "未知错误"')
		ui_warn "配置 Tunnel ingress 失败: $err_msg"
	else
		ui_success "Tunnel ingress 配置完成"
	fi

	# DNS 记录名：子域名直接用前缀，根域名用 @
	local dns_name
	if [[ -z "$prefix" ]]; then
		dns_name="@"
	else
		dns_name="$prefix"
	fi

	ui_info "添加 DNS CNAME 记录..."
	local dns_resp
	dns_resp=$(cf_api POST "/zones/$zone_id/dns_records" "{\"type\":\"CNAME\",\"name\":\"${dns_name}\",\"content\":\"${tunnel_id}.cfargotunnel.com\",\"proxied\":true,\"comment\":\"PathMemos Open\"}")
	if ! echo "$dns_resp" | jq -e '.success' >/dev/null 2>&1; then
		local err_msg
		err_msg=$(echo "$dns_resp" | jq -r '.errors[0].message // "未知错误"')
		ui_error "添加 DNS 记录失败: $err_msg"
		ui_info "请手动添加 CNAME: ${hostname} -> ${tunnel_id}.cfargotunnel.com"
		exit 1
	fi
	ui_success "DNS 记录添加完成"

	local tunnel_domain="https://${hostname}"

	if grep -q "^CLOUDFLARE_TUNNEL_TOKEN=" .env; then
		sed -i "s|^CLOUDFLARE_TUNNEL_TOKEN=.*|CLOUDFLARE_TUNNEL_TOKEN=${tunnel_token}|" .env
	else
		echo "CLOUDFLARE_TUNNEL_TOKEN=${tunnel_token}" >> .env
	fi
	if grep -q "^API_HOST=" .env; then
		sed -i "s|^API_HOST=.*|API_HOST=${tunnel_domain}|" .env
	else
		echo "API_HOST=${tunnel_domain}" >> .env
	fi

	ui_success "公网地址: $tunnel_domain"
}

setup_cloudflare_tunnel_quick() {
	_gum_style_box "零域名临时隧道\n\n将使用 Cloudflare Quick Tunnel（trycloudflare.com）。\n优点：无需域名、无需 API Token。\n缺点：每次重启服务地址都会变化，不适合长期使用。\n\n此方式在重启服务器后会失效，建议跑完测试之后切换到「自定义域名」模式。"

	if grep -q "^CLOUDFLARE_TUNNEL_TOKEN=" .env; then
		sed -i "s|^CLOUDFLARE_TUNNEL_TOKEN=.*|CLOUDFLARE_TUNNEL_TOKEN=|" .env
	fi

	if grep -q "^API_HOST=" .env; then
		sed -i "s|^API_HOST=.*|API_HOST=|" .env
	else
		echo "API_HOST=" >> .env
	fi

	ui_info "已选择零域名临时隧道，将在启动容器后获取公网地址"
}
setup_cloudflare_tunnel() {
	cd "$PATHMEMOS_DIR"
	source .env

	if [[ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" && -n "${API_HOST:-}" && "$API_HOST" != "http://localhost:8080" && "$API_HOST" != "http://localhost" ]]; then
		if ! _gum_confirm "检测到已有 Cloudflare Tunnel 配置，是否重新创建？" --default no; then
			ui_info "跳过 Tunnel 创建，使用现有配置"
			return 0
		fi
	fi

	ui_section "Cloudflare Tunnel 自动配置"

	local mode
	mode=$(_gum_choose --header "选择公网入口方式" "自定义域名（固定地址）" "零域名临时隧道（trycloudflare.com，每次地址会变）")

	if [[ "$mode" == "自定义域名"* ]]; then
		setup_cloudflare_tunnel_custom_domain
	else
		setup_cloudflare_tunnel_quick
	fi
}
start_cloudflared() {
	cd "$PATHMEMOS_DIR"
	source .env

	ui_section "启动公网入口"

	if [[ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]]; then
		# 切换到正式部署：停掉临时测试的 Quick Tunnel 服务，避免双隧道并存
		run_as_root systemctl disable --now pathmemos-quicktunnel.service 2>/dev/null || true
		ui_info "启动 Cloudflare Tunnel 容器（自定义域名）..."
		if ! docker compose --profile tunnel up -d cloudflared; then
			ui_error "Tunnel 容器启动失败"
			ui_info "请检查: docker compose logs -f cloudflared"
			exit 1
		fi
		ui_success "Tunnel 容器已启动"
	else
		# 切换到临时测试：删除 compose 管理的 cloudflared 容器，避免双隧道并存
		docker compose --profile tunnel rm -sf cloudflared >/dev/null 2>&1 || true
		ui_info "启动 Cloudflare Quick Tunnel..."
		# 清理旧的运行方式残留：docker 容器（历史版本）与旧二进制进程，避免多实例并存
		docker rm -f pathmemos-cloudflared >/dev/null 2>&1 || true
		run_as_root systemctl stop pathmemos-quicktunnel 2>/dev/null || true
		run_as_root pkill -f "cloudflared-quick" 2>/dev/null || true
		# Quick Tunnel：不从 Docker Hub 拉镜像（国内常超时），直接下载二进制运行
		local cloudflared_bin="/usr/local/bin/cloudflared-quick"
		if ! command -v "$cloudflared_bin" >/dev/null 2>&1; then
			ui_info "下载 cloudflared 二进制..."
			local arch
			arch=$(uname -m)
			case "$arch" in
				x86_64) arch="amd64" ;;
				aarch64) arch="arm64" ;;
				*) arch="amd64" ;;
			esac
			local url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${arch}"
			if ! run_as_root timeout 60 curl -fsSL -o "$cloudflared_bin" "$url"; then
				ui_warn "GitHub 下载失败，尝试 Gitee 镜像..."
				local gitee_url="https://gitee.com/wowproton/path-memos/raw/open/deploy/assets/cloudflared-linux-${arch}"
				if ! run_as_root timeout 60 curl -fsSL -o "$cloudflared_bin" "$gitee_url"; then
					ui_error "下载 cloudflared 二进制失败"
					ui_info "请手动从 GitHub Releases 下载:"
					ui_info "  ${url}"
					exit 1
				fi
			fi
			run_as_root chmod +x "$cloudflared_bin"
		fi
		# 用 systemd 守护运行：崩溃自动重启、开机自启（trycloudflare 地址重启后会变化，
		# 需重跑本脚本或手动更新 .env 中的 API_HOST）
		run_as_root tee /etc/systemd/system/pathmemos-quicktunnel.service >/dev/null <<EOF
[Unit]
Description=PathMemos Cloudflare Quick Tunnel
After=network-online.target

[Service]
ExecStart=${cloudflared_bin} tunnel --url http://localhost:80 --no-autoupdate
Restart=always
RestartSec=5
StandardOutput=append:/var/log/pathmemos-quicktunnel.log
StandardError=append:/var/log/pathmemos-quicktunnel.log

[Install]
WantedBy=multi-user.target
EOF
		run_as_root rm -f /var/log/pathmemos-quicktunnel.log
		run_as_root systemctl daemon-reload
		if ! run_as_root systemctl enable --now pathmemos-quicktunnel >/dev/null 2>&1; then
			ui_error "Quick Tunnel 服务启动失败"
			ui_info "请检查: journalctl -u pathmemos-quicktunnel"
			exit 1
		fi
		ui_info "等待获取公网地址..."
		local api_host=""
		local i
		for i in $(seq 1 30); do
			api_host=$(run_as_root grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' /var/log/pathmemos-quicktunnel.log 2>/dev/null | head -1 || true)
			if [[ -n "$api_host" ]]; then
				break
			fi
			sleep 2
		done
		if [[ -z "$api_host" ]]; then
			run_as_root systemctl stop pathmemos-quicktunnel 2>/dev/null || true
			ui_error "未能获取 Quick Tunnel 公网地址"
			ui_info "请检查: cat /var/log/pathmemos-quicktunnel.log"
			exit 1
		fi
		ui_success "Quick Tunnel 已启动: $api_host"
		if grep -q "^API_HOST=" .env; then
			sed -i "s|^API_HOST=.*|API_HOST=${api_host}|" .env
		else
			echo "API_HOST=${api_host}" >> .env
		fi
		ui_success "公网地址: $api_host"
	fi

	ui_info "重新创建应用容器以应用新 API_HOST..."
	if ! docker compose up -d --build app nginx backup; then
		ui_error "应用重启失败"
		ui_info "请检查: docker compose logs -f app"
		exit 1
	fi
	ui_success "应用已重新创建"
}

health_check() {
	cd "$PATHMEMOS_DIR"
	source .env

	ui_section "健康检查"
	ui_info "等待应用就绪..."
	local i
	for i in $(seq 1 60); do
		if docker compose ps app | grep -q "(healthy)"; then
			break
		fi
		sleep 2
	done
	if ! docker compose ps app | grep -q "(healthy)"; then
		ui_error "应用未在 120 秒内就绪"
		ui_info "最近 50 行应用日志:"
		docker compose logs --tail 50 app || true
		ui_info "请检查: docker compose logs -f app"
		exit 1
	fi
	ui_success "应用已就绪"

	local api_host
	api_host=$(grep "^API_HOST=" .env | cut -d= -f2-)

	if [[ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]]; then
		ui_info "等待 Tunnel 容器启动..."
		for i in $(seq 1 30); do
			if docker compose ps cloudflared | grep -q "Up"; then
				break
			fi
			sleep 2
		done
		if ! docker compose ps cloudflared | grep -q "Up"; then
			ui_error "Tunnel 容器未在 60 秒内启动"
			ui_info "请检查: docker compose logs -f cloudflared"
			exit 1
		fi
		ui_success "Tunnel 容器已启动"

		if [[ -n "$api_host" && "$api_host" != http://localhost:* ]]; then
			ui_info "探测公网地址: ${api_host}/health ..."
			if curl -fsSL --retry 3 --retry-delay 2 "${api_host}/health" >/dev/null 2>&1; then
				ui_success "公网地址可访问: ${api_host}"
			else
				ui_warn "公网地址暂不可访问"
				ui_info "可能原因：DNS 未生效、本地网络拦截了该域名，或域名未解析到 Cloudflare"
			fi
		fi
	elif run_as_root systemctl is-active pathmemos-quicktunnel >/dev/null 2>&1; then
		ui_info "探测 Quick Tunnel 公网地址: ${api_host}/health ..."
		if curl -fsSL --retry 3 --retry-delay 2 "${api_host}/health" >/dev/null 2>&1; then
			ui_success "Quick Tunnel 可访问: ${api_host}"
		else
			ui_warn "Quick Tunnel 暂不可访问"
			ui_info "可能原因：本地 DNS 拦截了 trycloudflare.com，可尝试换网络或使用手机流量测试"
		fi
	fi

	ui_success "本地服务已启动: http://localhost"
}

show_summary() {
  cd "$PATHMEMOS_DIR"
  source .env

  local api_host log_cmd
  api_host=$(grep "^API_HOST=" .env | cut -d= -f2-)
  if [[ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]]; then
    log_cmd="docker compose logs -f cloudflared"
  else
    log_cmd="tail -f /var/log/pathmemos-quicktunnel.log"
  fi

  # 摘要固化到磁盘：SSH 断开或滚屏丢失后仍可随时找回
  {
    echo "后端地址: ${api_host}"
    echo "API Key: ${OPEN_API_KEY}"
    echo "生成时间: $(date '+%F %T')"
  } > "${PATHMEMOS_DIR}/install-info.txt" 2>/dev/null || true
  chmod 600 "${PATHMEMOS_DIR}/install-info.txt" 2>/dev/null || true

  # 用普通输出而非 gum 框：复制时不会带边框字符，且不依赖 /dev/tty
  ui_section "安装完成 🎉"
  ui_kv "后端地址" "${api_host}"
  ui_kv "API Key" "${OPEN_API_KEY}"
  ui_info "在小程序「个人中心 → 设置 → 后端配置」填写以上两项即可切换到开源版，管理自身全部数据"
  ui_info "以上信息已保存至 ${PATHMEMOS_DIR}/install-info.txt，可随时查看"
  ui_info "查看隧道日志: ${log_cmd}"
}

# ---------- 主流程 ----------
main() {
  # 防止在已被删除的目录中运行脚本（常见于 rm -rf pathmemos 后）
  if ! pwd >/dev/null 2>&1; then
    ui_warn "当前工作目录不存在，切换到 /tmp"
    cd /tmp || cd /
  fi

  ui_section "检查环境"
  require_root
  check_prerequisites
  create_swap_if_needed
  bootstrap_gum || true
  install_git
  ensure_jq
  install_docker

  ui_section "下载代码"
  clone_repo

  preflight_inputs
  run_init
  setup_cloudflare_tunnel
  start_cloudflared
  health_check
  show_summary
}
main "$@"
