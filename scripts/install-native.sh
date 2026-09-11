#!/usr/bin/env bash
set -Eeuo pipefail

# One-command native installer. It uses systemd when the host provides it and
# falls back to a portable background process on systems without systemd.
REPOSITORY="https://github.com/galiandan/WPS_2_WebDAV"
# This is deliberately an immutable commit, updated by the release process.
SOURCE_REF="${WPS_ADAPTER_SOURCE_REF:-49e641b2c7cdc0d9cb5b7b5a64474243094cca96}"
# Release assets are preferred. The source path below remains the complete
# fallback for hosts that cannot reach the binary mirror or have an unsupported
# prebuilt architecture.
BINARY_RELEASE_TAG="${WPS_ADAPTER_BINARY_RELEASE_TAG:-v1.0.6}"
BINARY_BASE_URL="${WPS_ADAPTER_BINARY_BASE_URL:-}"
# The service is built with a fixed toolchain only when the host does not
# already provide a compatible Go compiler. The toolchain stays in the
# installer's private temporary directory and is never installed system-wide.
GO_VERSION="1.25.0"
DEFAULT_APP_DIR="/opt/wps-adapter"
LEGACY_CONFIG_DIR="/etc/wps-adapter"
LEGACY_SECRET_DIR="$LEGACY_CONFIG_DIR/secrets"
LEGACY_ENV_FILE="$LEGACY_CONFIG_DIR/wps-adapter.env"
CURRENT_DIR="$(pwd -P 2>/dev/null || true)"
case "$CURRENT_DIR" in
    ""|/|/root|/home|/home/*|/tmp|/var/tmp|/opt|/usr|/var|/etc)
        APP_DIR="${WPS_ADAPTER_DIR:-$DEFAULT_APP_DIR}"
        ;;
    *)
        APP_DIR="${WPS_ADAPTER_DIR:-$CURRENT_DIR}"
        ;;
esac
CONFIG_DIR="$APP_DIR/config"
SECRET_DIR="$CONFIG_DIR/secrets"
DATA_DIR="$APP_DIR/data"
RESUME_DIR="$DATA_DIR/uploads"
ENV_FILE="$CONFIG_DIR/wps-adapter.env"
RUNTIME_DIR="$APP_DIR/runtime"
LOG_DIR="$APP_DIR/logs"
HARDENING_ENV_FILE="$CONFIG_DIR/wps-adapter-hardening.env"
ENV_SOURCE_FILE="$ENV_FILE"
[[ -f "$ENV_SOURCE_FILE" ]] || ENV_SOURCE_FILE="$LEGACY_ENV_FILE"

PORT_ARG=""
BIND_ARG=""
GROUP_ID_ARG=""
ROOT_ID_ARG=""
ADAPTER_USER_ARG=""
RUN_USER_ARG=""

TOTAL_STEPS=8
CURRENT_STEP=0
PACKAGE_MANAGER=""
DOWNLOAD_CONNECT_TIMEOUT="${WPS_ADAPTER_DOWNLOAD_CONNECT_TIMEOUT:-10}"
DOWNLOAD_MAX_TIME="${WPS_ADAPTER_DOWNLOAD_MAX_TIME:-300}"
PID_FILE="$DATA_DIR/wps-adapter.pid"
LOG_FILE="$LOG_DIR/wps-adapter.log"
SERVICE_FILE="/etc/systemd/system/wps-adapter.service"
OVERRIDE_FILE="/etc/systemd/system/wps-adapter.service.d/override.conf"
OVERRIDE_DIR="/etc/systemd/system/wps-adapter.service.d"
SERVICE_MODE="direct"
USE_PREBUILT=0
BINARY_ASSET=""
BINARY_FILE=""

die() {
    printf '安装失败：%s\n' "$*" >&2
    exit 1
}

[[ "$APP_DIR" == /* && "$APP_DIR" != "/" ]] || die "部署目录必须是非根目录的绝对路径"

progress_step() {
    ((CURRENT_STEP += 1))
    printf '\n[%d/%d] %s\n' "$CURRENT_STEP" "$TOTAL_STEPS" "$1"
}

has_command() {
    command -v "$1" >/dev/null 2>&1
}

package_manager_busy() {
    local comm_path process_name
    for comm_path in /proc/[0-9]*/comm; do
        [[ -r "$comm_path" ]] || continue
        IFS= read -r process_name <"$comm_path" || continue
        case "$process_name" in
            apt|apt-get|dpkg|dpkg-deb|unattended-upgrade|packagekitd|dnf|microdnf|tdnf|yum|apk|pacman|zypper|xbps-install)
                return 0
                ;;
        esac
    done
    return 1
}

detect_package_manager() {
    if has_command apt-get; then PACKAGE_MANAGER="apt"; return 0; fi
    if has_command dnf; then PACKAGE_MANAGER="dnf"; return 0; fi
    if has_command microdnf; then PACKAGE_MANAGER="microdnf"; return 0; fi
    if has_command tdnf; then PACKAGE_MANAGER="tdnf"; return 0; fi
    if has_command yum; then PACKAGE_MANAGER="yum"; return 0; fi
    if has_command apk; then PACKAGE_MANAGER="apk"; return 0; fi
    if has_command pacman; then PACKAGE_MANAGER="pacman"; return 0; fi
    if has_command zypper; then PACKAGE_MANAGER="zypper"; return 0; fi
    if has_command xbps-install; then PACKAGE_MANAGER="xbps"; return 0; fi
    return 1
}

package_install() {
    (($# > 0)) || return 0
    [[ -n "$PACKAGE_MANAGER" ]] || detect_package_manager \
        || die "缺少安装依赖，且未识别 apt、dnf、yum、apk、pacman、zypper 或 xbps-install"
    package_manager_busy && die "检测到其他软件包管理进程正在运行，请先让它完成后再安装"
    case "$PACKAGE_MANAGER" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -o Acquire::Retries=2 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15
            apt-get install -y -o Acquire::Retries=2 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15 "$@"
            ;;
        dnf) dnf -y --setopt=retries=2 --setopt=timeout=15 install "$@" ;;
        microdnf) microdnf install -y "$@" ;;
        tdnf) tdnf install -y "$@" ;;
        yum) yum -y --setopt=retries=2 --setopt=timeout=15 install "$@" ;;
        apk) apk add --no-cache "$@" ;;
        pacman) pacman --noconfirm -Sy --needed "$@" ;;
        zypper)
            zypper --non-interactive --gpg-auto-import-keys refresh
            zypper --non-interactive install --no-recommends "$@"
            ;;
        xbps) xbps-install -Sy "$@" ;;
    esac
}

install_native_dependencies() {
    local transport=""
    local find_package=""
    if ! has_command curl && ! has_command wget; then
        transport="curl"
    fi
    if ! has_command find; then
        find_package="findutils"
    fi
    package_install ca-certificates tar coreutils $find_package $transport
}

