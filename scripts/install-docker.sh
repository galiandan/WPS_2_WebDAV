#!/usr/bin/env bash
set -Eeuo pipefail

# One-command Docker installer. The host keeps configuration and credentials;
# the image contains only the dependency-free application code.
REPOSITORY="https://github.com/galiandan/WPS_2_WebDAV"
# This is deliberately an immutable commit, updated by the release process.
SOURCE_REF="${WPS_ADAPTER_SOURCE_REF:-4d1e91cbe6ce20774bd9fdf73ca8f2fffd718c68}"
SOURCE_MANIFEST_SHA256="${WPS_ADAPTER_SOURCE_MANIFEST_SHA256:-2b6fb426eceded815ce79f27511479b530b84bb9a13456c667369d801d542899}"
APP_DIR="/opt/wps-adapter"
ETC_DIR="/etc/wps-adapter"
SECRET_DIR="$ETC_DIR/secrets"
RESUME_DIR="/var/lib/wps-adapter/uploads"
ENV_FILE="$ETC_DIR/wps-adapter.env"
IMAGE_NAME="wps-enterprise-adapter:latest"
CONTAINER_NAME="wps-adapter"

PORT_ARG=""
BIND_ARG=""
GROUP_ID_ARG=""
ROOT_ID_ARG=""
ADAPTER_USER_ARG=""
RUN_USER_ARG=""
REPLACE_NATIVE=0

TOTAL_STEPS=7
CURRENT_STEP=0
PACKAGE_MANAGER=""
DOWNLOAD_CONNECT_TIMEOUT="${WPS_ADAPTER_DOWNLOAD_CONNECT_TIMEOUT:-10}"
DOWNLOAD_MAX_TIME="${WPS_ADAPTER_DOWNLOAD_MAX_TIME:-300}"
DOCKER_SERVICE_MODE=""
DOCKER_BASE_IMAGE=""
LOCAL_BASE_IMAGE="wps-adapter-python-base:3.12-slim"

die() {
    printf '安装失败：%s\n' "$*" >&2
    exit 1
}

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

install_docker_package() {
    case "$PACKAGE_MANAGER" in
        apt) package_install docker.io ;;
        *)
            if package_install docker; then
                return 0
            fi
            if package_install moby-engine; then
                return 0
            fi
            die "无法通过 $PACKAGE_MANAGER 安装 Docker；请检查发行版软件源"
            ;;
    esac
}

install_docker_dependencies() {
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

download_file() {
    local url="$1"
    local target="$2"
    case "$url" in
        https://*) ;;
        *)
            printf '拒绝非 HTTPS 下载地址。\n' >&2
            return 1
            ;;
    esac
    if has_command curl; then
        curl --fail --show-error --progress-bar --location --max-filesize 52428800 \
            --connect-timeout "$DOWNLOAD_CONNECT_TIMEOUT" --max-time "$DOWNLOAD_MAX_TIME" \
            --retry 2 --retry-delay 1 --proto-redir '=https' --proto '=https' --tlsv1.2 \
            "$url" -o "$target"
    elif has_command wget; then
        if has_command timeout; then
            timeout "$DOWNLOAD_MAX_TIME" \
                wget -T "$DOWNLOAD_CONNECT_TIMEOUT" -t 3 -O "$target" "$url"
        else
            wget -T "$DOWNLOAD_CONNECT_TIMEOUT" -t 3 -O "$target" "$url"
        fi
    else
        die "缺少 curl 或 wget，无法下载项目归档"
    fi
}

