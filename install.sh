#!/usr/bin/env bash

set -Eeuo pipefail

IMAGE="${OSS_IMAGE:-}"
RELEASE_BASE_URL="${OSS_RELEASE_BASE_URL:-https://github.com/helantianshen/oss-sync/releases/latest/download}"
RELEASE_PROXY="${OSS_RELEASE_PROXY:-}"
DEFAULT_RELEASE_PROXY="https://gh-proxy.com/"
MANAGEMENT_BASE_URL="${OSS_MANAGEMENT_BASE_URL:-https://raw.githubusercontent.com/helantianshen/oss-sync/main}"
RELEASE_IMAGE="oss-sync-server:release"
CONTAINER="${OSS_CONTAINER_NAME:-oss-sync}"
VOLUME="${OSS_DATA_VOLUME:-oss-data}"
PORT="${OSS_PORT:-}"
BIND_ADDRESS="${OSS_BIND_ADDRESS:-0.0.0.0}"
INSTALL_DIR="${OSS_INSTALL_DIR:-}"
STORAGE_GB="${OSS_STORAGE_LIMIT_GB:-}"
INSTALL_DOCKER="${OSS_INSTALL_DOCKER:-}"
SKIP_PULL="${OSS_SKIP_PULL:-0}"
MANAGER_SOURCE="${OSS_MANAGER_SOURCE:-}"
INSTALLER_SOURCE="${OSS_INSTALLER_SOURCE:-}"
GLOBAL_BIN_DIR="${OSS_GLOBAL_BIN_DIR:-/usr/local/bin}"
TEMP_DIR=""

info() { printf '[OSS] %s\n' "$*"; }
fail() { printf '[OSS] 错误: %s\n' "$*" >&2; exit 1; }

cleanup() {
  [[ -z "$TEMP_DIR" ]] || rm -rf "$TEMP_DIR"
}

trap cleanup EXIT

read_tty() {
  local prompt="$1" result=""
  if { exec 3</dev/tty; } 2>/dev/null; then
    read -r -p "$prompt" result <&3 || true
    exec 3<&-
  fi
  printf '%s' "$result"
}

run_privileged() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
  elif "$@" 2>/dev/null; then
    return
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    fail "此操作需要 root 权限，请使用 sudo 重新运行"
  fi
}

confirm_docker_install() {
  case "${INSTALL_DOCKER,,}" in
    1|true|y|yes) return 0 ;;
    0|false|n|no) return 1 ;;
  esac
  local answer
  answer="$(read_tty '未检测到 Docker，是否使用 Docker 官方脚本安装？[y/N] ')"
  [[ -n "$answer" ]] || fail "未检测到 Docker；非交互安装请设置 OSS_INSTALL_DOCKER=1"
  [[ "$answer" =~ ^[Yy]([Ee][Ss])?$ ]]
}

select_release_source() {
  if [[ -n "$RELEASE_PROXY" ]]; then
    if [[ "$RELEASE_PROXY" == "official" ]]; then
      RELEASE_PROXY=""
    fi
    return 0
  fi
  [[ -n "$IMAGE" ]] && return 0
  local choice custom
  choice="$(read_tty $'请选择 OSS Sync Release 更新源：\n  1. gh-proxy.com 加速地址（推荐）\n  2. GitHub 官方地址\n  3. 自定义 HTTPS 地址前缀\n请选择 [1]：')"
  case "$choice" in
    ""|1) RELEASE_PROXY="$DEFAULT_RELEASE_PROXY" ;;
    2) RELEASE_PROXY="" ;;
    3)
      custom="$(read_tty '请输入自定义 HTTPS 地址前缀（例如 https://gh-proxy.com/）：')"
      [[ -n "$custom" ]] || fail "自定义加速前缀不能为空"
      [[ "$custom" =~ ^https:// ]] || fail "自定义更新源必须以 https:// 开头"
      RELEASE_PROXY="$custom"
      ;;
    *) fail "下载源选项无效" ;;
  esac
}

