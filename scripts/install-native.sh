#!/usr/bin/env bash
set -Eeuo pipefail

# One-command systemd installer. It deliberately keeps credentials outside
# the application checkout so upgrades cannot overwrite them.
REPOSITORY="https://github.com/galiandan/WPS_2_WebDAV"
# This is deliberately an immutable commit, updated by the release process.
SOURCE_REF="${WPS_ADAPTER_SOURCE_REF:-c693a0ea3fbbdf184977a7206fa0970892e09c27}"
SOURCE_MANIFEST_SHA256="${WPS_ADAPTER_SOURCE_MANIFEST_SHA256:-1be9893e9c5c096a3d7a868e57fbed4c1fda01c7f4b10001ac00199a6ebd8966}"
APP_DIR="/opt/wps-adapter"
ETC_DIR="/etc/wps-adapter"
SECRET_DIR="$ETC_DIR/secrets"
ENV_FILE="$ETC_DIR/wps-adapter.env"

PORT_ARG=""
BIND_ARG=""
GROUP_ID_ARG=""
ROOT_ID_ARG=""
ADAPTER_USER_ARG=""
RUN_USER_ARG=""

TOTAL_STEPS=7
CURRENT_STEP=0

die() {
    printf '安装失败：%s\n' "$*" >&2
    exit 1
}

progress_step() {
    ((CURRENT_STEP += 1))
    printf '\n[%d/%d] %s\n' "$CURRENT_STEP" "$TOTAL_STEPS" "$1"
}

usage() {
    cat <<'EOF'
用法：install-native.sh [选项]

选项：
  --port PORT          适配器监听端口，默认 54321
  --bind ADDRESS      监听地址，默认 0.0.0.0
  --group-id ID       WPS 企业群组 ID（可选，默认自动识别）
  --root-id ID        WPS 根目录 ID（可选，默认自动识别）
  --adapter-user USER 适配器 Basic Auth 用户名
  --run-user USER      服务运行用户，默认执行 sudo 的当前用户
  --source-ref SHA     要安装的 40 位 Git 提交号（默认使用脚本内固定版本）
  --source-manifest-sha256 SHA256
                       归档内容清单的 SHA-256（用于自定义 source-ref）
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
        --help|-h) usage; exit 0 ;;
        *) die "未知参数：$1" ;;
    esac
done

[[ "${EUID:-$(id -u)}" == "0" ]] || die "请使用 root 运行，或在命令前加 sudo"

progress_step "检查运行环境和安装参数"

if ! command -v curl >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1 \
    || ! command -v sha256sum >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y ca-certificates curl tar coreutils python3
    else
        die "缺少 curl、tar、sha256sum 或 python3，且当前系统没有 apt-get"
    fi
fi
command -v systemctl >/dev/null 2>&1 || die "当前系统没有 systemctl，不适合原生 systemd 部署"
python3 -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)' \
    || die "需要 Python 3.11 或更高版本"

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
for protected_file in "$ENV_FILE" "/etc/systemd/system/wps-adapter.service" \
    "${ENV_FILE}.new" \
    "/etc/systemd/system/wps-adapter.service.d/override.conf" \
    "$ETC_DIR/wps-adapter-hardening.env"; do
    if [[ -L "$protected_file" || ( -e "$protected_file" && ! -f "$protected_file" ) ]]; then
        die "安装文件必须是普通文件且不能是符号链接：$protected_file"
    fi
done
if [[ -L "/etc/systemd/system/wps-adapter.service.d" || ( -e "/etc/systemd/system/wps-adapter.service.d" \
    && ! -d "/etc/systemd/system/wps-adapter.service.d" ) ]]; then
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
mkdir -p "$SOURCE_DIR"
progress_step "下载并显示项目归档进度"
curl --fail --show-error --progress-bar --location --max-filesize 52428800 \
    --proto-redir '=https' --retry 3 --proto '=https' --tlsv1.2 \
    "$REPOSITORY/archive/$SOURCE_REF.tar.gz" -o "$ARCHIVE"
progress_step "校验归档清单和文件完整性"
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
[[ -f "$SOURCE_DIR/deploy/wps-adapter.service" ]] || die "下载的项目缺少 systemd 服务文件"
[[ -f "$SOURCE_DIR/.env.example" ]] || die "下载的项目缺少环境变量模板"
[[ -f "$SOURCE_DIR/deploy/wps-adapter-hardening.conf" ]] || die "下载的项目缺少 systemd 安全配置"
[[ -f "$SOURCE_DIR/deploy/wps-adapter-hardening.env" ]] || die "下载的项目缺少安全环境变量配置"

