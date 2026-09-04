#!/usr/bin/env bash
set -Eeuo pipefail

# Remove the Native and Docker installations created by this project.
# Credentials/configuration are retained unless --purge is explicitly used.
APP_DIR="/opt/wps-adapter"
ETC_DIR="/etc/wps-adapter"
SECRET_DIR="$ETC_DIR/secrets"
ENV_FILE="$ETC_DIR/wps-adapter.env"
HARDENING_ENV_FILE="$ETC_DIR/wps-adapter-hardening.env"
PID_FILE="$ETC_DIR/wps-adapter.pid"
LOG_FILE="$ETC_DIR/wps-adapter.log"
SERVICE_FILE="/etc/systemd/system/wps-adapter.service"
OVERRIDE_DIR="/etc/systemd/system/wps-adapter.service.d"
OVERRIDE_FILE="$OVERRIDE_DIR/override.conf"
WANTS_LINK="/etc/systemd/system/multi-user.target.wants/wps-adapter.service"
CONTAINER_NAME="wps-adapter"
IMAGE_NAME="wps-enterprise-adapter:latest"

PURGE=0
ASSUME_YES=0
REMOVE_IMAGE=0
SYSTEMD_RUNNING=0
DOCKER_READY=0
UNIT_MANAGED=0
CONTAINER_MANAGED=0
MANAGED_IMAGE_ID=""
INCOMPLETE=0
CURRENT_STEP=0
TOTAL_STEPS=4

die() {
    printf '卸载失败：%s\n' "$*" >&2
    exit 1
}

warn() {
    printf '警告：%s\n' "$*" >&2
}

progress_step() {
    ((CURRENT_STEP += 1))
    printf '\n[%d/%d] %s\n' "$CURRENT_STEP" "$TOTAL_STEPS" "$1"
}

has_command() {
    command -v "$1" >/dev/null 2>&1
}

usage() {
    cat <<'EOF'
用法：uninstall.sh [选项]

默认行为：
  停止并删除 WPS 适配器服务、应用文件和本项目管理的 Docker 容器。
  保留 /etc/wps-adapter/wps-adapter.env 和 /etc/wps-adapter/secrets/，
  这样以后重新安装时仍可继续使用原来的凭据。

选项：
  --purge        同时删除配置、Cookie、CSRF、Basic Auth 和工作区文件
  --remove-image 删除本项目 Docker 镜像 wps-enterprise-adapter:latest
  --yes          跳过确认提示；适合已经明确确认目标的自动化执行
  --help         显示帮助

安全行为：
  同名但没有本项目管理标记的 Docker 容器不会被删除。
  非本项目的 systemd 服务文件、符号链接和异常 PID 不会被强制处理。
  不会删除 WPS 云盘上的远端文件，也不会卸载 Docker 软件。
EOF
}

while (($# > 0)); do
    case "$1" in
        --purge) PURGE=1; shift ;;
        --remove-image) REMOVE_IMAGE=1; shift ;;
        --yes) ASSUME_YES=1; shift ;;
        --help|-h) usage; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done

[[ "${EUID:-$(id -u)}" == "0" ]] || die "请使用 root 运行，或在命令前加 sudo"

require_directory_target() {
    local path="$1"
    local label="$2"
    if [[ -L "$path" ]]; then
        die "$label 不能是符号链接：$path"
    fi
    if [[ -e "$path" && ! -d "$path" ]]; then
        die "$label 必须是目录：$path"
    fi
}

require_regular_target() {
    local path="$1"
    local label="$2"
    if [[ -L "$path" ]]; then
        die "$label 不能是符号链接：$path"
    fi
    if [[ -e "$path" && ! -f "$path" ]]; then
        die "$label 必须是普通文件：$path"
    fi
}

pid_is_adapter() {
    local pid="$1"
    [[ "$pid" =~ ^[0-9]+$ ]] || return 1
    [[ -r "/proc/$pid/cmdline" ]] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    local command_line
    command_line="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
    [[ "$command_line" == *wps_adapter* && "$command_line" == *" serve"* ]]
}

host_uses_systemd() {
    local init_name=""
    has_command systemctl || return 1
    [[ -r /proc/1/comm ]] || return 1
    IFS= read -r init_name </proc/1/comm || return 1
    [[ "$init_name" == "systemd" ]]
}