validate_release_source() {
  [[ -z "$RELEASE_PROXY" || "$RELEASE_PROXY" =~ ^https:// ]] || fail "更新源必须是 official 或 HTTPS 地址前缀"
}

download_release_image() {
  command -v curl >/dev/null 2>&1 || fail "下载 Release 需要 curl"
  command -v sha256sum >/dev/null 2>&1 || fail "校验 Release 需要 sha256sum"

  local arch asset archive checksums expected checksums_url download_url script script_url
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) fail "暂不支持的系统架构：$(uname -m)" ;;
  esac
  asset="oss-sync-image_linux_${arch}.tar.gz"
  TEMP_DIR="$(mktemp -d)"
  archive="$TEMP_DIR/$asset"
  checksums="$TEMP_DIR/checksums.txt"
  checksums_url="${RELEASE_BASE_URL%/}/checksums.txt"
  download_url="${RELEASE_BASE_URL%/}/$asset"
  if [[ -n "$RELEASE_PROXY" ]]; then
    # ponytail: blocked networks need both files proxied; use signed checksums if the proxy trust boundary becomes unacceptable.
    checksums_url="${RELEASE_PROXY%/}/$checksums_url"
    download_url="${RELEASE_PROXY%/}/$download_url"
  fi

  info "获取 Release 校验文件：$checksums_url"
  curl -fL --retry 3 --connect-timeout 15 "$checksums_url" -o "$checksums" || fail "下载 checksums.txt 失败"
  expected="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$checksums")"
  [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || fail "checksums.txt 中缺少 $asset"

  info "下载 OSS Sync Release 镜像：$download_url"
  curl -fL --retry 3 --connect-timeout 15 "$download_url" -o "$archive" || fail "下载 OSS Sync Release 失败"
  printf '%s  %s\n' "$expected" "$archive" | sha256sum -c - >/dev/null || fail "OSS Sync Release SHA-256 校验失败"
  for script in install.sh manage.sh; do
    expected="$(awk -v name="$script" '$2 == name || $2 == "*" name { print $1; exit }' "$checksums")"
    if [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]]; then
      script_url="${RELEASE_BASE_URL%/}/$script"
    else
      # ponytail: one-release compatibility fallback; new releases ship checksummed management scripts.
      script_url="${MANAGEMENT_BASE_URL%/}/$script"
    fi
    if [[ -n "$RELEASE_PROXY" ]]; then script_url="${RELEASE_PROXY%/}/$script_url"; fi
    info "下载管理组件：$script_url"
    curl -fL --retry 3 --connect-timeout 15 "$script_url" -o "$TEMP_DIR/$script" || fail "下载 $script 失败"
    if [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]]; then
      printf '%s  %s\n' "$expected" "$TEMP_DIR/$script" | sha256sum -c - >/dev/null || fail "$script SHA-256 校验失败"
    fi
  done
  docker load -i "$archive" >/dev/null || fail "导入 OSS Sync Release 镜像失败"
  docker image inspect "$RELEASE_IMAGE" >/dev/null 2>&1 || fail "Release 中缺少镜像 $RELEASE_IMAGE"
  IMAGE="$RELEASE_IMAGE"
  info "Release 校验通过并已导入 Docker"
}

install_managed_file() {
  local source="$1" destination="$2" temp
  if [[ "$(readlink -f "$source" 2>/dev/null || true)" == "$(readlink -f "$destination" 2>/dev/null || true)" ]]; then
    return
  fi
  temp="${destination}.tmp.$$"
  run_privileged install -m 0755 "$source" "$temp"
  run_privileged mv -f "$temp" "$destination"
}

install_command_link() {
  local destination="$1" current=""
  current="$(readlink -f "$destination" 2>/dev/null || true)"
  if [[ (-e "$destination" || -L "$destination") && "$current" != "$INSTALL_DIR/manage.sh" ]]; then
    fail "全局命令已存在且不属于 OSS Sync：$destination"
  fi
  run_privileged ln -sfn "$INSTALL_DIR/manage.sh" "$destination"
}

