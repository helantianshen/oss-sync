#!/usr/bin/env bash

set -Eeuo pipefail

SELF="$(readlink -f "$0" 2>/dev/null || printf '%s' "$0")"
CONTAINER="${OSS_CONTAINER_NAME:-}"

info() { printf '[OSS] %s\n' "$*"; }
fail() { printf '[OSS] 错误: %s\n' "$*" >&2; exit 1; }

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

ensure_docker() {
  command -v docker >/dev/null 2>&1 || fail "未检测到 Docker"
  if ! docker info >/dev/null 2>&1; then
    if [[ "$(id -u)" -ne 0 ]] && command -v sudo >/dev/null 2>&1; then
      exec sudo "$SELF" "$@"
    fi
    fail "Docker 守护进程不可用"
  fi
}

discover_container() {
  [[ -n "$CONTAINER" ]] && return
  mapfile -t containers < <(docker ps -a --filter label=io.oss-sync.managed=true --format '{{.Names}}')
  if ((${#containers[@]} > 1)); then
    fail "检测到多个 OSS Sync 容器，请设置 OSS_CONTAINER_NAME 后重试"
  fi
  CONTAINER="${containers[0]:-oss-sync}"
}

require_container() {
  docker inspect "$CONTAINER" >/dev/null 2>&1 || fail "未找到 OSS Sync 容器：$CONTAINER"
}

inspect() {
  docker inspect --format "$1" "$CONTAINER" 2>/dev/null || true
}

label() {
  local value
  value="$(inspect "{{index .Config.Labels \"$1\"}}")"
  [[ "$value" != "<no value>" ]] || value=""
  printf '%s' "$value"
}

current_port() { inspect '{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostPort}}'; }
current_bind() { inspect '{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostIp}}'; }
current_limit() { local value; value="$(label io.oss-sync.storage-limit-gb)"; printf '%s' "${value:-0}"; }
install_dir() { local value; value="$(label io.oss-sync.install-dir)"; printf '%s' "${value:-/opt/oss-sync}"; }
global_bin_dir() { local value; value="$(label io.oss-sync.global-bin-dir)"; printf '%s' "${value:-/usr/local/bin}"; }

show_status() {
  require_container
  local state health image port bind limit dir source used version proxy
  state="$(inspect '{{.State.Status}}')"
  health="$(inspect '{{if .State.Health}}{{.State.Health.Status}}{{else}}未配置{{end}}')"
  image="$(inspect '{{.Config.Image}}')"
  port="$(current_port)"
  bind="$(current_bind)"
  limit="$(current_limit)"
  dir="$(install_dir)"
  proxy="$(label io.oss-sync.release-proxy)"
  source="$(inspect '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Source}}{{end}}{{end}}')"
  used="$(du -sh "$source" 2>/dev/null | awk '{print $1}' || true)"
  if [[ -z "$used" && "$state" == "running" ]]; then
    used="$(docker exec "$CONTAINER" sh -c 'du -sh /app/data 2>/dev/null | cut -f1' 2>/dev/null || true)"
  fi
  version="$(docker exec "$CONTAINER" /app/oss-server --version 2>/dev/null || true)"
  printf '\nOSS Sync 状态\n'
  printf '  容器：%s\n' "$CONTAINER"
  printf '  状态：%s（健康检查：%s）\n' "$state" "$health"
  printf '  版本：%s\n' "${version:-容器未运行，无法读取}"
  printf '  镜像：%s\n' "$image"
  printf '  访问：http://%s:%s\n' "${bind:-0.0.0.0}" "$port"
  printf '  部署路径：%s\n' "$dir"
  printf '  数据路径：%s\n' "${source:-未知}"
  printf '  更新源：%s\n' "${proxy:-official}"
  printf '  已用空间：%s\n' "${used:-无法读取}"
  if [[ "$limit" == "0" ]]; then
    printf '  存储上限：不限\n\n'
  else
    printf '  存储上限：%s GiB\n\n' "$limit"
  fi
}

run_installer() {
  local mode="$1" port="$2" limit="$3" selected_proxy="${4:-}" dir installer bind base proxy image
  dir="$(install_dir)"
  installer="$dir/install.sh"
  [[ -r "$installer" ]] || fail "安装脚本不存在：$installer"
  bind="$(current_bind)"
  base="$(label io.oss-sync.release-base-url)"
  proxy="$(label io.oss-sync.release-proxy)"
  if [[ "$mode" == "update" ]]; then
    proxy="$selected_proxy"
    env OSS_CONTAINER_NAME="$CONTAINER" OSS_PORT="$port" OSS_BIND_ADDRESS="${bind:-0.0.0.0}" \
      OSS_INSTALL_DIR="$dir" OSS_STORAGE_LIMIT_GB="$limit" OSS_RELEASE_BASE_URL="${base:-https://github.com/helantianshen/oss-sync/releases/latest/download}" \
      OSS_RELEASE_PROXY="${proxy:-official}" OSS_GLOBAL_BIN_DIR="$(global_bin_dir)" \
      bash "$installer"
    return
  fi
  image="$(inspect '{{.Config.Image}}')"
  env OSS_CONTAINER_NAME="$CONTAINER" OSS_PORT="$port" OSS_BIND_ADDRESS="${bind:-0.0.0.0}" \
    OSS_INSTALL_DIR="$dir" OSS_STORAGE_LIMIT_GB="$limit" OSS_RELEASE_BASE_URL="${base:-https://github.com/helantianshen/oss-sync/releases/latest/download}" \
    OSS_RELEASE_PROXY="${proxy:-official}" OSS_GLOBAL_BIN_DIR="$(global_bin_dir)" OSS_IMAGE="$image" OSS_SKIP_PULL=1 \
    OSS_MANAGER_SOURCE="$SELF" OSS_INSTALLER_SOURCE="$installer" bash "$installer"
}

select_update_source() {
  local requested="${1:-}" custom="${2:-}" choice
  if [[ -z "$requested" ]]; then
    requested="${OSS_RELEASE_SOURCE:-}"
  fi
  if [[ -z "$requested" ]]; then
    requested="${OSS_RELEASE_PROXY:-}"
  fi
  if [[ -z "$requested" ]]; then
    choice="$(read_tty $'请选择本次更新源：\n  1. 加速地址（gh-proxy.com）\n  2. GitHub 官方地址\n  3. 自定义 HTTPS 地址前缀\n请选择 [1]：')"
    requested="${choice:-1}"
  fi

  case "$requested" in
    1|proxy)
      UPDATE_PROXY="https://gh-proxy.com/"
      ;;
    2|official)
      UPDATE_PROXY="official"
      ;;
    3|custom)
      if [[ -z "$custom" || "$custom" == "custom" || "$custom" == "proxy" || "$custom" == "official" ]]; then
        custom="$(read_tty '请输入自定义 HTTPS 地址前缀（例如 https://gh-proxy.com/）：')"
      fi
      [[ "$custom" =~ ^https:// ]] || fail "自定义更新源必须以 https:// 开头"
      UPDATE_PROXY="$custom"
      ;;
    https://*)
      UPDATE_PROXY="$requested"
      ;;
    http://*)
      fail "自定义更新源必须使用 HTTPS"
      ;;
    *)
      fail "更新源选项无效，请输入 1、2、3、official、proxy 或 HTTPS 地址"
      ;;
  esac
}

update_oss() {
  require_container
  local requested="${1:-}" custom="${2:-}"
  select_update_source "$requested" "$custom"
  info "本次更新使用源：$UPDATE_PROXY"
  run_installer update "$(current_port)" "$(current_limit)" "$UPDATE_PROXY"
}

uninstall_oss() {
  require_container
  local answer="${1:-}" dir source command_dir
  [[ -n "$answer" ]] || answer="$(read_tty '确认卸载 OSS Sync？项目数据将保留。[y/N] ')"
  [[ "$answer" =~ ^[Yy]([Ee][Ss])?$ ]] || { info "已取消卸载"; return; }
  dir="$(install_dir)"
  command_dir="$(global_bin_dir)"
  source="$(inspect '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Source}}{{end}}{{end}}')"
  docker rm -f "$CONTAINER" >/dev/null
  for command_path in "$command_dir/oss" "$command_dir/oss-sync"; do
    if [[ "$(readlink -f "$command_path" 2>/dev/null || true)" == "$SELF" ]]; then
      run_privileged rm -f "$command_path"
    fi
  done
  run_privileged rm -f "$dir/manage.sh" "$dir/install.sh"
  info "OSS Sync 已卸载，项目数据保留在 ${source:-$dir/data}"
}

change_storage() {
  require_container
  local value="${1:-}"
  [[ -n "$value" ]] || value="$(read_tty '请输入新的项目总存储上限 GiB（0 表示不限）：')"
  [[ "$value" =~ ^[0-9]+$ ]] && ((value <= 4096)) || fail "项目存储上限必须是 0-4096 的整数 GiB"
  run_installer local "$(current_port)" "$value"
}

change_port() {
  require_container
  local value="${1:-}"
  [[ -n "$value" ]] || value="$(read_tty '请输入新的映射端口：')"
  [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 1 && value <= 65535)) || fail "映射端口必须是 1-65535"
  run_installer local "$value" "$(current_limit)"
}

menu() {
  printf '\nOSS Sync 管理脚本\n'
  printf '  1，更新 oss-sync\n'
  printf '  2，卸载 oss-sync\n'
  printf '  3，查看状态\n'
  printf '  4，启动 oss-sync\n'
  printf '  5，停止 oss-sync\n'
  printf '  6，重启 oss-sync\n'
  printf '  7，修改项目存储空间\n'
  printf '  8，修改项目端口\n'
  printf '  0，退出脚本\n'
}

main() {
  ensure_docker "$@"
  discover_container
  local choice="${1:-}" value="${2:-}" interactive=0
  if [[ -z "$choice" ]]; then interactive=1; fi
  while true; do
    if ((interactive)); then
      menu
      choice="$(read_tty '请输入选项 [0-8]：')"
    fi
    case "$choice" in
      0) return ;;
      1) update_oss ;;
      2) uninstall_oss "$value"; return ;;
      3) show_status ;;
      4) require_container; docker start "$CONTAINER" >/dev/null; info "OSS Sync 已启动" ;;
      5) require_container; docker stop --time 20 "$CONTAINER" >/dev/null; info "OSS Sync 已停止" ;;
      6) require_container; docker restart --time 20 "$CONTAINER" >/dev/null; info "OSS Sync 已重启" ;;
      7) change_storage "$value" ;;
      8) change_port "$value" ;;
      *) fail "选项无效，请输入 0-8" ;;
    esac
    if ((!interactive)); then return 0; fi
    choice=""
    value=""
  done
}

main "$@"