go_version_at_least_125() {
    local binary="$1"
    local version major minor
    # `go version` is: go version go1.25.0 linux/amd64.  Read the
    # toolchain token (field 3), not the platform token (field 4).
    version="$("$binary" version 2>/dev/null | awk 'NR == 1 { print $3 }')"
    [[ "$version" =~ ^go([0-9]+)\.([0-9]+)(\.[0-9]+)?([a-zA-Z.-].*)?$ ]] || return 1
    major="${BASH_REMATCH[1]}"
    minor="${BASH_REMATCH[2]}"
    ((10#${major} > 1 || (10#${major} == 1 && 10#${minor} >= 25)))
}

find_go() {
    local candidate
    for candidate in go /usr/local/go/bin/go; do
        if [[ "$candidate" == */* ]]; then
            [[ -x "$candidate" ]] || continue
        else
            command -v "$candidate" >/dev/null 2>&1 || continue
        fi
        if go_version_at_least_125 "$candidate"; then
            if [[ "$candidate" == */* ]]; then
                printf '%s\n' "$candidate"
            else
                command -v "$candidate"
            fi
            return 0
        fi
    done
    return 1
}

linux_go_target() {
    GOARCH=""
    GOARM=""
    GO_ARCHIVE=""
    case "$(uname -m)" in
        x86_64|amd64) GOARCH="amd64"; GO_ARCHIVE="amd64" ;;
        aarch64|arm64) GOARCH="arm64"; GO_ARCHIVE="arm64" ;;
        armv6l|armv7l|armv8l) GOARCH="arm"; GOARM="6"; GO_ARCHIVE="armv6l" ;;
        i386|i686) GOARCH="386"; GO_ARCHIVE="386" ;;
        ppc64le) GOARCH="ppc64le"; GO_ARCHIVE="ppc64le" ;;
        riscv64) GOARCH="riscv64"; GO_ARCHIVE="riscv64" ;;
        s390x) GOARCH="s390x"; GO_ARCHIVE="s390x" ;;
        *) die "不支持的 Linux CPU 架构：$(uname -m)；请提供 Go >= 1.25 或使用 amd64/arm64 VPS" ;;
    esac
}

linux_binary_target() {
    BINARY_ASSET=""
    case "$(uname -m)" in
        x86_64|amd64) BINARY_ASSET="amd64" ;;
        aarch64|arm64) BINARY_ASSET="arm64" ;;
        armv7l|armv8l) BINARY_ASSET="armv7" ;;
        armv6l) BINARY_ASSET="armv6" ;;
        i386|i686) BINARY_ASSET="386" ;;
        ppc64le) BINARY_ASSET="ppc64le" ;;
        riscv64) BINARY_ASSET="riscv64" ;;
        s390x) BINARY_ASSET="s390x" ;;
        *) return 1 ;;
    esac
}

download_file() {
    local url="$1"
    local target="$2"
    local max_filesize="${3:-52428800}"
    case "$url" in
        https://*) ;;
        *)
            printf '拒绝非 HTTPS 下载地址。\n' >&2
            return 1
            ;;
    esac
    if has_command curl; then
        curl --fail --show-error --progress-bar --location --max-filesize "$max_filesize" \
            --connect-timeout "$DOWNLOAD_CONNECT_TIMEOUT" --max-time "$DOWNLOAD_MAX_TIME" \
            --proto-redir '=https' --proto '=https' --tlsv1.2 \
            "$url" -o "$target"
    elif has_command wget; then
        if has_command timeout; then
            timeout "$DOWNLOAD_MAX_TIME" \
                wget -T "$DOWNLOAD_CONNECT_TIMEOUT" -t 1 -O "$target" "$url"
        else
            wget -T "$DOWNLOAD_CONNECT_TIMEOUT" -t 1 -O "$target" "$url"
        fi
    else
        die "缺少 curl 或 wget，无法下载项目归档"
    fi
}

download_go_toolchain() {
    linux_go_target
    local filename="go${GO_VERSION}.linux-${GO_ARCHIVE}.tar.gz"
    local archive="$TMP_DIR/$filename"
    local url="${WPS_ADAPTER_GO_URL:-https://mirrors.aliyun.com/golang/$filename}"
    printf '下载 Go 工具链：%s\n' "$url"
    download_file "$url" "$archive" 157286400 \
        || die "Go ${GO_VERSION} 工具链下载失败；可先在主机安装 Go >= 1.25 后重试"
    GO_BIN="$TMP_DIR/go/bin/go"
    tar -xzf "$archive" -C "$TMP_DIR" \
        || die "Go ${GO_VERSION} 工具链解压失败"
    [[ -x "$GO_BIN" ]] || die "Go 工具链解压后不可用"
}

download_prebuilt_binary() {
    linux_binary_target || {
        printf '提示：当前 Linux CPU 架构没有对应预编译二进制。\n' >&2
        return 1
    }
    local base_url="${BINARY_BASE_URL:-https://ghfast.top/$REPOSITORY/releases/download/$BINARY_RELEASE_TAG}"
    base_url="${base_url%/}"
    local url="$base_url/wps-adapter-linux-$BINARY_ASSET"
    local partial="$TMP_DIR/wps-adapter.binary.part"
    BINARY_FILE="$TMP_DIR/wps-adapter"
    printf '尝试下载预编译二进制（Linux/%s）：%s\n' "$BINARY_ASSET" "$url"
    rm -f -- "$partial" "$BINARY_FILE"
    if ! download_file "$url" "$partial" 52428800; then
        rm -f -- "$partial"
        return 1
    fi
    chmod 755 "$partial"
    if ! "$partial" --version >/dev/null 2>&1; then
        printf '提示：下载的预编译文件无法执行。\n' >&2
        rm -f -- "$partial"
        return 1
    fi
    mv -f -- "$partial" "$BINARY_FILE"
    return 0
}

download_archive() {
    local direct_url="$REPOSITORY/archive/$SOURCE_REF.tar.gz"
    local url="${WPS_ADAPTER_ARCHIVE_URL:-https://ghfast.top/$direct_url}"
    printf '下载源代码：%s\n' "$url"
    download_file "$url" "$ARCHIVE" \
        || die "项目归档下载失败"
    tar -tzf "$ARCHIVE" >/dev/null 2>&1 \
        || die "项目归档不是可读取的 tar.gz 文件"
}

health_check() {
    local url="$1"
    if has_command curl; then
        curl --fail --silent --show-error --max-time 8 "$url" >/dev/null
    elif has_command wget; then
        wget -q -T 8 -O - "$url" >/dev/null
    else
        return 1
    fi
}