install_management_commands() {
  local script_dir="" local_installer="" local_manager=""
  if [[ -n "${BASH_SOURCE[0]:-}" && -r "${BASH_SOURCE[0]}" ]]; then
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local_installer="$script_dir/install.sh"
    local_manager="$script_dir/manage.sh"
  fi
  if [[ -z "$INSTALLER_SOURCE" ]]; then
    if [[ -n "$TEMP_DIR" && -r "$TEMP_DIR/install.sh" ]]; then INSTALLER_SOURCE="$TEMP_DIR/install.sh";
    elif [[ -r "$local_installer" ]]; then INSTALLER_SOURCE="$local_installer"; fi
  fi
  if [[ -z "$MANAGER_SOURCE" ]]; then
    if [[ -n "$TEMP_DIR" && -r "$TEMP_DIR/manage.sh" ]]; then MANAGER_SOURCE="$TEMP_DIR/manage.sh";
    elif [[ -r "$local_manager" ]]; then MANAGER_SOURCE="$local_manager"; fi
  fi
  if [[ -z "$INSTALLER_SOURCE" || -z "$MANAGER_SOURCE" ]]; then
    [[ -n "$TEMP_DIR" ]] || TEMP_DIR="$(mktemp -d)"
    command -v curl >/dev/null 2>&1 || fail "安装管理命令需要 curl"
    if [[ -z "$INSTALLER_SOURCE" ]]; then
      curl -fL --retry 3 --connect-timeout 15 "${MANAGEMENT_BASE_URL%/}/install.sh" -o "$TEMP_DIR/install.sh" || fail "下载 install.sh 失败"
      INSTALLER_SOURCE="$TEMP_DIR/install.sh"
    fi
    if [[ -z "$MANAGER_SOURCE" ]]; then
      curl -fL --retry 3 --connect-timeout 15 "${MANAGEMENT_BASE_URL%/}/manage.sh" -o "$TEMP_DIR/manage.sh" || fail "下载 manage.sh 失败"
      MANAGER_SOURCE="$TEMP_DIR/manage.sh"
    fi
  fi
  run_privileged mkdir -p "$INSTALL_DIR"
  [[ "$GLOBAL_BIN_DIR" == /* && "$GLOBAL_BIN_DIR" != "/" ]] || fail "全局命令目录必须是非根目录的绝对路径"
  [[ "$GLOBAL_BIN_DIR" != *$'\n'* && "$GLOBAL_BIN_DIR" != *$'\r'* ]] || fail "全局命令目录不能包含换行"
  run_privileged mkdir -p "$GLOBAL_BIN_DIR"
  install_managed_file "$INSTALLER_SOURCE" "$INSTALL_DIR/install.sh"
  install_managed_file "$MANAGER_SOURCE" "$INSTALL_DIR/manage.sh"
  install_command_link "$GLOBAL_BIN_DIR/oss-sync"
  install_command_link "$GLOBAL_BIN_DIR/oss"
}

select_port() {
  [[ -n "$PORT" ]] && return 0
  local existing="" requested="" candidate
  existing="$(docker inspect --format '{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostPort}}' "$CONTAINER" 2>/dev/null || true)"
  if [[ -n "$existing" && "$existing" != "<no value>" ]]; then
    PORT="$existing"
    return 0
  fi
  requested="$(read_tty '请输入映射端口（留空随机选择 10000-25565）：')"
  if [[ -n "$requested" ]]; then
    PORT="$requested"
    return 0
  fi
  while true; do
    candidate=$((10000 + RANDOM % 15566))
    case "$candidate" in
      10000|10050|10051|11211|12345|15672|18080|19000|20000|22000|25565) continue ;;
    esac
    PORT="$candidate"
    info "未指定端口，随机选择：$PORT"
    return 0
  done
}

select_install_dir() {
  [[ -n "$INSTALL_DIR" ]] && return 0
  local existing="" existing_data="" requested
  existing="$(docker inspect --format '{{index .Config.Labels "io.oss-sync.install-dir"}}' "$CONTAINER" 2>/dev/null || true)"
  if [[ -n "$existing" && "$existing" != "<no value>" ]]; then
    INSTALL_DIR="$existing"
    return 0
  fi
  existing_data="$(docker inspect --format '{{range .Mounts}}{{if and (eq .Destination "/app/data") (eq .Type "bind")}}{{.Source}}{{end}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
  if [[ "$existing_data" == */data ]]; then
    INSTALL_DIR="${existing_data%/data}"
    return 0
  fi
  requested="$(read_tty '请输入部署路径 [/opt/oss-sync]：')"
  INSTALL_DIR="${requested:-/opt/oss-sync}"
}