download_archive() {
    local direct_url="$REPOSITORY/archive/$SOURCE_REF.tar.gz"
    local candidate index=0 total
    local candidates=()
    [[ -n "${WPS_ADAPTER_ARCHIVE_URL:-}" ]] && candidates+=("$WPS_ADAPTER_ARCHIVE_URL")
    candidates+=(
        "https://gh-proxy.com/$direct_url"
        "https://ghfast.top/$direct_url"
        "$direct_url"
    )
    total="${#candidates[@]}"
    for candidate in "${candidates[@]}"; do
        ((index += 1))
        printf '尝试下载源代码（地址 %d/%d）\n' "$index" "$total"
        rm -f -- "$ARCHIVE"
        if download_file "$candidate" "$ARCHIVE" \
            && tar -tzf "$ARCHIVE" >/dev/null 2>&1 \
            && tar -xOf "$ARCHIVE" "WPS_2_WebDAV-$SOURCE_REF/release-manifest.txt" \
                | sha256sum -c <(printf '%s  -\n' "$SOURCE_MANIFEST_SHA256") >/dev/null 2>&1; then
            return 0
        fi
        printf '该下载地址不可用，准备尝试下一个地址。\n' >&2
    done
    die "项目归档下载失败；可设置 WPS_ADAPTER_ARCHIVE_URL 指定可访问的归档地址"
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

host_uses_systemd() {
    local init_name=""
    has_command systemctl || return 1
    [[ -r /proc/1/comm ]] || return 1
    IFS= read -r init_name </proc/1/comm || return 1
    [[ "$init_name" == "systemd" ]]
}

docker_daemon_ready() {
    docker info >/dev/null 2>&1
}

select_docker_service_mode() {
    if host_uses_systemd; then
        DOCKER_SERVICE_MODE="systemd"
    elif has_command rc-service; then
        DOCKER_SERVICE_MODE="openrc"
    elif has_command service; then
        DOCKER_SERVICE_MODE="sysv"
    else
        DOCKER_SERVICE_MODE=""
    fi
}

start_docker_daemon() {
    docker_daemon_ready && return 0
    select_docker_service_mode
    case "$DOCKER_SERVICE_MODE" in
        systemd)
            systemctl enable --now docker.service \
                || systemctl enable --now docker \
                || die "systemd 无法启动 Docker 服务"
            ;;
        openrc)
            rc-update add docker default >/dev/null 2>&1 || true
            rc-service docker start || die "OpenRC 无法启动 Docker 服务"
            ;;
        sysv)
            service docker start || die "SysV service 无法启动 Docker 服务"
            ;;
        *)
            die "Docker 命令存在，但 Docker daemon 未运行；当前系统没有 systemd、OpenRC 或 SysV service，请先手动启动 dockerd 后重试"
            ;;
    esac

    local attempt
    for ((attempt = 1; attempt <= 30; attempt += 1)); do
        if docker_daemon_ready; then
            printf 'Docker daemon 已就绪。\n'
            return 0
        fi
        printf '等待 Docker daemon（%d/30）\r' "$attempt"
        sleep 1
    done
    printf '\n'
    docker info >&2 || true
    die "Docker daemon 启动后未在 30 秒内就绪"
}

docker_pull() {
    local image="$1"
    if has_command timeout; then
        timeout 180 docker pull "$image"
    else
        docker pull "$image"
    fi
}

prepare_docker_base_image() {
    local candidate
    local candidates=()
    [[ -n "${WPS_ADAPTER_DOCKER_BASE_IMAGE:-}" ]] \
        && candidates+=("$WPS_ADAPTER_DOCKER_BASE_IMAGE")
    candidates+=(
        "docker.m.daocloud.io/library/python:3.12-slim"
        "dockerproxy.net/library/python:3.12-slim"
        "mirror.ccs.tencentyun.com/library/python:3.12-slim"
        "python:3.12-slim"
    )
    for candidate in "${candidates[@]}"; do
        printf '准备 Python 基础镜像：%s\n' "$candidate"
        if ! docker image inspect "$candidate" >/dev/null 2>&1; then
            docker_pull "$candidate" || {
                printf '该基础镜像地址不可用，准备尝试下一个地址。\n' >&2
                continue
            }
        fi
        docker tag "$candidate" "$LOCAL_BASE_IMAGE"
        DOCKER_BASE_IMAGE="$LOCAL_BASE_IMAGE"
        return 0
    done
    die "Python 基础镜像下载失败；可设置 WPS_ADAPTER_DOCKER_BASE_IMAGE 指定可访问的镜像"
}

