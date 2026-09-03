#!/usr/bin/env bash
set -Eeuo pipefail

# One-command Docker installer. The host keeps configuration and credentials;
# the image contains only the dependency-free application code.
REPOSITORY="https://github.com/galiandan/WPS_2_WebDAV"
# This is deliberately an immutable commit, updated by the release process.
SOURCE_REF="${WPS_ADAPTER_SOURCE_REF:-2abd48e264cb3bf4a095e12a10aa72d374afe261}"
SOURCE_MANIFEST_SHA256="${WPS_ADAPTER_SOURCE_MANIFEST_SHA256:-d1a2226de011e87989433d99d0ca8110169d3efaf53ccb3851263cff04e1f9c8}"
APP_DIR="/opt/wps-adapter"
ETC_DIR="/etc/wps-adapter"
SECRET_DIR="$ETC_DIR/secrets"
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

die() {
    printf '安装失败：%s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
用法：install-docker.sh [选项]

选项：
  --port PORT          适配器监听端口，默认 54321
  --bind ADDRESS      宿主机监听地址，默认 0.0.0.0
  --group-id ID       WPS 企业群组 ID
  --root-id ID        WPS 根目录 ID，默认 0
  --adapter-user USER 适配器 Basic Auth 用户名
  --run-user USER      容器运行用户，默认执行 sudo 的当前用户
  --source-ref SHA     要安装的 40 位 Git 提交号（默认使用脚本内固定版本）
  --source-manifest-sha256 SHA256
                       归档内容清单的 SHA-256（用于自定义 source-ref）
  --replace-native    停用同名的原生 systemd 服务
  --help              显示帮助

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

if ! command -v curl >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1 \
    || ! command -v sha256sum >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y ca-certificates curl tar coreutils
    else
        die "缺少 curl、tar 或 sha256sum，且当前系统没有 apt-get"
    fi
fi

if ! command -v docker >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y docker.io
    else
        die "没有 Docker，且当前系统没有 apt-get；请先安装 Docker"
    fi
fi
command -v systemctl >/dev/null 2>&1 || die "当前系统没有 systemctl"
systemctl enable --now docker.service || die "Docker 服务没有正常启动"

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
[[ "$BIND" =~ ^[A-Za-z0-9.:[\]-]+$ ]] || die "监听地址格式不正确"

GROUP_ID="${GROUP_ID_ARG:-${OLD_GROUP_ID:-}}"
if [[ -z "$GROUP_ID" ]]; then
    ask_value "WPS 企业群组 ID"
    GROUP_ID="$REPLY"
fi
validate_safe_value "WPS 企业群组 ID" "$GROUP_ID"

ROOT_ID="${ROOT_ID_ARG:-${OLD_ROOT_ID:-0}}"
validate_safe_value "WPS 根目录 ID" "$ROOT_ID"

COOKIE_FILE="${OLD_COOKIE_FILE:-$SECRET_DIR/wps-cookie}"
CSRF_FILE="${OLD_CSRF_FILE:-$SECRET_DIR/wps-csrf}"
USER_FILE="${OLD_USER_FILE:-$SECRET_DIR/adapter-username}"
PASSWORD_FILE="${OLD_PASSWORD_FILE:-$SECRET_DIR/adapter-password}"
for secret_path in "$COOKIE_FILE" "$CSRF_FILE" "$USER_FILE" "$PASSWORD_FILE"; do
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
[[ "$COOKIE_FILE" != "$CSRF_FILE" && "$COOKIE_FILE" != "$USER_FILE" \
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
USER_BACKUP="$TMP_DIR/user.before"
PASSWORD_BACKUP="$TMP_DIR/password.before"
COOKIE_WAS_PRESENT=0
CSRF_WAS_PRESENT=0
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
curl --fail --silent --show-error --location --max-filesize 52428800 \
    --proto-redir '=https' --retry 3 --proto '=https' --tlsv1.2 \
    "$REPOSITORY/archive/$SOURCE_REF.tar.gz" -o "$ARCHIVE"
tar -xzf "$ARCHIVE" -C "$SOURCE_DIR" --strip-components=1 --no-same-owner --no-same-permissions
[[ -z "$(find "$SOURCE_DIR" -mindepth 1 ! \( -type f -o -type d \) -print -quit)" ]] \
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
    find . -mindepth 1 -type f -printf '%P\n'
) | awk '$0 != "release-manifest.txt" && $0 != "scripts/install-native.sh" && $0 != "scripts/install-docker.sh"' \
    | LC_ALL=C sort >"$ACTUAL_FILES"
cmp -s "$MANIFEST_FILES" "$ACTUAL_FILES" || die "下载归档的文件清单与预期不一致"
(cd "$SOURCE_DIR" && sha256sum --strict --check release-manifest.txt >/dev/null) \
    || die "下载归档的文件校验失败"
[[ -f "$SOURCE_DIR/deploy/Dockerfile" ]] || die "下载的项目缺少 Dockerfile"
[[ -f "$SOURCE_DIR/.env.example" ]] || die "下载的项目缺少环境变量模板"

if systemctl is-active --quiet wps-adapter.service; then
    NATIVE_WAS_ACTIVE=1
    if (( REPLACE_NATIVE == 0 )); then
        die "检测到正在运行的原生服务；确认替换时请加 --replace-native"
    fi
fi

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
docker build \
    --file "$SOURCE_DIR/deploy/Dockerfile" \
    --build-arg "APP_UID=$RUN_UID" \
    --build-arg "APP_GID=$RUN_GID" \
    --tag "$IMAGE_NAME" \
    "$SOURCE_DIR"

STAGED_COOKIE="$TMP_DIR/wps-cookie"
STAGED_CSRF="$TMP_DIR/wps-csrf"
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
    if systemctl is-enabled --quiet wps-adapter.service; then
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
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 700 "$ETC_DIR" "$SECRET_DIR"
install -d -m 755 "$(dirname "$APP_DIR")"
for pair in \
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
    --volume "$USER_FILE:/etc/wps-adapter/secrets/$USER_BASENAME:ro" \
    --volume "$PASSWORD_FILE:/etc/wps-adapter/secrets/$PASSWORD_BASENAME:ro" \
    --publish "$BIND:$PORT:$PORT" \
    "$IMAGE_NAME" >/dev/null
sleep 1
docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" | grep -qx true \
    || die "Docker 容器没有正常运行"
curl --fail --silent --show-error --max-time 8 "http://127.0.0.1:$PORT/healthz" >/dev/null \
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

if (( NATIVE_WAS_ACTIVE )); then
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