select_storage_limit() {
  [[ -n "$STORAGE_GB" ]] && return 0
  local existing="" requested
  existing="$(docker inspect --format '{{index .Config.Labels "io.oss-sync.storage-limit-gb"}}' "$CONTAINER" 2>/dev/null || true)"
  if [[ -n "$existing" && "$existing" != "<no value>" ]]; then
    STORAGE_GB="$existing"
    return 0
  fi
  requested="$(read_tty '请输入项目总存储上限 GiB（0 或留空表示不限）[0]：')"
  STORAGE_GB="${requested:-0}"
}

install_docker() {
  command -v curl >/dev/null 2>&1 || fail "安装 Docker 需要 curl"
  local installer
  installer="$(mktemp)"
  info "下载 Docker 官方安装脚本"
  if ! curl -fsSL https://get.docker.com -o "$installer" || ! run_privileged sh "$installer"; then
    rm -f "$installer"
    fail "Docker 安装失败"
  fi
  rm -f "$installer"
}

start_docker() {
  docker info >/dev/null 2>&1 && return 0
  if command -v systemctl >/dev/null 2>&1; then
    run_privileged systemctl enable --now docker || true
  elif command -v service >/dev/null 2>&1; then
    run_privileged service docker start || true
  fi
  docker info >/dev/null 2>&1 || fail "Docker 已安装但守护进程不可用，请启动 Docker 后重试"
}

restore_previous() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  if docker inspect "$CONTAINER-previous" >/dev/null 2>&1; then
    docker rename "$CONTAINER-previous" "$CONTAINER"
    docker start "$CONTAINER" >/dev/null
    info "新容器启动失败，已恢复原容器"
  fi
}

wait_healthy() {
  local status
  for ((attempt = 1; attempt <= 60; attempt++)); do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
    case "$status" in
      healthy) return 0 ;;
      exited|dead|unhealthy) return 1 ;;
    esac
    sleep 1
  done
  return 1
}

[[ "$(uname -s)" == "Linux" ]] || fail "一键安装脚本仅支持 Linux"
select_port
[[ "$PORT" =~ ^[0-9]+$ ]] && ((PORT >= 1 && PORT <= 65535)) || fail "映射端口必须是 1-65535"
[[ "$BIND_ADDRESS" == "127.0.0.1" || "$BIND_ADDRESS" == "0.0.0.0" ]] || fail "OSS_BIND_ADDRESS 仅支持 127.0.0.1 或 0.0.0.0"
[[ "$CONTAINER" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]+$ ]] || fail "OSS_CONTAINER_NAME 非法"
[[ "$VOLUME" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]+$ ]] || fail "OSS_DATA_VOLUME 非法"

if ! command -v docker >/dev/null 2>&1; then
  confirm_docker_install || fail "需要 Docker 才能继续安装"
  install_docker
fi
start_docker
select_release_source
validate_release_source
select_install_dir
select_storage_limit