archive_members_are_safe() {
    local archive="$1"
    local member
    # Names alone do not reveal symlinks, hardlinks, devices, or FIFOs.  Do
    # this check before extraction so a malicious archive cannot influence
    # extraction through a special entry.
    tar -tvzf "$archive" | awk '
        { type = substr($1, 1, 1); if (type != "-" && type != "d") bad = 1 }
        END { exit bad }
    ' || return 1
    while IFS= read -r member; do
        member="${member%/}"
        [[ -z "$member" ]] && continue
        [[ "$member" != /* ]] || return 1
        [[ "$member" != ".." && "$member" != ../* && "$member" != */../* && "$member" != */.. ]] \
            || return 1
        [[ "$member" != *//* ]] || return 1
    done < <(tar -tzf "$archive")
}

pid_is_adapter() {
    local pid="$1"
    [[ "$pid" =~ ^[0-9]+$ ]] || return 1
    [[ -r "/proc/$pid/cmdline" ]] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    local command_line
    command_line="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
    [[ "$command_line" == *"wps-adapter"* && "$command_line" == *" serve"* ]] \
        || [[ "$command_line" == *"wps_adapter"* && "$command_line" == *" serve"* ]]
}

host_uses_systemd() {
    local init_name=""
    has_command systemctl || return 1
    [[ -r /proc/1/comm ]] || return 1
    IFS= read -r init_name </proc/1/comm || return 1
    [[ "$init_name" == "systemd" ]]
}

direct_service_active() {
    local pid=""
    [[ -r "$PID_FILE" ]] || return 1
    read -r pid <"$PID_FILE" || return 1
    pid_is_adapter "$pid"
}

service_is_active() {
    if [[ "$SERVICE_MODE" == "systemd" ]]; then
        systemctl is-active --quiet wps-adapter.service
    else
        direct_service_active
    fi
}

service_is_enabled() {
    # A missing unit is expected during a first install; do not show it as an error.
    [[ "$SERVICE_MODE" == "systemd" ]] && systemctl is-enabled --quiet wps-adapter.service 2>/dev/null
}

service_stop() {
    if [[ "$SERVICE_MODE" == "systemd" ]]; then
        systemctl stop wps-adapter.service
        return
    fi
    local pid=""
    [[ -r "$PID_FILE" ]] || return 0
    read -r pid <"$PID_FILE" || true
    if [[ -n "$pid" ]] && pid_is_adapter "$pid"; then
        kill "$pid" || true
        for _ in {1..20}; do
            pid_is_adapter "$pid" || break
            sleep 0.25
        done
        pid_is_adapter "$pid" && kill -KILL "$pid" || true
    fi
    rm -f -- "$PID_FILE"
}

service_start() {
    if [[ "$SERVICE_MODE" == "systemd" ]]; then
        systemctl start wps-adapter.service
        return
    fi
    mkdir -p -- "$(dirname "$PID_FILE")"
    : >"$LOG_FILE"
    chown "$RUN_USER:$RUN_GROUP" "$LOG_FILE"
    local launcher='set -a; . "$1"; set +a; exec "$2/wps-adapter" serve'
    if [[ "$(id -u)" == "$RUN_UID" ]]; then
        nohup bash -c "$launcher" -- "$ENV_FILE" "$RUNTIME_DIR" \
            >>"$LOG_FILE" 2>&1 < /dev/null &
    elif has_command runuser; then
        nohup runuser -u "$RUN_USER" -- bash -c "$launcher" -- "$ENV_FILE" "$RUNTIME_DIR" \
            >>"$LOG_FILE" 2>&1 < /dev/null &
    elif has_command su; then
        nohup su -s /bin/sh "$RUN_USER" -c "$launcher" -- "$ENV_FILE" "$RUNTIME_DIR" \
            >>"$LOG_FILE" 2>&1 < /dev/null &
    else
        die "当前系统没有 systemd、runuser 或 su，无法以指定服务用户启动适配器"
    fi
    printf '%s\n' "$!" >"$PID_FILE"
    chown "$RUN_USER:$RUN_GROUP" "$PID_FILE"
}

service_reload() {
    [[ "$SERVICE_MODE" == "systemd" ]] || return 0
    systemctl daemon-reload
}

service_enable() {
    [[ "$SERVICE_MODE" == "systemd" ]] || return 0
    systemctl enable wps-adapter.service
}

service_disable() {
    [[ "$SERVICE_MODE" == "systemd" ]] || return 0
    systemctl disable wps-adapter.service
}

usage() {
    cat <<'EOF'
用法：install-native.sh [选项]

选项：
  --port PORT          适配器监听端口，默认 54321
  --bind ADDRESS      监听地址，默认 0.0.0.0
  --group-id ID       WPS 群组 ID（可选，默认自动识别）
  --root-id ID        WPS 根目录 ID（可选，默认自动识别）
  --adapter-user USER 适配器 Basic Auth 用户名
  --run-user USER      服务运行用户，默认执行 sudo 的当前用户
  --source-ref SHA     要安装的 40 位 Git 提交号（默认使用脚本内固定版本）
  --help              显示帮助

环境变量：
  WPS_ADAPTER_DIR                    自定义部署目录；未设置时按当前 pwd 选择
  WPS_ADAPTER_ARCHIVE_URL              自定义项目归档 HTTPS 地址
  WPS_ADAPTER_BINARY_BASE_URL          预编译二进制目录 HTTPS 地址
  WPS_ADAPTER_BINARY_RELEASE_TAG       预编译二进制 Release 标签，默认 v1.0.6
  WPS_ADAPTER_GO_URL                   自定义 Go 工具链 HTTPS 地址
  WPS_ADAPTER_DOWNLOAD_CONNECT_TIMEOUT 下载连接超时秒数，默认 10
  WPS_ADAPTER_DOWNLOAD_MAX_TIME        单个地址总超时秒数，默认 300

Native 构建：
  优先下载对应架构的静态预编译二进制；失败后才使用主机 Go 1.25+，或下载 Go 1.25.0 和源码现场编译。

适配器密码不会作为命令行参数接受；首次安装时会隐藏输入。
EOF
}

while (($# > 0)); do
    case "$1" in
        --port)
            (($# >= 2)) || die "--port 缺少参数"
            PORT_ARG="$2"
            shift 2
            ;;
        --port=*) PORT_ARG="${1#*=}"; shift ;;
        --bind)
            (($# >= 2)) || die "--bind 缺少参数"
            BIND_ARG="$2"
            shift 2
            ;;
        --bind=*) BIND_ARG="${1#*=}"; shift ;;
        --group-id)
            (($# >= 2)) || die "--group-id 缺少参数"
            GROUP_ID_ARG="$2"
            shift 2
            ;;
        --group-id=*) GROUP_ID_ARG="${1#*=}"; shift ;;
        --root-id)
            (($# >= 2)) || die "--root-id 缺少参数"
            ROOT_ID_ARG="$2"
            shift 2
            ;;
        --root-id=*) ROOT_ID_ARG="${1#*=}"; shift ;;
        --adapter-user)
            (($# >= 2)) || die "--adapter-user 缺少参数"
            ADAPTER_USER_ARG="$2"
            shift 2
            ;;
        --adapter-user=*) ADAPTER_USER_ARG="${1#*=}"; shift ;;
        --run-user)
            (($# >= 2)) || die "--run-user 缺少参数"
            RUN_USER_ARG="$2"
            shift 2
            ;;
        --run-user=*) RUN_USER_ARG="${1#*=}"; shift ;;
        --source-ref)
            (($# >= 2)) || die "--source-ref 缺少参数"
            SOURCE_REF="$2"
            shift 2
            ;;
        --source-ref=*) SOURCE_REF="${1#*=}"; shift ;;
        --help|-h) usage; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done

[[ "${EUID:-$(id -u)}" == "0" ]] || die "请使用 root 运行，或在命令前加 sudo"

[[ "$DOWNLOAD_CONNECT_TIMEOUT" =~ ^[1-9][0-9]*$ ]] \
    || die "WPS_ADAPTER_DOWNLOAD_CONNECT_TIMEOUT 必须是正整数"
[[ "$DOWNLOAD_MAX_TIME" =~ ^[1-9][0-9]*$ ]] \
    || die "WPS_ADAPTER_DOWNLOAD_MAX_TIME 必须是正整数"

progress_step "检查运行环境和安装参数"

GO_BIN="$(find_go || true)"
if ! has_command curl || ! has_command tar \
    || ! has_command find; then
    detect_package_manager || die "缺少安装依赖，且未识别 apt、dnf、yum、apk、pacman、zypper 或 xbps-install"
    install_native_dependencies
fi
has_command curl || has_command wget || die "缺少 curl 或 wget，无法下载项目归档"
has_command tar || die "缺少 tar，无法解压项目归档"

if host_uses_systemd; then
    SERVICE_MODE="systemd"
else
    SERVICE_MODE="direct"
    printf '提示：当前系统不是 systemd，将使用便携后台模式；安装完成后不会自动注册开机启动。\n'
fi

if [[ -n "$RUN_USER_ARG" ]]; then
    RUN_USER="$RUN_USER_ARG"
elif [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
    RUN_USER="$SUDO_USER"
else
    RUN_USER="${USER:-root}"
fi
[[ "$RUN_USER" =~ ^[A-Za-z_][A-Za-z0-9_.-]*[$]?$ ]] || die "服务运行用户格式不正确"
id "$RUN_USER" >/dev/null 2>&1 || die "服务运行用户不存在：$RUN_USER"
RUN_GROUP="$(id -gn "$RUN_USER")"
RUN_UID="$(id -u "$RUN_USER")"

TTY="/dev/tty"
ask_value() {
    local prompt="$1"
    local default_value="${2:-}"
    [[ -r "$TTY" && -w "$TTY" ]] || die "当前终端不能进行交互输入，请通过参数提供配置"
    if [[ -n "$default_value" ]]; then
        printf '%s [%s]: ' "$prompt" "$default_value" >"$TTY"
    else
        printf '%s: ' "$prompt" >"$TTY"
    fi
    IFS= read -r REPLY <"$TTY" || die "读取终端输入失败"
    REPLY="${REPLY:-$default_value}"
}

ask_secret() {
    local prompt="$1"
    [[ -r "$TTY" && -w "$TTY" ]] || die "当前终端不能进行交互输入"
    printf '%s: ' "$prompt" >"$TTY"
    IFS= read -r -s REPLY <"$TTY" || die "读取终端输入失败"
    printf '\n' >"$TTY"
}

read_env_value() {
    local key="$1"
    local source_file="$ENV_SOURCE_FILE"
    [[ -f "$source_file" ]] || return 0
    awk -v key="$key" '
        $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
            value = $0
            sub(/^[^=]*=/, "", value)
            sub(/^"/, "", value)
            sub(/"$/, "", value)
            print value
            exit
        }
    ' "$source_file"
}

set_env_value() {
    local key="$1"
    local value="$2"
    local target="${ENV_TARGET_FILE:-$ENV_FILE}"
    local temporary
    temporary="$(mktemp)"
    if [[ -f "$target" ]]; then
        awk -v key="$key" -v value="$value" '
            BEGIN { found = 0 }
            $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
                print key "=" value
                found = 1
                next
            }
            { print }
            END { if (!found) print key "=" value }
        ' "$target" >"$temporary"
    else
        printf '%s=%s\n' "$key" "$value" >"$temporary"
    fi
    chmod 600 "$temporary"
    mv -f "$temporary" "$target"
}

remove_env_value() {
    local key="$1"
    local target="${ENV_TARGET_FILE:-$ENV_FILE}"
    local temporary
    [[ -f "$target" ]] || return 0
    temporary="$(mktemp)"
    awk -v key="$key" '$0 ~ "^[[:space:]]*" key "[[:space:]]*=" { next } { print }' \
        "$target" >"$temporary"
    chmod 600 "$temporary"
    mv -f "$temporary" "$target"
}

validate_port() {
    [[ "$1" =~ ^[0-9]+$ ]] || die "端口必须是数字"
    ((1 <= 10#$1 && 10#$1 <= 65535)) || die "端口必须在 1 到 65535 之间"
}

validate_safe_value() {
    local label="$1"
    local value="$2"
    [[ -n "$value" && "$value" =~ ^[A-Za-z0-9._-]+$ ]] || die "$label 格式不正确"
}

[[ "$SOURCE_REF" =~ ^[0-9a-fA-F]{40}$ ]] || die "source-ref 必须是 40 位 Git 提交号"
[[ "$BINARY_RELEASE_TAG" =~ ^[A-Za-z0-9._-]+$ ]] || die "binary-release-tag 格式不正确"
OLD_PORT="$(read_env_value ADAPTER_PORT || true)"
OLD_BIND="$(read_env_value ADAPTER_BIND || true)"
OLD_GROUP_ID="$(read_env_value WPS_GROUP_ID || true)"
OLD_ROOT_ID="$(read_env_value WPS_ROOT_ID || true)"
OLD_WORKSPACE_FILE="$(read_env_value WPS_WORKSPACE_FILE || true)"
OLD_COOKIE_FILE="$(read_env_value WPS_COOKIE_FILE || true)"
OLD_CSRF_FILE="$(read_env_value WPS_CSRF_TOKEN_FILE || true)"
OLD_USER_FILE="$(read_env_value ADAPTER_USERNAME_FILE || true)"
OLD_PASSWORD_FILE="$(read_env_value ADAPTER_PASSWORD_FILE || true)"

PORT="${PORT_ARG:-${OLD_PORT:-54321}}"
if [[ -z "$PORT_ARG" && -z "$OLD_PORT" && -r "$TTY" ]]; then
    ask_value "适配器监听端口" "$PORT"
    PORT="$REPLY"
fi
validate_port "$PORT"

BIND="${BIND_ARG:-${OLD_BIND:-0.0.0.0}}"
[[ "$BIND" =~ ^\[?[A-Za-z0-9.:-]+\]?$ ]] || die "监听地址格式不正确"

GROUP_ID="${GROUP_ID_ARG:-${OLD_GROUP_ID:-auto}}"
validate_safe_value "WPS 群组 ID" "$GROUP_ID"

ROOT_ID="${ROOT_ID_ARG:-${OLD_ROOT_ID:-auto}}"
validate_safe_value "WPS 根目录 ID" "$ROOT_ID"

SOURCE_WORKSPACE_FILE="${OLD_WORKSPACE_FILE:-$LEGACY_SECRET_DIR/wps-workspace.json}"
SOURCE_COOKIE_FILE="${OLD_COOKIE_FILE:-$LEGACY_SECRET_DIR/wps-cookie}"
SOURCE_CSRF_FILE="${OLD_CSRF_FILE:-$LEGACY_SECRET_DIR/wps-csrf}"
SOURCE_USER_FILE="${OLD_USER_FILE:-$LEGACY_SECRET_DIR/adapter-username}"
SOURCE_PASSWORD_FILE="${OLD_PASSWORD_FILE:-$LEGACY_SECRET_DIR/adapter-password}"
WORKSPACE_FILE="$SECRET_DIR/wps-workspace.json"
COOKIE_FILE="$SECRET_DIR/wps-cookie"
CSRF_FILE="$SECRET_DIR/wps-csrf"
USER_FILE="$SECRET_DIR/adapter-username"
PASSWORD_FILE="$SECRET_DIR/adapter-password"
[[ -e "$WORKSPACE_FILE" ]] && SOURCE_WORKSPACE_FILE="$WORKSPACE_FILE"
[[ -e "$COOKIE_FILE" ]] && SOURCE_COOKIE_FILE="$COOKIE_FILE"
[[ -e "$CSRF_FILE" ]] && SOURCE_CSRF_FILE="$CSRF_FILE"
[[ -e "$USER_FILE" ]] && SOURCE_USER_FILE="$USER_FILE"
[[ -e "$PASSWORD_FILE" ]] && SOURCE_PASSWORD_FILE="$PASSWORD_FILE"
for secret_path in "$WORKSPACE_FILE" "$COOKIE_FILE" "$CSRF_FILE" "$USER_FILE" "$PASSWORD_FILE"; do
    [[ "$secret_path" == /* ]] || die "secret 文件路径必须是绝对路径"
    [[ "$secret_path" == "$SECRET_DIR"/* ]] || die "secret 文件必须位于 $SECRET_DIR 目录内"
    relative_path="${secret_path#"$SECRET_DIR"/}"
    [[ "$relative_path" != */* && "$relative_path" != "." && "$relative_path" != ".." ]] \
        || die "secret 文件必须是目录下的直接文件"
    [[ "$relative_path" =~ ^[A-Za-z0-9._-]+$ ]] || die "secret 文件名格式不正确"
    if [[ -L "$secret_path" || ( -e "$secret_path" && ! -f "$secret_path" ) ]]; then
        die "secret 路径必须是普通文件且不能是符号链接：$secret_path"
    fi
done

[[ "$WORKSPACE_FILE" != "$COOKIE_FILE" && "$WORKSPACE_FILE" != "$CSRF_FILE" \
    && "$WORKSPACE_FILE" != "$USER_FILE" && "$WORKSPACE_FILE" != "$PASSWORD_FILE" \
    && "$COOKIE_FILE" != "$CSRF_FILE" && "$COOKIE_FILE" != "$USER_FILE" \
    && "$COOKIE_FILE" != "$PASSWORD_FILE" && "$CSRF_FILE" != "$USER_FILE" \
    && "$CSRF_FILE" != "$PASSWORD_FILE" && "$USER_FILE" != "$PASSWORD_FILE" ]] \
    || die "secret 文件路径不能重复"
for protected_path in "$CONFIG_DIR" "$SECRET_DIR" "$ENV_FILE" "$APP_DIR" "$DATA_DIR" "$RUNTIME_DIR" "$LOG_DIR"; do
    if [[ -L "$protected_path" || ( -e "$protected_path" && ! -d "$protected_path" \
        && "$protected_path" != "$ENV_FILE" ) ]]; then
        die "安装目标类型不正确或是符号链接：$protected_path"
    fi
done
PROTECTED_FILES=("$ENV_FILE" "${ENV_FILE}.new")
if [[ "$SERVICE_MODE" == "systemd" ]]; then
    PROTECTED_FILES+=(
        "/etc/systemd/system/wps-adapter.service"
        "/etc/systemd/system/wps-adapter.service.d/override.conf"
        "$CONFIG_DIR/wps-adapter-hardening.env"
    )
fi
for protected_file in "${PROTECTED_FILES[@]}"; do
    if [[ -L "$protected_file" || ( -e "$protected_file" && ! -f "$protected_file" ) ]]; then
        die "安装文件必须是普通文件且不能是符号链接：$protected_file"
    fi
done
if [[ "$SERVICE_MODE" == "systemd" ]] && {
    [[ -L "/etc/systemd/system/wps-adapter.service.d" ]] ||
    [[ -e "/etc/systemd/system/wps-adapter.service.d" && ! -d "/etc/systemd/system/wps-adapter.service.d" ]]
}; then
    die "systemd drop-in 目录类型不正确或是符号链接"
fi
[[ ! -e "${ENV_FILE}.new" && ! -L "${ENV_FILE}.new" ]] \
    || die "发现未清理的环境临时文件：${ENV_FILE}.new"

APP_PARENT="$(dirname "$APP_DIR")"
[[ -d "$APP_PARENT" ]] || install -d -m 755 "$APP_PARENT"
TMP_DIR="$(mktemp -d -p "$APP_PARENT" -t wps-adapter-install.XXXXXX)"
trap 'rm -rf -- "$TMP_DIR"' EXIT
ARCHIVE="$TMP_DIR/source.tar.gz"
SOURCE_DIR="$TMP_DIR/source"
mkdir -p "$RESUME_DIR"
chown "$RUN_USER:$RUN_GROUP" "$RESUME_DIR"
install -d -m 700 "$RESUME_DIR"
archive_tree_is_safe() {
    local root="$1"
    local path
    while IFS= read -r path; do
        [[ "$path" == "$root" ]] && continue
        [[ -L "$path" ]] && return 1
        [[ -f "$path" || -d "$path" ]] || return 1
    done < <(find "$root" -print)
}

progress_step "下载预编译服务（失败后回退源码编译）"
if download_prebuilt_binary; then
    USE_PREBUILT=1
    printf '预编译二进制下载成功，将跳过 Go 和源码下载。\n'
else
    printf '预编译二进制不可用，回退到源码现场编译。\n'
    mkdir -p "$SOURCE_DIR"
    download_archive
fi

progress_step "检查安装内容"
if (( USE_PREBUILT == 0 )); then
    archive_members_are_safe "$ARCHIVE" \
        || die "下载的项目归档包含不安全的路径"
    tar -xzf "$ARCHIVE" -C "$SOURCE_DIR" --strip-components=1
    archive_tree_is_safe "$SOURCE_DIR" \
        || die "下载的项目包含不允许的特殊文件或符号链接"
    [[ -f "$SOURCE_DIR/.env.example" ]] || die "下载的项目缺少环境变量模板"
    [[ -f "$SOURCE_DIR/go/go.mod" && -f "$SOURCE_DIR/go/cmd/wps-adapter/main.go" ]] \
        || die "下载的项目缺少 Go 服务源码"
    if [[ "$SERVICE_MODE" == "systemd" ]]; then
        [[ -f "$SOURCE_DIR/deploy/wps-adapter.service" ]] || die "下载的项目缺少 systemd 服务文件"
        [[ -f "$SOURCE_DIR/deploy/wps-adapter-hardening.conf" ]] || die "下载的项目缺少 systemd 安全配置"
        [[ -f "$SOURCE_DIR/deploy/wps-adapter-hardening.env" ]] || die "下载的项目缺少安全环境变量配置"
    fi
else
    [[ -x "$BINARY_FILE" ]] || die "预编译二进制文件不可执行"
fi

ENV_TARGET_FILE="$TMP_DIR/wps-adapter.env"
progress_step "准备配置和保留现有凭据"
if [[ -f "$ENV_SOURCE_FILE" ]]; then
    cp -p "$ENV_SOURCE_FILE" "$ENV_TARGET_FILE"
elif (( USE_PREBUILT )); then
    : >"$ENV_TARGET_FILE"
else
    install -o root -g root -m 600 "$SOURCE_DIR/.env.example" "$ENV_TARGET_FILE"
fi
set_env_value WPS_GROUP_ID "$GROUP_ID"
set_env_value WPS_ROOT_ID "$ROOT_ID"
set_env_value WPS_WORKSPACE_FILE "$WORKSPACE_FILE"
set_env_value WPS_COOKIE_FILE "$COOKIE_FILE"
set_env_value WPS_CSRF_TOKEN_FILE "$CSRF_FILE"
set_env_value ADAPTER_USERNAME_FILE "$USER_FILE"
set_env_value ADAPTER_PASSWORD_FILE "$PASSWORD_FILE"
remove_env_value ADAPTER_USER_DB
remove_env_value ADAPTER_REGISTRATION_ENABLED
set_env_value ADAPTER_BIND "$BIND"
set_env_value ADAPTER_PORT "$PORT"
chown "$RUN_USER:$RUN_GROUP" "$ENV_TARGET_FILE"
chmod 600 "$ENV_TARGET_FILE"

APP_STAGE_DIR="$TMP_DIR/runtime"
mkdir -p "$APP_STAGE_DIR"

progress_step "准备预编译服务或现场构建"
if (( USE_PREBUILT )); then
    printf '使用预编译静态二进制：Linux/%s\n' "$BINARY_ASSET"
    install -m 755 "$BINARY_FILE" "$APP_STAGE_DIR/wps-adapter"
else
    cp -a "$SOURCE_DIR/." "$APP_STAGE_DIR/"
    linux_go_target
    if [[ -z "$GO_BIN" ]]; then
        download_go_toolchain
    else
        printf '使用现有 Go 工具链：%s\n' "$GO_BIN"
    fi
    GO_BUILD_DIR="$TMP_DIR/go-build"
    mkdir -p "$GO_BUILD_DIR"
    GO_BUILD_ENV=(CGO_ENABLED=0 GOTOOLCHAIN=local GOOS=linux GOARCH="$GOARCH")
    [[ -n "$GOARM" ]] && GO_BUILD_ENV+=(GOARM="$GOARM")
    (
        cd "$SOURCE_DIR/go"
        env "${GO_BUILD_ENV[@]}" \
            "$GO_BIN" build -trimpath -ldflags "-s -w -X main.commit=$SOURCE_REF" \
            -o "$GO_BUILD_DIR/wps-adapter" ./cmd/wps-adapter
    )
    [[ -x "$GO_BUILD_DIR/wps-adapter" ]] || die "Go 服务构建失败"
    install -m 755 "$GO_BUILD_DIR/wps-adapter" "$APP_STAGE_DIR/wps-adapter"
fi
chown -R "$RUN_USER:$RUN_GROUP" "$APP_STAGE_DIR"

UNIT_FILE="$TMP_DIR/wps-adapter.service"
HARDENING_ENV_STAGE="$TMP_DIR/wps-adapter-hardening.env"
if [[ "$SERVICE_MODE" == "systemd" ]]; then
    if (( USE_PREBUILT )); then
        cat >"$UNIT_FILE" <<EOF
[Unit]
Description=WPS Drive WebDAV adapter
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_GROUP
WorkingDirectory=$APP_DIR
EnvironmentFile=-$ENV_FILE
ExecStart=$RUNTIME_DIR/wps-adapter serve
Restart=on-failure
RestartSec=5s
UMask=0077
PrivateTmp=true
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$CONFIG_DIR $DATA_DIR $RUNTIME_DIR

[Install]
WantedBy=multi-user.target
EOF
        printf '%s\n' '[Service]' \
            "EnvironmentFile=-$HARDENING_ENV_FILE" \
            >"$TMP_DIR/wps-adapter-hardening.conf"
        cat >"$HARDENING_ENV_STAGE" <<EOF
# Deployment overrides for the low-memory VPS. No credentials belong here.
WPS_ENABLE_RANGE=true
WPS_AUTO_REFRESH=true
WPS_UPLOAD_SPOOL_MEMORY=8388608
WPS_UPLOAD_MIN_FREE_BYTES=536870912
WPS_MAX_UPLOAD_BYTES=1073741824
WPS_UPLOAD_RETRIES=2
WPS_UPLOAD_RETRY_DELAY=0.5
WPS_UPLOAD_RESUME_DIR=$RESUME_DIR
WPS_MAX_UPLOADS=2
WPS_MAX_DOWNLOADS=4
WPS_TRANSFER_WAIT_TIMEOUT=30
WPS_MAX_COPY_ENTRIES=10000
WPS_MAX_COPY_DEPTH=64
WPS_MAX_PROPFIND_ENTRIES=10000
WPS_MAX_PROPFIND_DEPTH=64
WPS_MAX_LOCKS=4096
WPS_MAX_JSON_RESPONSE_BYTES=8388608
WPS_MAX_RESPONSE_BODY_BYTES=16777216
EOF
    else
        awk -v run_user="$RUN_USER" -v run_group="$RUN_GROUP" \
            -v app_dir="$APP_DIR" -v env_file="$ENV_FILE" -v runtime_dir="$RUNTIME_DIR" \
            -v config_dir="$CONFIG_DIR" -v data_dir="$DATA_DIR" '
            /^User=/ { print "User=" run_user; next }
            /^Group=/ { print "Group=" run_group; next }
            /^WorkingDirectory=/ { print "WorkingDirectory=" app_dir; next }
            /^EnvironmentFile=/ && index($0, "wps-adapter.env") { print "EnvironmentFile=-" env_file; next }
            /^ExecStart=/ { print "ExecStart=" runtime_dir "/wps-adapter serve"; next }
            /^ReadWritePaths=/ { print "ReadWritePaths=" config_dir " " data_dir " " runtime_dir; next }
            { print }
        ' "$SOURCE_DIR/deploy/wps-adapter.service" >"$UNIT_FILE"
        sed \
            -e "s#EnvironmentFile=-/etc/wps-adapter/wps-adapter-hardening.env#EnvironmentFile=-$HARDENING_ENV_FILE#" \
            -e "s#EnvironmentFile=-/opt/wps-adapter/config/wps-adapter-hardening.env#EnvironmentFile=-$HARDENING_ENV_FILE#" \
            "$SOURCE_DIR/deploy/wps-adapter-hardening.conf" >"$TMP_DIR/wps-adapter-hardening.conf"
        sed \
            -e "s#/var/lib/wps-adapter/uploads#$RESUME_DIR#g" \
            -e "s#/opt/wps-adapter/data/uploads#$RESUME_DIR#g" \
            "$SOURCE_DIR/deploy/wps-adapter-hardening.env" >"$HARDENING_ENV_STAGE"
    fi
fi

STAGED_COOKIE="$TMP_DIR/wps-cookie"
STAGED_CSRF="$TMP_DIR/wps-csrf"
STAGED_WORKSPACE="$TMP_DIR/wps-workspace.json"
STAGED_USER="$TMP_DIR/adapter-username"
STAGED_PASSWORD="$TMP_DIR/adapter-password"
stage_secret() {
    local source_path="$1"
    local staged_path="$2"
    if [[ -e "$source_path" ]]; then
        install -o root -g root -m 600 "$source_path" "$staged_path"
    else
        install -o root -g root -m 600 /dev/null "$staged_path"
    fi
}
stage_secret "$SOURCE_COOKIE_FILE" "$STAGED_COOKIE"
stage_secret "$SOURCE_CSRF_FILE" "$STAGED_CSRF"
stage_secret "$SOURCE_WORKSPACE_FILE" "$STAGED_WORKSPACE"
for extra in web-settings.json auth-settings.json; do
    source_extra="$SECRET_DIR/$extra"
    [[ -f "$source_extra" ]] || source_extra="$LEGACY_SECRET_DIR/$extra"
    [[ -f "$source_extra" ]] || continue
    install -o root -g root -m 600 "$source_extra" "$TMP_DIR/$extra"
done

if [[ -s "$SOURCE_USER_FILE" ]]; then
    stage_secret "$SOURCE_USER_FILE" "$STAGED_USER"
else
    ADAPTER_USER="$ADAPTER_USER_ARG"
    if [[ -z "$ADAPTER_USER" ]]; then
        ask_value "适配器 Basic Auth 用户名" "wps-adapter"
        ADAPTER_USER="$REPLY"
    fi
    [[ "$ADAPTER_USER" =~ ^[^[:space:]:]+$ ]] || die "适配器用户名格式不正确"
    umask 077
    printf '%s\n' "$ADAPTER_USER" >"$STAGED_USER"
    chmod 600 "$STAGED_USER"
fi

if [[ -s "$SOURCE_PASSWORD_FILE" ]]; then
    stage_secret "$SOURCE_PASSWORD_FILE" "$STAGED_PASSWORD"
else
    ask_secret "适配器 Basic Auth 密码"
    [[ -n "$REPLY" ]] || die "适配器密码不能为空"
    umask 077
    printf '%s\n' "$REPLY" >"$STAGED_PASSWORD"
    chmod 600 "$STAGED_PASSWORD"
fi

SERVICE_FILE="/etc/systemd/system/wps-adapter.service"
OVERRIDE_FILE="/etc/systemd/system/wps-adapter.service.d/override.conf"
OVERRIDE_DIR="/etc/systemd/system/wps-adapter.service.d"
HARDENING_ENV_FILE="$CONFIG_DIR/wps-adapter-hardening.env"
SERVICE_WAS_ACTIVE=0
SERVICE_WAS_ENABLED=0
UNIT_WAS_PRESENT=0
APP_WAS_PRESENT=0
ENV_WAS_PRESENT=0
OVERRIDE_WAS_PRESENT=0
HARDENING_ENV_WAS_PRESENT=0
directory_state() {
    local directory="$1"
    if [[ -d "$directory" ]]; then
        stat -c '1 %u %g %a' -- "$directory"
    else
        printf '0 0 0 0\n'
    fi
}
restore_directory_state() {
    local directory="$1"
    local was_present="$2"
    local owner_uid="$3"
    local owner_gid="$4"
    local mode="$5"
    if [[ "$was_present" == 1 ]]; then
        chown "$owner_uid:$owner_gid" "$directory" >/dev/null 2>&1 || true
        chmod "$mode" "$directory" >/dev/null 2>&1 || true
    else
        rmdir -- "$directory" >/dev/null 2>&1 || true
    fi
}
ETC_DIR_STATE="$(directory_state "$CONFIG_DIR")"
SECRET_DIR_STATE="$(directory_state "$SECRET_DIR")"
OVERRIDE_DIR_STATE="$(directory_state "$OVERRIDE_DIR")"
read -r ETC_DIR_WAS_PRESENT ETC_DIR_UID ETC_DIR_GID ETC_DIR_MODE <<<"$ETC_DIR_STATE"
read -r SECRET_DIR_WAS_PRESENT SECRET_DIR_UID SECRET_DIR_GID SECRET_DIR_MODE <<<"$SECRET_DIR_STATE"
read -r OVERRIDE_DIR_WAS_PRESENT OVERRIDE_DIR_UID OVERRIDE_DIR_GID OVERRIDE_DIR_MODE <<<"$OVERRIDE_DIR_STATE"
if service_is_active; then SERVICE_WAS_ACTIVE=1; fi
if service_is_enabled; then SERVICE_WAS_ENABLED=1; fi
if [[ "$SERVICE_MODE" == "systemd" && -e "$SERVICE_FILE" ]]; then UNIT_WAS_PRESENT=1; fi
[[ -e "$RUNTIME_DIR" ]] && APP_WAS_PRESENT=1
[[ -e "$ENV_FILE" ]] && ENV_WAS_PRESENT=1
[[ -e "$OVERRIDE_FILE" ]] && OVERRIDE_WAS_PRESENT=1
[[ -e "$HARDENING_ENV_FILE" ]] && HARDENING_ENV_WAS_PRESENT=1

ENV_BACKUP="$TMP_DIR/env.before"
UNIT_BACKUP="$TMP_DIR/unit.before"
OVERRIDE_BACKUP="$TMP_DIR/override.before"
HARDENING_ENV_BACKUP="$TMP_DIR/hardening.env.before"
APP_BACKUP="$TMP_DIR/runtime.before"
COOKIE_BACKUP="$TMP_DIR/cookie.before"
CSRF_BACKUP="$TMP_DIR/csrf.before"
USER_BACKUP="$TMP_DIR/user.before"
PASSWORD_BACKUP="$TMP_DIR/password.before"
WORKSPACE_BACKUP="$TMP_DIR/workspace.before"
[[ "$ENV_WAS_PRESENT" == 0 ]] || cp -p "$ENV_FILE" "$ENV_BACKUP"
[[ "$UNIT_WAS_PRESENT" == 0 ]] || cp -p "$SERVICE_FILE" "$UNIT_BACKUP"
[[ "$OVERRIDE_WAS_PRESENT" == 0 ]] || cp -p "$OVERRIDE_FILE" "$OVERRIDE_BACKUP"
[[ "$HARDENING_ENV_WAS_PRESENT" == 0 ]] || cp -p "$HARDENING_ENV_FILE" "$HARDENING_ENV_BACKUP"
[[ -e "$COOKIE_FILE" ]] && cp -p "$COOKIE_FILE" "$COOKIE_BACKUP"
[[ -e "$CSRF_FILE" ]] && cp -p "$CSRF_FILE" "$CSRF_BACKUP"
[[ -e "$USER_FILE" ]] && cp -p "$USER_FILE" "$USER_BACKUP"
[[ -e "$PASSWORD_FILE" ]] && cp -p "$PASSWORD_FILE" "$PASSWORD_BACKUP"
[[ -e "$WORKSPACE_FILE" ]] && cp -p "$WORKSPACE_FILE" "$WORKSPACE_BACKUP"

COMMIT_STARTED=0
APP_OLD_MOVED=0
APP_NEW_MOVED=0
rollback() {
    local status="$?"
    trap - EXIT
    if (( status != 0 && COMMIT_STARTED )); then
        service_stop >/dev/null 2>&1 || true
        if (( APP_NEW_MOVED )); then
            rm -rf -- "$RUNTIME_DIR" >/dev/null 2>&1 || true
        fi
        if (( APP_OLD_MOVED )); then
            rm -rf -- "$RUNTIME_DIR" >/dev/null 2>&1 || true
            [[ -e "$APP_BACKUP" ]] && mv "$APP_BACKUP" "$RUNTIME_DIR" >/dev/null 2>&1 || true
        fi
        if (( ENV_WAS_PRESENT )); then mv -f "$ENV_BACKUP" "$ENV_FILE" >/dev/null 2>&1 || true; else rm -f -- "$ENV_FILE" >/dev/null 2>&1 || true; fi
        if [[ "$SERVICE_MODE" == "systemd" ]]; then
            if (( UNIT_WAS_PRESENT )); then mv -f "$UNIT_BACKUP" "$SERVICE_FILE" >/dev/null 2>&1 || true; else rm -f -- "$SERVICE_FILE" >/dev/null 2>&1 || true; fi
            if (( OVERRIDE_WAS_PRESENT )); then mv -f "$OVERRIDE_BACKUP" "$OVERRIDE_FILE" >/dev/null 2>&1 || true; else rm -f -- "$OVERRIDE_FILE" >/dev/null 2>&1 || true; fi
            if (( HARDENING_ENV_WAS_PRESENT )); then mv -f "$HARDENING_ENV_BACKUP" "$HARDENING_ENV_FILE" >/dev/null 2>&1 || true; else rm -f -- "$HARDENING_ENV_FILE" >/dev/null 2>&1 || true; fi
        fi
        for pair in \
            "$WORKSPACE_FILE:$WORKSPACE_BACKUP" \
            "$COOKIE_FILE:$COOKIE_BACKUP" "$CSRF_FILE:$CSRF_BACKUP" \
            "$USER_FILE:$USER_BACKUP" "$PASSWORD_FILE:$PASSWORD_BACKUP"; do
            target="${pair%%:*}"
            backup="${pair#*:}"
            rm -f -- "$target" >/dev/null 2>&1 || true
            [[ -e "$backup" ]] && mv -f "$backup" "$target" >/dev/null 2>&1 || true
        done
        restore_directory_state "$OVERRIDE_DIR" "$OVERRIDE_DIR_WAS_PRESENT" "$OVERRIDE_DIR_UID" "$OVERRIDE_DIR_GID" "$OVERRIDE_DIR_MODE"
        restore_directory_state "$SECRET_DIR" "$SECRET_DIR_WAS_PRESENT" "$SECRET_DIR_UID" "$SECRET_DIR_GID" "$SECRET_DIR_MODE"
        restore_directory_state "$CONFIG_DIR" "$ETC_DIR_WAS_PRESENT" "$ETC_DIR_UID" "$ETC_DIR_GID" "$ETC_DIR_MODE"
        service_reload >/dev/null 2>&1 || true
        if (( SERVICE_WAS_ACTIVE )); then service_start >/dev/null 2>&1 || true; fi
        if (( SERVICE_WAS_ENABLED )); then service_enable >/dev/null 2>&1 || true; else service_disable >/dev/null 2>&1 || true; fi
    fi
    rm -rf -- "$TMP_DIR"
    exit "$status"
}
trap rollback EXIT

COMMIT_STARTED=1
progress_step "切换应用文件和服务配置"
if (( SERVICE_WAS_ACTIVE )); then service_stop; fi
install -d -m 755 "$(dirname "$APP_DIR")"
if [[ "$SERVICE_MODE" == "systemd" ]]; then
    install -d -m 755 "$OVERRIDE_DIR"
fi
if (( APP_WAS_PRESENT )); then
    mv "$RUNTIME_DIR" "$APP_BACKUP"
    APP_OLD_MOVED=1
fi
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 700 "$APP_DIR" "$CONFIG_DIR" "$SECRET_DIR" "$DATA_DIR" "$LOG_DIR"
mv "$APP_STAGE_DIR" "$RUNTIME_DIR"
APP_NEW_MOVED=1
for pair in \
    "$STAGED_WORKSPACE:$WORKSPACE_FILE" \
    "$STAGED_COOKIE:$COOKIE_FILE" "$STAGED_CSRF:$CSRF_FILE" \
    "$STAGED_USER:$USER_FILE" "$STAGED_PASSWORD:$PASSWORD_FILE"; do
    staged="${pair%%:*}"
    target="${pair#*:}"
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$staged" "$target"
done
for extra in web-settings.json auth-settings.json; do
    staged="$TMP_DIR/$extra"
    [[ -f "$staged" ]] || continue
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$staged" "$SECRET_DIR/$extra"
done
install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_TARGET_FILE" "${ENV_FILE}.new"
mv -f "${ENV_FILE}.new" "$ENV_FILE"
if [[ "$SERVICE_MODE" == "systemd" ]]; then
    install -o root -g root -m 644 "$UNIT_FILE" "$SERVICE_FILE"
    install -o root -g root -m 644 "$TMP_DIR/wps-adapter-hardening.conf" "$OVERRIDE_FILE"
    install -o root -g root -m 600 "$HARDENING_ENV_STAGE" "$HARDENING_ENV_FILE"
fi

service_reload
if (( UNIT_WAS_PRESENT == 0 )); then service_enable; fi
progress_step "启动适配器服务"
service_start
sleep 1
service_is_active || {
    if [[ "$SERVICE_MODE" == "systemd" ]]; then
        systemctl status wps-adapter.service --no-pager >&2 || true
    else
        tail -50 "$LOG_FILE" >&2 || true
    fi
    die "wps-adapter 服务没有正常启动"
}
progress_step "执行本地健康检查"
health_check "http://127.0.0.1:$PORT/healthz" \
    || {
        if [[ "$SERVICE_MODE" == "systemd" ]]; then
            die "服务已启动但健康检查失败，请查看 journalctl -u wps-adapter"
        fi
        die "服务已启动但健康检查失败，请查看 $LOG_FILE"
    }

if [[ -d "$LEGACY_CONFIG_DIR" && ! -L "$LEGACY_CONFIG_DIR" ]]; then
    rm -rf -- "$LEGACY_CONFIG_DIR"
fi

printf '\n原生部署完成。\n'
printf '监听端口：%s\n' "$PORT"
printf '部署目录：%s\n' "$APP_DIR"
printf 'WebDAV： http://<VPS地址>:%s/dav/\n' "$PORT"
printf '网页：   http://<VPS地址>:%s/\n' "$PORT"
printf '运行用户：%s\n' "$RUN_USER"
printf '凭据目录：%s（不会被升级覆盖）\n' "$SECRET_DIR"
printf '下一步：在自己的电脑下载并运行独立的 wps_login.py 完成 WPS 登录。\n'
