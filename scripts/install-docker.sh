#!/usr/bin/env bash
set -Eeuo pipefail

# One-command Docker installer. The host keeps configuration and credentials;
# the image contains only the dependency-free application code.
REPOSITORY="https://github.com/galiandan/WPS_2_WebDAV"
BRANCH="${WPS_ADAPTER_BRANCH:-main}"
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
  --run-user USER     容器运行用户，默认执行 sudo 的当前用户
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
        --replace-native) REPLACE_NATIVE=1; shift ;;
        --help|-h) usage; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done

[[ "${EUID:-$(id -u)}" == "0" ]] || die "请使用 root 运行，或在命令前加 sudo"

if ! command -v curl >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y ca-certificates curl tar
    else
        die "缺少 curl 或 tar，且当前系统没有 apt-get"
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

install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 700 "$ETC_DIR" "$SECRET_DIR"
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 755 "$APP_DIR"

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
done
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 700 "$(dirname "$COOKIE_FILE")" "$(dirname "$CSRF_FILE")" \
    "$(dirname "$USER_FILE")" "$(dirname "$PASSWORD_FILE")"
ensure_secret_file() {
    local secret_path="$1"
    if [[ -L "$secret_path" || ( -e "$secret_path" && ! -f "$secret_path" ) ]]; then
        die "secret 路径必须是普通文件且不能是符号链接：$secret_path"
    fi
    [[ -e "$secret_path" ]] || install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 /dev/null "$secret_path"
    chown "$RUN_USER:$RUN_GROUP" "$secret_path"
    chmod 600 "$secret_path"
}
ensure_secret_file "$COOKIE_FILE"
ensure_secret_file "$CSRF_FILE"
ensure_secret_file "$USER_FILE"
ensure_secret_file "$PASSWORD_FILE"

if [[ ! -s "$USER_FILE" ]]; then
    ADAPTER_USER="$ADAPTER_USER_ARG"
    if [[ -z "$ADAPTER_USER" ]]; then
        ask_value "适配器 Basic Auth 用户名" "wps-adapter"
        ADAPTER_USER="$REPLY"
    fi
    [[ "$ADAPTER_USER" =~ ^[^[:space:]:]+$ ]] || die "适配器用户名格式不正确"
    umask 077
    printf '%s\n' "$ADAPTER_USER" >"$USER_FILE"
    chown "$RUN_USER:$RUN_GROUP" "$USER_FILE"
    chmod 600 "$USER_FILE"
fi

if [[ ! -s "$PASSWORD_FILE" ]]; then
    ask_secret "适配器 Basic Auth 密码"
    [[ -n "$REPLY" ]] || die "适配器密码不能为空"
    umask 077
    printf '%s\n' "$REPLY" >"$PASSWORD_FILE"
    chown "$RUN_USER:$RUN_GROUP" "$PASSWORD_FILE"
    chmod 600 "$PASSWORD_FILE"
fi

TMP_DIR="$(mktemp -d -t wps-adapter-docker.XXXXXX)"
NATIVE_WAS_ACTIVE=0
NATIVE_WAS_ENABLED=0
NATIVE_STOPPED=0
OLD_CONTAINER_NAME=""
OLD_CONTAINER_WAS_RUNNING=0
NEW_CONTAINER_STARTED=0
ENV_BACKUP=""
ENV_WAS_PRESENT=0
APP_BACKUP=""

rollback() {
    local status="$?"
    trap - EXIT
    if (( status != 0 )); then
        if (( NEW_CONTAINER_STARTED )); then
            docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
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
        if [[ -n "$APP_BACKUP" && ( -e "$APP_BACKUP" || -L "$APP_BACKUP" ) ]]; then
            rm -rf -- "$APP_DIR" >/dev/null 2>&1 || true
            mv "$APP_BACKUP" "$APP_DIR" >/dev/null 2>&1 || true
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
curl --fail --silent --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
    "$REPOSITORY/archive/refs/heads/$BRANCH.tar.gz" -o "$ARCHIVE"
tar -xzf "$ARCHIVE" -C "$SOURCE_DIR" --strip-components=1
[[ -f "$SOURCE_DIR/deploy/Dockerfile" ]] || die "下载的项目缺少 Dockerfile"

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

if [[ -f "$ENV_FILE" ]]; then
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_TARGET_FILE" "${ENV_FILE}.new"
    mv -f "${ENV_FILE}.new" "$ENV_FILE"
else
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_TARGET_FILE" "$ENV_FILE"
fi

NEW_CONTAINER_STARTED=1
docker run --detach \
    --name "$CONTAINER_NAME" \
    --label com.galiandan.wps-adapter.managed=true \
    --label "com.galiandan.wps-adapter.version=$BRANCH" \
    --restart unless-stopped \
    --user "$RUN_UID:$RUN_GID" \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --env-file "$ENV_FILE" \
    --env ADAPTER_BIND=0.0.0.0 \
    --volume "$SECRET_DIR:/etc/wps-adapter/secrets:rw" \
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
fi
mv "$APP_STAGE_DIR" "$APP_DIR"
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