ENV_TARGET_FILE="$TMP_DIR/wps-adapter.env"
progress_step "准备配置和保留现有凭据"
if [[ -f "$ENV_FILE" ]]; then
    cp -p "$ENV_FILE" "$ENV_TARGET_FILE"
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
set_env_value ADAPTER_BIND "$BIND"
set_env_value ADAPTER_PORT "$PORT"
chown "$RUN_USER:$RUN_GROUP" "$ENV_TARGET_FILE"
chmod 600 "$ENV_TARGET_FILE"

APP_STAGE_DIR="$TMP_DIR/app"
mkdir -p "$APP_STAGE_DIR"
cp -a "$SOURCE_DIR/." "$APP_STAGE_DIR/"
chown -R "$RUN_USER:$RUN_GROUP" "$APP_STAGE_DIR"

UNIT_FILE="$TMP_DIR/wps-adapter.service"
awk -v run_user="$RUN_USER" -v run_group="$RUN_GROUP" '
    /^User=/ { print "User=" run_user; next }
    /^Group=/ { print "Group=" run_group; next }
    { print }
' "$SOURCE_DIR/deploy/wps-adapter.service" >"$UNIT_FILE"

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

SERVICE_FILE="/etc/systemd/system/wps-adapter.service"
OVERRIDE_FILE="/etc/systemd/system/wps-adapter.service.d/override.conf"
OVERRIDE_DIR="/etc/systemd/system/wps-adapter.service.d"
HARDENING_ENV_FILE="$ETC_DIR/wps-adapter-hardening.env"
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
ETC_DIR_STATE="$(directory_state "$ETC_DIR")"
SECRET_DIR_STATE="$(directory_state "$SECRET_DIR")"
OVERRIDE_DIR_STATE="$(directory_state "$OVERRIDE_DIR")"
read -r ETC_DIR_WAS_PRESENT ETC_DIR_UID ETC_DIR_GID ETC_DIR_MODE <<<"$ETC_DIR_STATE"
read -r SECRET_DIR_WAS_PRESENT SECRET_DIR_UID SECRET_DIR_GID SECRET_DIR_MODE <<<"$SECRET_DIR_STATE"
read -r OVERRIDE_DIR_WAS_PRESENT OVERRIDE_DIR_UID OVERRIDE_DIR_GID OVERRIDE_DIR_MODE <<<"$OVERRIDE_DIR_STATE"
if systemctl is-active --quiet wps-adapter.service; then SERVICE_WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet wps-adapter.service; then SERVICE_WAS_ENABLED=1; fi
[[ -e "$SERVICE_FILE" ]] && UNIT_WAS_PRESENT=1
[[ -e "$APP_DIR" ]] && APP_WAS_PRESENT=1
[[ -e "$ENV_FILE" ]] && ENV_WAS_PRESENT=1
[[ -e "$OVERRIDE_FILE" ]] && OVERRIDE_WAS_PRESENT=1
[[ -e "$HARDENING_ENV_FILE" ]] && HARDENING_ENV_WAS_PRESENT=1