usage() {
    cat <<'EOF'
用法：install-docker.sh [选项]

选项：
  --port PORT          适配器监听端口，默认 54321
  --bind ADDRESS      宿主机监听地址，默认 0.0.0.0
  --group-id ID       WPS 企业群组 ID（可选，默认自动识别）
  --root-id ID        WPS 根目录 ID（可选，默认自动识别）
  --adapter-user USER 适配器 Basic Auth 用户名
  --run-user USER      容器运行用户，默认执行 sudo 的当前用户
  --source-ref SHA     要安装的 40 位 Git 提交号（默认使用脚本内固定版本）
  --source-manifest-sha256 SHA256
                       归档内容清单的 SHA-256（用于自定义 source-ref）
  --replace-native    停用同名的原生服务
  --help              显示帮助

环境变量：
  WPS_ADAPTER_ARCHIVE_URL              自定义项目归档 HTTPS 地址
  WPS_ADAPTER_DOWNLOAD_CONNECT_TIMEOUT 下载连接超时秒数，默认 10
  WPS_ADAPTER_DOWNLOAD_MAX_TIME        单个地址总超时秒数，默认 300
  WPS_ADAPTER_DOCKER_BASE_IMAGE        自定义 Python 基础镜像地址

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
        --source-manifest-sha256)
            (($# >= 2)) || die "--source-manifest-sha256 缺少参数"
            SOURCE_MANIFEST_SHA256="$2"
            shift 2
            ;;
        --source-manifest-sha256=*) SOURCE_MANIFEST_SHA256="${1#*=}"; shift ;;
        --replace-native) REPLACE_NATIVE=1; shift ;;
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

if ! has_command curl || ! has_command tar || ! has_command sha256sum \
    || ! has_command find; then
    detect_package_manager || die "缺少安装依赖，且未识别 apt、dnf、yum、apk、pacman、zypper 或 xbps-install"
    install_docker_dependencies
fi
has_command curl || has_command wget || die "缺少 curl 或 wget，无法下载项目归档"
has_command tar || die "缺少 tar，无法解压项目归档"
has_command sha256sum || die "缺少 sha256sum；请安装 coreutils 或提供该命令"

if ! has_command docker; then
    detect_package_manager || die "没有 Docker，且未识别可用的软件包管理器"
    install_docker_package
fi
has_command docker || die "Docker 安装后仍不可用，请检查发行版软件源"
start_docker_daemon

if [[ -n "$RUN_USER_ARG" ]]; then
    RUN_USER="$RUN_USER_ARG"