[[ "$INSTALL_DIR" == /* && "$INSTALL_DIR" != "/" ]] || fail "部署路径必须是非根目录的绝对路径"
[[ "$INSTALL_DIR" != *$'\n'* && "$INSTALL_DIR" != *$'\r'* ]] || fail "部署路径不能包含换行"
[[ "$STORAGE_GB" =~ ^[0-9]+$ ]] && ((STORAGE_GB <= 4096)) || fail "项目存储上限必须是 0-4096 的整数 GiB"
STORAGE_MB=$((STORAGE_GB * 1024))

if [[ -z "$IMAGE" ]]; then
  download_release_image
elif [[ "$SKIP_PULL" == "1" ]]; then
  info "使用本地镜像 $IMAGE"
  docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "本地不存在镜像 $IMAGE"
else
  info "拉取自定义镜像 $IMAGE"
  docker pull "$IMAGE"
fi

install_management_commands

existing_mount_type="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Type}}{{end}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
existing_mount_source="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Name}}{{end}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
mount_args=()
if [[ "$existing_mount_type" == "volume" && -n "$existing_mount_source" ]]; then
  VOLUME="$existing_mount_source"
  docker volume create "$VOLUME" >/dev/null
  mount_args=(-v "${VOLUME}:/app/data")
  info "检测到旧版数据卷 $VOLUME，升级时继续使用，避免迁移风险"
else
  mkdir -p "$INSTALL_DIR/data" 2>/dev/null || run_privileged mkdir -p "$INSTALL_DIR/data"
  if [[ "$(stat -c '%u:%g' "$INSTALL_DIR/data")" != "10001:10001" ]]; then
    run_privileged chown 10001:10001 "$INSTALL_DIR/data"
  fi
  mount_args=(-v "${INSTALL_DIR}/data:/app/data")
fi

if docker inspect "$CONTAINER-previous" >/dev/null 2>&1; then
  docker rm -f "$CONTAINER-previous" >/dev/null
fi
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
  info "停止现有容器，项目数据保持不变"
  docker stop --time 20 "$CONTAINER" >/dev/null || true
  docker rename "$CONTAINER" "$CONTAINER-previous"
fi

if ! docker run -d \
  --name "$CONTAINER" \
  --restart unless-stopped \
  --stop-timeout 20 \
  --label org.opencontainers.image.source=https://github.com/helantianshen/oss-sync \
  --label io.oss-sync.managed=true \
  --label "io.oss-sync.install-dir=$INSTALL_DIR" \
  --label "io.oss-sync.storage-limit-gb=$STORAGE_GB" \
  --label "io.oss-sync.global-bin-dir=$GLOBAL_BIN_DIR" \
  --label "io.oss-sync.release-base-url=$RELEASE_BASE_URL" \
  --label "io.oss-sync.release-proxy=${RELEASE_PROXY:-official}" \
  -e "OSS_STORAGE_MAX_TOTAL_SIZE_MB=$STORAGE_MB" \
  -p "${BIND_ADDRESS}:${PORT}:8080" \
  "${mount_args[@]}" \
  --health-cmd 'wget -q -O /dev/null http://127.0.0.1:8080/readyz' \
  --health-interval 30s \
  --health-timeout 5s \
  --health-retries 3 \
  --health-start-period 10s \
  "$IMAGE" >/dev/null; then
  restore_previous
  fail "无法创建 OSS Sync 容器"
fi

info "等待服务健康检查"
if ! wait_healthy; then
  docker logs --tail 100 "$CONTAINER" >&2 || true
  restore_previous
  fail "服务未能健康启动"
fi

docker rm -f "$CONTAINER-previous" >/dev/null 2>&1 || true
info "OSS Sync 已启动，部署路径：$INSTALL_DIR"
if ((STORAGE_GB > 0)); then
  info "项目总存储上限：${STORAGE_GB} GiB"
else
  info "项目总存储上限：不限"
fi
if [[ "$BIND_ADDRESS" == "0.0.0.0" ]]; then
  info "访问地址：http://<服务器IP>:$PORT"
else
  info "访问地址：http://127.0.0.1:$PORT"
fi
info "请及时注册为管理员确保安全"
info "管理命令已安装：oss 或 oss-sync"