ENV_BACKUP="$TMP_DIR/env.before"
UNIT_BACKUP="$TMP_DIR/unit.before"
OVERRIDE_BACKUP="$TMP_DIR/override.before"
HARDENING_ENV_BACKUP="$TMP_DIR/hardening.env.before"
APP_BACKUP="$TMP_DIR/app.before"
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
        systemctl stop wps-adapter.service >/dev/null 2>&1 || true
        if (( APP_NEW_MOVED )); then
            rm -rf -- "$APP_DIR" >/dev/null 2>&1 || true
        fi
        if (( APP_OLD_MOVED )); then
            rm -rf -- "$APP_DIR" >/dev/null 2>&1 || true
            [[ -e "$APP_BACKUP" ]] && mv "$APP_BACKUP" "$APP_DIR" >/dev/null 2>&1 || true
        elif (( APP_NEW_MOVED == 0 && APP_WAS_PRESENT == 0 )); then
            rm -rf -- "$APP_DIR" >/dev/null 2>&1 || true
        fi
        if (( ENV_WAS_PRESENT )); then mv -f "$ENV_BACKUP" "$ENV_FILE" >/dev/null 2>&1 || true; else rm -f -- "$ENV_FILE" >/dev/null 2>&1 || true; fi
        if (( UNIT_WAS_PRESENT )); then mv -f "$UNIT_BACKUP" "$SERVICE_FILE" >/dev/null 2>&1 || true; else rm -f -- "$SERVICE_FILE" >/dev/null 2>&1 || true; fi
        if (( OVERRIDE_WAS_PRESENT )); then mv -f "$OVERRIDE_BACKUP" "$OVERRIDE_FILE" >/dev/null 2>&1 || true; else rm -f -- "$OVERRIDE_FILE" >/dev/null 2>&1 || true; fi
        if (( HARDENING_ENV_WAS_PRESENT )); then mv -f "$HARDENING_ENV_BACKUP" "$HARDENING_ENV_FILE" >/dev/null 2>&1 || true; else rm -f -- "$HARDENING_ENV_FILE" >/dev/null 2>&1 || true; fi
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
        restore_directory_state "$ETC_DIR" "$ETC_DIR_WAS_PRESENT" "$ETC_DIR_UID" "$ETC_DIR_GID" "$ETC_DIR_MODE"
        systemctl daemon-reload >/dev/null 2>&1 || true
        if (( SERVICE_WAS_ACTIVE )); then systemctl start wps-adapter.service >/dev/null 2>&1 || true; fi
        if (( SERVICE_WAS_ENABLED )); then systemctl enable wps-adapter.service >/dev/null 2>&1 || true; else systemctl disable wps-adapter.service >/dev/null 2>&1 || true; fi
    fi
    rm -rf -- "$TMP_DIR"
    exit "$status"
}
trap rollback EXIT

COMMIT_STARTED=1
progress_step "切换应用文件和 systemd 配置"
if (( SERVICE_WAS_ACTIVE )); then systemctl stop wps-adapter.service; fi
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 700 "$ETC_DIR" "$SECRET_DIR"
install -d -m 755 "$(dirname "$APP_DIR")" "/etc/systemd/system/wps-adapter.service.d"
if (( APP_WAS_PRESENT )); then mv "$APP_DIR" "$APP_BACKUP"; APP_OLD_MOVED=1; fi
mv "$APP_STAGE_DIR" "$APP_DIR"
APP_NEW_MOVED=1
for pair in \
    "$STAGED_WORKSPACE:$WORKSPACE_FILE" \
    "$STAGED_COOKIE:$COOKIE_FILE" "$STAGED_CSRF:$CSRF_FILE" \
    "$STAGED_USER:$USER_FILE" "$STAGED_PASSWORD:$PASSWORD_FILE"; do
    staged="${pair%%:*}"
    target="${pair#*:}"
    install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$staged" "$target"
done
install -o "$RUN_USER" -g "$RUN_GROUP" -m 600 "$ENV_TARGET_FILE" "${ENV_FILE}.new"
mv -f "${ENV_FILE}.new" "$ENV_FILE"
install -o root -g root -m 644 "$UNIT_FILE" "$SERVICE_FILE"
install -o root -g root -m 644 "$SOURCE_DIR/deploy/wps-adapter-hardening.conf" "$OVERRIDE_FILE"
install -o root -g root -m 600 "$SOURCE_DIR/deploy/wps-adapter-hardening.env" "$HARDENING_ENV_FILE"

systemctl daemon-reload
if (( UNIT_WAS_PRESENT == 0 )); then systemctl enable wps-adapter.service; fi
progress_step "启动适配器服务"
systemctl start wps-adapter.service
sleep 1
systemctl is-active --quiet wps-adapter.service || {
    systemctl status wps-adapter.service --no-pager >&2 || true
    die "wps-adapter 服务没有正常启动"
}
progress_step "执行本地健康检查"
curl --fail --silent --show-error --max-time 8 "http://127.0.0.1:$PORT/healthz" >/dev/null \
    || die "服务已启动但健康检查失败，请查看 journalctl -u wps-adapter"

printf '\n原生部署完成。\n'
printf '监听端口：%s\n' "$PORT"
printf 'WebDAV： http://<VPS地址>:%s/dav/\n' "$PORT"
printf '网页：   http://<VPS地址>:%s/\n' "$PORT"
printf '运行用户：%s\n' "$RUN_USER"
printf '凭据目录：%s（不会被升级覆盖）\n' "$SECRET_DIR"
printf '下一步：在自己的电脑下载并运行独立的 wps_login.py 完成 WPS 登录。\n'