validate_unit() {
    if [[ -L "$SERVICE_FILE" || ! -f "$SERVICE_FILE" ]]; then
        die "发现异常的 systemd 服务文件，未执行卸载：$SERVICE_FILE"
    fi
    grep -Fqx 'Description=WPS enterprise cloud drive WebDAV adapter' "$SERVICE_FILE" \
        || die "systemd 服务文件不是本项目的，未执行卸载：$SERVICE_FILE"
    grep -Fqx 'WorkingDirectory=/opt/wps-adapter' "$SERVICE_FILE" \
        || die "systemd 服务文件不是本项目的，未执行卸载：$SERVICE_FILE"
    grep -Eq '^ExecStart=/[^[:space:]]*/python3(\.[0-9]+)* -m wps_adapter serve$' "$SERVICE_FILE" \
        || die "systemd 服务文件不是本项目的，未执行卸载：$SERVICE_FILE"
    grep -Fqx 'EnvironmentFile=-/etc/wps-adapter/wps-adapter.env' "$SERVICE_FILE" \
        || die "systemd 服务文件不是本项目的，未执行卸载：$SERVICE_FILE"
    grep -Fqx 'Environment=PYTHONPATH=/opt/wps-adapter/src' "$SERVICE_FILE" \
        || die "systemd 服务文件不是本项目的，未执行卸载：$SERVICE_FILE"
    UNIT_MANAGED=1
}

validate_targets() {
    require_directory_target "$APP_DIR" "应用目录"
    require_directory_target "$ETC_DIR" "配置目录"
    require_directory_target "$SECRET_DIR" "凭据目录"
    require_directory_target "$OVERRIDE_DIR" "systemd drop-in 目录"
    for path in \
        "$ENV_FILE" "$HARDENING_ENV_FILE" "$PID_FILE" "$LOG_FILE" \
        "$OVERRIDE_FILE"; do
        require_regular_target "$path" "安装文件"
    done
    if [[ -e "$SERVICE_FILE" || -L "$SERVICE_FILE" ]]; then
        validate_unit
    fi
    if [[ -f "$OVERRIDE_FILE" ]]; then
        grep -Fqx 'EnvironmentFile=-/etc/wps-adapter/wps-adapter-hardening.env' "$OVERRIDE_FILE" \
            || die "systemd drop-in 不是本项目的，未执行卸载：$OVERRIDE_FILE"
    fi
    if [[ -L "$WANTS_LINK" ]]; then
        local wants_target
        wants_target="$(readlink -- "$WANTS_LINK")"
        case "$wants_target" in
            ../wps-adapter.service|"$SERVICE_FILE") ;;
            *) die "systemd 启用链接指向未知目标，未执行卸载：$WANTS_LINK" ;;
        esac
    elif [[ -e "$WANTS_LINK" ]]; then
        die "systemd 启用路径不是符号链接，未执行卸载：$WANTS_LINK"
    fi
}

inspect_docker() {
    if ! has_command docker; then
        if (( REMOVE_IMAGE )); then
            die "未找到 Docker 命令，无法执行 --remove-image"
        fi
        return 0
    fi
    if ! docker info >/dev/null 2>&1; then
        die "无法连接 Docker daemon；为避免留下未清理的容器，请启动 Docker 后重新运行卸载脚本"
    fi
    DOCKER_READY=1
    if docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
        local managed
        managed="$(docker inspect -f '{{index .Config.Labels "com.galiandan.wps-adapter.managed"}}' "$CONTAINER_NAME")"
        [[ "$managed" == "true" ]] \
            || die "发现同名但不属于本项目的 Docker 容器：$CONTAINER_NAME"
        CONTAINER_MANAGED=1
        MANAGED_IMAGE_ID="$(docker inspect -f '{{.Image}}' "$CONTAINER_NAME")"
    fi
}

confirm() {
    (( ASSUME_YES )) && return 0
    [[ -r /dev/tty && -w /dev/tty ]] \
        || die "当前终端不能确认卸载；确认目标后请添加 --yes"
    printf '\n即将卸载 WPS 适配器。\n' > /dev/tty
    printf '将删除：服务、应用代码和本项目管理的 Docker 容器。\n' > /dev/tty
    if (( PURGE )); then
        printf '将额外删除：配置、Cookie、CSRF、Basic Auth 和工作区文件。\n' > /dev/tty
    else
        printf '将保留：%s 和其中的凭据，便于以后重新安装。\n' "$SECRET_DIR" > /dev/tty
    fi
    printf '确认请输入 YES：' > /dev/tty
    local answer
    IFS= read -r answer < /dev/tty || die "读取确认失败"
    [[ "$answer" == "YES" ]] || die "已取消卸载"
}