elif [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
    RUN_USER="$SUDO_USER"
else
    RUN_USER="${USER:-root}"
fi
[[ "$RUN_USER" =~ ^[A-Za-z_][A-Za-z0-9_.-]*[$]?$ ]] || die "容器运行用户格式不正确"
id "$RUN_USER" >/dev/null 2>&1 || die "容器运行用户不存在：$RUN_USER"
RUN_GROUP="$(id -gn "$RUN_USER")"
RUN_UID="$(id -u "$RUN_USER")"
RUN_GID="$(id -g "$RUN_USER")"

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
    [[ -f "$ENV_FILE" ]] || return 0
    awk -v key="$key" '
        $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
            value = $0
            sub(/^[^=]*=/, "", value)
            sub(/^"/, "", value)
            sub(/"$/, "", value)
            print value
            exit
        }
    ' "$ENV_FILE"
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
[[ "$SOURCE_MANIFEST_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] \
    || die "source-manifest-sha256 必须是 64 位 SHA-256"

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
validate_safe_value "WPS 企业群组 ID" "$GROUP_ID"

ROOT_ID="${ROOT_ID_ARG:-${OLD_ROOT_ID:-auto}}"
validate_safe_value "WPS 根目录 ID" "$ROOT_ID"

WORKSPACE_FILE="${OLD_WORKSPACE_FILE:-$SECRET_DIR/wps-workspace.json}"
COOKIE_FILE="${OLD_COOKIE_FILE:-$SECRET_DIR/wps-cookie}"
CSRF_FILE="${OLD_CSRF_FILE:-$SECRET_DIR/wps-csrf}"
USER_FILE="${OLD_USER_FILE:-$SECRET_DIR/adapter-username}"
PASSWORD_FILE="${OLD_PASSWORD_FILE:-$SECRET_DIR/adapter-password}"
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
for protected_path in "$ETC_DIR" "$SECRET_DIR" "$ENV_FILE" "$APP_DIR"; do
    if [[ -L "$protected_path" || ( -e "$protected_path" && ! -d "$protected_path" \
        && "$protected_path" != "$ENV_FILE" ) ]]; then
        die "安装目标类型不正确或是符号链接：$protected_path"
    fi
done
if [[ -L "$ENV_FILE" || ( -e "$ENV_FILE" && ! -f "$ENV_FILE" ) ]]; then
    die "安装环境文件必须是普通文件且不能是符号链接：$ENV_FILE"
fi
[[ ! -e "${ENV_FILE}.new" && ! -L "${ENV_FILE}.new" ]] \
    || die "发现未清理的环境临时文件：${ENV_FILE}.new"

APP_PARENT="$(dirname "$APP_DIR")"
[[ -d "$APP_PARENT" ]] || install -d -m 755 "$APP_PARENT"
TMP_DIR="$(mktemp -d -p "$APP_PARENT" -t wps-adapter-docker.XXXXXX)"
NATIVE_WAS_ACTIVE=0
NATIVE_WAS_ENABLED=0
NATIVE_STOPPED=0
NATIVE_SYSTEMD=0
OLD_CONTAINER_NAME=""
OLD_CONTAINER_WAS_RUNNING=0
NEW_CONTAINER_STARTED=0
ENV_BACKUP=""
ENV_WAS_PRESENT=0
APP_BACKUP=""
APP_OLD_MOVED=0
APP_NEW_MOVED=0
SECRET_MUTATION_STARTED=0
COOKIE_BASENAME="$(basename -- "$COOKIE_FILE")"
CSRF_BASENAME="$(basename -- "$CSRF_FILE")"
USER_BASENAME="$(basename -- "$USER_FILE")"
PASSWORD_BASENAME="$(basename -- "$PASSWORD_FILE")"
COOKIE_BACKUP="$TMP_DIR/cookie.before"
CSRF_BACKUP="$TMP_DIR/csrf.before"
WORKSPACE_BACKUP="$TMP_DIR/workspace.before"
USER_BACKUP="$TMP_DIR/user.before"
PASSWORD_BACKUP="$TMP_DIR/password.before"
COOKIE_WAS_PRESENT=0
CSRF_WAS_PRESENT=0
WORKSPACE_WAS_PRESENT=0
USER_WAS_PRESENT=0
PASSWORD_WAS_PRESENT=0
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
ETC_DIR_STATE="$(directory_state "$ETC_DIR")"
SECRET_DIR_STATE="$(directory_state "$SECRET_DIR")"
read -r ETC_DIR_WAS_PRESENT ETC_DIR_UID ETC_DIR_GID ETC_DIR_MODE <<<"$ETC_DIR_STATE"
read -r SECRET_DIR_WAS_PRESENT SECRET_DIR_UID SECRET_DIR_GID SECRET_DIR_MODE <<<"$SECRET_DIR_STATE"

rollback() {
    local status="$?"
    trap - EXIT
    if (( status != 0 )); then
        if (( NEW_CONTAINER_STARTED )); then
            docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
        fi
        if (( SECRET_MUTATION_STARTED )); then
            for pair in \
                "$WORKSPACE_FILE:$WORKSPACE_BACKUP:$WORKSPACE_WAS_PRESENT" \
                "$COOKIE_FILE:$COOKIE_BACKUP:$COOKIE_WAS_PRESENT" \
                "$CSRF_FILE:$CSRF_BACKUP:$CSRF_WAS_PRESENT" \
                "$USER_FILE:$USER_BACKUP:$USER_WAS_PRESENT" \
                "$PASSWORD_FILE:$PASSWORD_BACKUP:$PASSWORD_WAS_PRESENT"; do
                target="${pair%%:*}"
                remainder="${pair#*:}"
                backup="${remainder%%:*}"
                was_present="${remainder#*:}"
                rm -f -- "$target" >/dev/null 2>&1 || true
                if [[ "$was_present" == 1 && -e "$backup" ]]; then
                    mv -f "$backup" "$target" >/dev/null 2>&1 || true
                fi
            done
        fi
        if [[ -n "$OLD_CONTAINER_NAME" ]] && docker container inspect "$OLD_CONTAINER_NAME" >/dev/null 2>&1; then
            docker rename "$OLD_CONTAINER_NAME" "$CONTAINER_NAME" >/dev/null 2>&1 || true
            if (( OLD_CONTAINER_WAS_RUNNING )); then
                docker start "$CONTAINER_NAME" >/dev/null 2>&1 || true
            fi
        fi
        if [[ -n "$ENV_BACKUP" && -f "$ENV_BACKUP" ]]; then
            install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_BACKUP" "${ENV_FILE}.rollback" >/dev/null 2>&1 || true
            mv -f "${ENV_FILE}.rollback" "$ENV_FILE" >/dev/null 2>&1 || true
        elif (( ENV_WAS_PRESENT == 0 )); then
            rm -f -- "$ENV_FILE" >/dev/null 2>&1 || true
        fi
        restore_directory_state "$SECRET_DIR" "$SECRET_DIR_WAS_PRESENT" "$SECRET_DIR_UID" "$SECRET_DIR_GID" "$SECRET_DIR_MODE"
        restore_directory_state "$ETC_DIR" "$ETC_DIR_WAS_PRESENT" "$ETC_DIR_UID" "$ETC_DIR_GID" "$ETC_DIR_MODE"
        if [[ -n "$APP_BACKUP" && ( -e "$APP_BACKUP" || -L "$APP_BACKUP" ) ]]; then
            rm -rf -- "$APP_DIR" >/dev/null 2>&1 || true
            mv "$APP_BACKUP" "$APP_DIR" >/dev/null 2>&1 || true
        elif (( APP_NEW_MOVED )); then
            rm -rf -- "$APP_DIR" >/dev/null 2>&1 || true
        fi
        if (( NATIVE_STOPPED )); then
            systemctl start wps-adapter.service >/dev/null 2>&1 || true
        fi
        if (( NATIVE_WAS_ENABLED )); then
            systemctl enable wps-adapter.service >/dev/null 2>&1 || true
        fi
    else
        if [[ -n "$OLD_CONTAINER_NAME" ]] && docker container inspect "$OLD_CONTAINER_NAME" >/dev/null 2>&1; then
            docker rm -f "$OLD_CONTAINER_NAME" >/dev/null 2>&1 || true
        fi
        if [[ -n "$APP_BACKUP" && ( -e "$APP_BACKUP" || -L "$APP_BACKUP" ) ]]; then
            rm -rf -- "$APP_BACKUP" >/dev/null 2>&1 || true
        fi
    fi
    rm -rf -- "$TMP_DIR"
    exit "$status"
}
trap rollback EXIT

ARCHIVE="$TMP_DIR/source.tar.gz"
SOURCE_DIR="$TMP_DIR/source"
mkdir -p "$SOURCE_DIR"
mkdir -p /var/lib/wps-adapter/uploads
chown "$RUN_USER:$RUN_GROUP" /var/lib/wps-adapter/uploads
install -d -m 700 "$RESUME_DIR"
progress_step "下载并显示项目归档进度"
download_archive
progress_step "校验归档清单和文件完整性"
archive_members_are_safe "$ARCHIVE" \
    || die "下载的项目归档包含不安全的路径"
tar -xzf "$ARCHIVE" -C "$SOURCE_DIR" --strip-components=1
archive_tree_is_safe() {
    local root="$1"
    local path
    while IFS= read -r path; do
        [[ "$path" == "$root" ]] && continue
        [[ -L "$path" ]] && return 1
        [[ -f "$path" || -d "$path" ]] || return 1
    done < <(find "$root" -print)
}
archive_tree_is_safe "$SOURCE_DIR" \
    || die "下载的项目包含不允许的特殊文件或符号链接"
MANIFEST_FILE="$SOURCE_DIR/release-manifest.txt"
[[ -f "$MANIFEST_FILE" ]] || die "下载的项目缺少内容清单"
MANIFEST_DIGEST="$(sha256sum "$MANIFEST_FILE" | awk '{print $1}')"
[[ "${MANIFEST_DIGEST,,}" == "${SOURCE_MANIFEST_SHA256,,}" ]] \
    || die "下载归档的内容清单校验失败"
MANIFEST_FILES="$TMP_DIR/manifest.files"
ACTUAL_FILES="$TMP_DIR/actual.files"
awk 'length($0) >= 67 { print substr($0, 67) }' "$MANIFEST_FILE" | LC_ALL=C sort >"$MANIFEST_FILES"
(
    cd "$SOURCE_DIR"
    find . -type f -print
) | sed 's#^\./##' | awk '$0 != "release-manifest.txt" && $0 != "scripts/install-native.sh" && $0 != "scripts/install-docker.sh"' \
    | LC_ALL=C sort >"$ACTUAL_FILES"
cmp -s "$MANIFEST_FILES" "$ACTUAL_FILES" || die "下载归档的文件清单与预期不一致"
(cd "$SOURCE_DIR" && sha256sum -c release-manifest.txt >/dev/null) \
    || die "下载归档的文件校验失败"
[[ -f "$SOURCE_DIR/deploy/Dockerfile" ]] || die "下载的项目缺少 Dockerfile"
[[ -f "$SOURCE_DIR/.env.example" ]] || die "下载的项目缺少环境变量模板"

if host_uses_systemd && systemctl is-active --quiet wps-adapter.service; then
    NATIVE_WAS_ACTIVE=1
    NATIVE_SYSTEMD=1
    if (( REPLACE_NATIVE == 0 )); then
        die "检测到正在运行的原生服务；确认替换时请加 --replace-native"
    fi
fi

progress_step "准备配置和保留现有凭据"
if [[ -f "$ENV_FILE" ]]; then
    ENV_WAS_PRESENT=1
    ENV_BACKUP="$TMP_DIR/wps-adapter.env.before"
    cp -p "$ENV_FILE" "$ENV_BACKUP"
    cp -p "$ENV_FILE" "$TMP_DIR/wps-adapter.env"
else
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$SOURCE_DIR/.env.example" "$TMP_DIR/wps-adapter.env"
fi
ENV_TARGET_FILE="$TMP_DIR/wps-adapter.env"
set_env_value WPS_GROUP_ID "$GROUP_ID"
set_env_value WPS_ROOT_ID "$ROOT_ID"
set_env_value WPS_WORKSPACE_FILE "$WORKSPACE_FILE"
set_env_value WPS_COOKIE_FILE "$COOKIE_FILE"
set_env_value WPS_CSRF_TOKEN_FILE "$CSRF_FILE"
set_env_value ADAPTER_USERNAME_FILE "$USER_FILE"
set_env_value ADAPTER_PASSWORD_FILE "$PASSWORD_FILE"
set_env_value ADAPTER_BIND "$BIND"
set_env_value ADAPTER_PORT "$PORT"
chown "$RUN_USER:$RUN_GROUP" "$ENV_TARGET_FILE"
chmod 600 "$ENV_TARGET_FILE"

APP_STAGE_DIR="$TMP_DIR/app"
mkdir -p "$APP_STAGE_DIR"
cp -a "$SOURCE_DIR/." "$APP_STAGE_DIR/"
chown -R "$RUN_USER:$RUN_GROUP" "$APP_STAGE_DIR"

# Build from the verified temporary checkout before stopping an active native service.
prepare_docker_base_image
progress_step "构建 Docker 镜像（构建输出会持续显示）"
docker build \
    --file "$SOURCE_DIR/deploy/Dockerfile" \
    --build-arg "BASE_IMAGE=$DOCKER_BASE_IMAGE" \
    --build-arg "APP_UID=$RUN_UID" \
    --build-arg "APP_GID=$RUN_GID" \
    --tag "$IMAGE_NAME" \
    "$SOURCE_DIR"

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
stage_secret "$COOKIE_FILE" "$STAGED_COOKIE"
stage_secret "$CSRF_FILE" "$STAGED_CSRF"
stage_secret "$WORKSPACE_FILE" "$STAGED_WORKSPACE"
if [[ -s "$USER_FILE" ]]; then
    stage_secret "$USER_FILE" "$STAGED_USER"
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
if [[ -s "$PASSWORD_FILE" ]]; then
    stage_secret "$PASSWORD_FILE" "$STAGED_PASSWORD"
else
    ask_secret "适配器 Basic Auth 密码"
    [[ -n "$REPLY" ]] || die "适配器密码不能为空"
    umask 077
    printf '%s\n' "$REPLY" >"$STAGED_PASSWORD"
    chmod 600 "$STAGED_PASSWORD"
fi

if [[ -e "$COOKIE_FILE" ]]; then COOKIE_WAS_PRESENT=1; cp -p "$COOKIE_FILE" "$COOKIE_BACKUP"; fi
if [[ -e "$CSRF_FILE" ]]; then CSRF_WAS_PRESENT=1; cp -p "$CSRF_FILE" "$CSRF_BACKUP"; fi
if [[ -e "$WORKSPACE_FILE" ]]; then WORKSPACE_WAS_PRESENT=1; cp -p "$WORKSPACE_FILE" "$WORKSPACE_BACKUP"; fi
if [[ -e "$USER_FILE" ]]; then USER_WAS_PRESENT=1; cp -p "$USER_FILE" "$USER_BACKUP"; fi
if [[ -e "$PASSWORD_FILE" ]]; then PASSWORD_WAS_PRESENT=1; cp -p "$PASSWORD_FILE" "$PASSWORD_BACKUP"; fi

if docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
    managed="$(docker inspect -f '{{index .Config.Labels "com.galiandan.wps-adapter.managed"}}' "$CONTAINER_NAME")"
    [[ "$managed" == "true" ]] || die "发现同名但不属于本项目的 Docker 容器：$CONTAINER_NAME"
    OLD_CONTAINER_NAME="${CONTAINER_NAME}.previous.$(date +%s)"
    if docker container inspect "$OLD_CONTAINER_NAME" >/dev/null 2>&1; then
        die "Docker 备份容器名称已存在：$OLD_CONTAINER_NAME"
    fi
    if [[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME")" == "true" ]]; then
        OLD_CONTAINER_WAS_RUNNING=1
    fi
fi

if (( NATIVE_WAS_ACTIVE )); then
    if (( NATIVE_SYSTEMD )) && systemctl is-enabled --quiet wps-adapter.service; then
        NATIVE_WAS_ENABLED=1
    fi
    NATIVE_STOPPED=1
    systemctl stop wps-adapter.service
fi

if [[ -n "$OLD_CONTAINER_NAME" ]]; then
    docker rename "$CONTAINER_NAME" "$OLD_CONTAINER_NAME"
    if (( OLD_CONTAINER_WAS_RUNNING )); then
        docker stop "$OLD_CONTAINER_NAME" >/dev/null
    fi
fi

SECRET_MUTATION_STARTED=1
progress_step "切换凭据、配置和容器"
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 700 "$ETC_DIR" "$SECRET_DIR"
install -d -m 755 "$(dirname "$APP_DIR")"
for pair in \
    "$STAGED_WORKSPACE:$WORKSPACE_FILE" \
    "$STAGED_COOKIE:$COOKIE_FILE" "$STAGED_CSRF:$CSRF_FILE" \
    "$STAGED_USER:$USER_FILE" "$STAGED_PASSWORD:$PASSWORD_FILE"; do
    staged="${pair%%:*}"
    target="${pair#*:}"
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$staged" "$target"
done
if [[ -f "$ENV_FILE" ]]; then
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_TARGET_FILE" "${ENV_FILE}.new"
    mv -f "${ENV_FILE}.new" "$ENV_FILE"
else
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_TARGET_FILE" "$ENV_FILE"
fi

NEW_CONTAINER_STARTED=1
# The directory must stay writable because credential rotation creates a
# temporary file beside the target before atomically replacing it. Overlay the
# Basic Auth files as read-only mounts so only the WPS session files can change.
docker run --detach \
    --name "$CONTAINER_NAME" \
    --label com.galiandan.wps-adapter.managed=true \
    --label "com.galiandan.wps-adapter.version=$SOURCE_REF" \
    --restart unless-stopped \
    --user "$RUN_UID:$RUN_GID" \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --env-file "$ENV_FILE" \
    --env ADAPTER_BIND=0.0.0.0 \
    --volume "$SECRET_DIR:/etc/wps-adapter/secrets:rw" \
    --volume "$RESUME_DIR:/var/lib/wps-adapter/uploads:rw" \
    --volume "$USER_FILE:/etc/wps-adapter/secrets/$USER_BASENAME:ro" \
    --volume "$PASSWORD_FILE:/etc/wps-adapter/secrets/$PASSWORD_BASENAME:ro" \
    --publish "$BIND:$PORT:$PORT" \
    "$IMAGE_NAME" >/dev/null
progress_step "执行容器健康检查"
sleep 1
docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" | grep -qx true \
    || die "Docker 容器没有正常运行"
health_check "http://127.0.0.1:$PORT/healthz" \
    || die "容器已启动但健康检查失败，请查看 docker logs $CONTAINER_NAME"

if [[ -d "$APP_DIR" || -L "$APP_DIR" ]]; then
    APP_BACKUP="${APP_DIR}.before-docker.$(date +%s)"
    [[ ! -e "$APP_BACKUP" ]] || die "应用备份目录已存在：$APP_BACKUP"
    mv "$APP_DIR" "$APP_BACKUP"
    APP_OLD_MOVED=1
fi
mv "$APP_STAGE_DIR" "$APP_DIR"
APP_NEW_MOVED=1
chown -R "$RUN_USER:$RUN_GROUP" "$APP_DIR"

if (( NATIVE_WAS_ACTIVE && NATIVE_SYSTEMD )); then
    systemctl disable wps-adapter.service
fi

if [[ -n "$OLD_CONTAINER_NAME" ]]; then
    docker rm -f "$OLD_CONTAINER_NAME" >/dev/null
fi
ENV_BACKUP=""
APP_BACKUP=""

printf '\nDocker 部署完成。\n'
printf '监听端口：%s\n' "$PORT"
printf 'WebDAV： http://<VPS地址>:%s/dav/\n' "$PORT"
printf '网页：   http://<VPS地址>:%s/\n' "$PORT"
printf '容器：   %s\n' "$CONTAINER_NAME"
printf '运行用户：%s\n' "$RUN_USER"
printf '凭据目录：%s（不会被升级覆盖）\n' "$SECRET_DIR"
printf '下一步：在自己的电脑下载并运行独立的 wps_login.py 完成 WPS 登录。\n'
