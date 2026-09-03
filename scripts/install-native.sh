#!/usr/bin/env bash
set -Eeuo pipefail

# One-command systemd installer. It deliberately keeps credentials outside
# the application checkout so upgrades cannot overwrite them.
REPOSITORY="https://github.com/galiandan/WPS_2_WebDAV"
BRANCH="${WPS_ADAPTER_BRANCH:-main}"
APP_DIR="/opt/wps-adapter"
ETC_DIR="/etc/wps-adapter"
SECRET_DIR="$ETC_DIR/secrets"
ENV_FILE="$ETC_DIR/wps-adapter.env"

PORT_ARG=""
BIND_ARG=""
GROUP_ID_ARG=""
ROOT_ID_ARG=""
ADAPTER_USER_ARG=""

die() {
    printf '安装失败：%s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
用法：install-native.sh [选项]

选项：
  --port PORT          适配器监听端口，默认 54321
  --bind ADDRESS      监听地址，默认 0.0.0.0
  --group-id ID       WPS 企业群组 ID
  --root-id ID        WPS 根目录 ID，默认 0
  --adapter-user USER 适配器 Basic Auth 用户名
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
        --help|-h) usage; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done

[[ "${EUID:-$(id -u)}" == "0" ]] || die "请使用 root 运行，或在命令前加 sudo"

if ! command -v curl >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y ca-certificates curl tar python3
    else
        die "缺少 curl、tar 或 python3，且当前系统没有 apt-get"
    fi
fi
command -v systemctl >/dev/null 2>&1 || die "当前系统没有 systemctl，不适合原生 systemd 部署"
python3 -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)' \
    || die "需要 Python 3.11 或更高版本"

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
    local temporary
    temporary="$(mktemp)"
    if [[ -f "$ENV_FILE" ]]; then
        awk -v key="$key" -v value="$value" '
            BEGIN { found = 0 }
            $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
                print key "=" value
                found = 1
                next
            }
            { print }
            END { if (!found) print key "=" value }
        ' "$ENV_FILE" >"$temporary"
    else
        printf '%s=%s\n' "$key" "$value" >"$temporary"
    fi
    chmod 600 "$temporary"
    mv -f "$temporary" "$ENV_FILE"
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

install -d -o root -g root -m 700 "$ETC_DIR" "$SECRET_DIR" "$APP_DIR"

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
done
install -d -o root -g root -m 700 "$(dirname "$COOKIE_FILE")" "$(dirname "$CSRF_FILE")" \
    "$(dirname "$USER_FILE")" "$(dirname "$PASSWORD_FILE")"
[[ -e "$COOKIE_FILE" ]] || install -o root -g root -m 600 /dev/null "$COOKIE_FILE"
[[ -e "$CSRF_FILE" ]] || install -o root -g root -m 600 /dev/null "$CSRF_FILE"
chmod 600 "$COOKIE_FILE" "$CSRF_FILE"

if [[ ! -s "$USER_FILE" ]]; then
    ADAPTER_USER="$ADAPTER_USER_ARG"
    if [[ -z "$ADAPTER_USER" ]]; then
        ask_value "适配器 Basic Auth 用户名" "wps-adapter"
        ADAPTER_USER="$REPLY"
    fi
    [[ "$ADAPTER_USER" =~ ^[^[:space:]:]+$ ]] || die "适配器用户名格式不正确"
    umask 077
    printf '%s\n' "$ADAPTER_USER" >"$USER_FILE"
    chmod 600 "$USER_FILE"
fi

if [[ ! -s "$PASSWORD_FILE" ]]; then
    ask_secret "适配器 Basic Auth 密码"
    [[ -n "$REPLY" ]] || die "适配器密码不能为空"
    umask 077
    printf '%s\n' "$REPLY" >"$PASSWORD_FILE"
    chmod 600 "$PASSWORD_FILE"
fi

TMP_DIR="$(mktemp -d -t wps-adapter-install.XXXXXX)"
trap 'rm -rf -- "$TMP_DIR"' EXIT
ARCHIVE="$TMP_DIR/source.tar.gz"
SOURCE_DIR="$TMP_DIR/source"
mkdir -p "$SOURCE_DIR"
curl --fail --silent --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
    "$REPOSITORY/archive/refs/heads/$BRANCH.tar.gz" -o "$ARCHIVE"
tar -xzf "$ARCHIVE" -C "$SOURCE_DIR" --strip-components=1
[[ -f "$SOURCE_DIR/deploy/wps-adapter.service" ]] || die "下载的项目缺少 systemd 服务文件"

if [[ ! -f "$ENV_FILE" ]]; then
    install -o root -g root -m 600 "$SOURCE_DIR/.env.example" "$ENV_FILE"
fi
set_env_value WPS_GROUP_ID "$GROUP_ID"
set_env_value WPS_ROOT_ID "$ROOT_ID"
set_env_value WPS_COOKIE_FILE "$COOKIE_FILE"
set_env_value WPS_CSRF_TOKEN_FILE "$CSRF_FILE"
set_env_value ADAPTER_USERNAME_FILE "$USER_FILE"
set_env_value ADAPTER_PASSWORD_FILE "$PASSWORD_FILE"
set_env_value ADAPTER_BIND "$BIND"
set_env_value ADAPTER_PORT "$PORT"
chmod 600 "$ENV_FILE"

cp -a "$SOURCE_DIR/." "$APP_DIR/"
install -o root -g root -m 644 "$SOURCE_DIR/deploy/wps-adapter.service" \
    /etc/systemd/system/wps-adapter.service
install -d -m 755 /etc/systemd/system/wps-adapter.service.d
install -o root -g root -m 644 "$SOURCE_DIR/deploy/wps-adapter-hardening.conf" \
    /etc/systemd/system/wps-adapter.service.d/override.conf
install -o root -g root -m 600 "$SOURCE_DIR/deploy/wps-adapter-hardening.env" \
    "$ETC_DIR/wps-adapter-hardening.env"

systemctl daemon-reload
systemctl enable --now wps-adapter.service
systemctl restart wps-adapter.service
sleep 1
systemctl is-active --quiet wps-adapter.service || {
    systemctl status wps-adapter.service --no-pager >&2 || true
    die "wps-adapter 服务没有正常启动"
}
curl --fail --silent --show-error --max-time 8 "http://127.0.0.1:$PORT/healthz" >/dev/null \
    || die "服务已启动但健康检查失败，请查看 journalctl -u wps-adapter"

printf '\n原生部署完成。\n'
printf '监听端口：%s\n' "$PORT"
printf 'WebDAV： http://<VPS地址>:%s/dav/\n' "$PORT"
printf '网页：   http://<VPS地址>:%s/\n' "$PORT"
printf '凭据目录：%s（不会被升级覆盖）\n' "$SECRET_DIR"
printf '下一步：在自己的电脑运行仓库中的 python3 wps_login.py 完成 WPS 登录。\n'