stop_portable_process() {
    [[ -f "$PID_FILE" ]] || return 0
    local pid=""
    IFS= read -r pid <"$PID_FILE" || true
    if [[ -n "$pid" ]] && pid_is_adapter "$pid"; then
        kill "$pid" || true
        for _ in {1..20}; do
            pid_is_adapter "$pid" || break
            sleep 0.25
        done
        pid_is_adapter "$pid" && kill -KILL "$pid" || true
    elif [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
        die "PID 文件指向的不是 WPS 适配器进程，未执行删除：$PID_FILE"
    fi
    rm -f -- "$PID_FILE"
}

stop_services() {
    if (( UNIT_MANAGED && SYSTEMD_RUNNING )); then
        systemctl stop wps-adapter.service \
            || die "无法停止 wps-adapter.service"
        systemctl disable wps-adapter.service >/dev/null 2>&1 || true
    fi
    stop_portable_process
    if (( CONTAINER_MANAGED )); then
        docker rm -f "$CONTAINER_NAME" >/dev/null \
            || die "无法删除 Docker 容器：$CONTAINER_NAME"
    fi
}

remove_service_and_app() {
    if (( UNIT_MANAGED )); then
        rm -f -- "$SERVICE_FILE"
    fi
    if [[ -L "$WANTS_LINK" ]]; then
        rm -f -- "$WANTS_LINK"
    fi
    if [[ -f "$OVERRIDE_FILE" ]]; then
        rm -f -- "$OVERRIDE_FILE"
    fi
    rmdir -- "$OVERRIDE_DIR" >/dev/null 2>&1 || true
    if [[ -f "$HARDENING_ENV_FILE" ]]; then
        rm -f -- "$HARDENING_ENV_FILE"
    fi
    if [[ -d "$APP_DIR" ]]; then
        rm -rf -- "$APP_DIR"
    fi
    rm -f -- "$LOG_FILE"
    if (( SYSTEMD_RUNNING )); then
        systemctl daemon-reload
    fi
}

remove_image() {
    (( REMOVE_IMAGE && DOCKER_READY )) || return 0
    if ! docker image inspect "$IMAGE_NAME" >/dev/null 2>&1; then
        return 0
    fi
    local image_id
    image_id="$(docker image inspect -f '{{.Id}}' "$IMAGE_NAME")"
    if [[ -n "$MANAGED_IMAGE_ID" && "$image_id" != "$MANAGED_IMAGE_ID" ]]; then
        warn "Docker 标签 $IMAGE_NAME 已指向其他镜像，保留该镜像以避免误删。"
        INCOMPLETE=1
        return 0
    fi
    if ! docker image rm "$IMAGE_NAME" >/dev/null; then
        warn "Docker 镜像仍被其他对象使用，未删除：$IMAGE_NAME"
        INCOMPLETE=1
    fi
}

remove_configuration() {
    if (( PURGE )); then
        if [[ -d "$ETC_DIR" ]]; then
            rm -rf -- "$ETC_DIR"
        fi
        return 0
    fi
    # Keep ENV_FILE and SECRET_DIR for a future reinstall. Remove only files
    # created solely for running the current installation.
    rmdir -- "$ETC_DIR" >/dev/null 2>&1 || true
}

progress_step "检查卸载目标和运行状态"
validate_targets
if host_uses_systemd; then
    SYSTEMD_RUNNING=1
fi
inspect_docker
confirm

progress_step "停止服务和容器"
stop_services

progress_step "删除服务和应用文件"
remove_service_and_app

progress_step "清理配置并完成卸载"
remove_image
remove_configuration

printf '\nWPS 适配器卸载完成。\n'
printf '已删除服务和应用文件。\n'
if (( PURGE )); then
    printf '已删除配置和凭据。\n'
else
    printf '已保留配置和凭据：%s\n' "$SECRET_DIR"
fi
if (( REMOVE_IMAGE )); then
    if (( INCOMPLETE )); then
        printf '部分 Docker 镜像未删除，请按上面的警告处理。\n' >&2
    else
        printf '已处理 Docker 镜像：%s\n' "$IMAGE_NAME"
    fi
fi

if (( INCOMPLETE )); then
    exit 1
fi
