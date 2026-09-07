#!/usr/bin/env bash
set -Eeuo pipefail

# Generate the source file manifest verified by the one-command installers.
# The repository is intentionally free of a Python build dependency; this
# helper only needs Git and a SHA-256 implementation.
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
MANIFEST_FILE="$ROOT_DIR/release-manifest.txt"

sha256_file() {
    local path="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$path" | awk '{print $1}'
        return 0
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$path" | awk '{print $1}'
        return 0
    fi
    printf '缺少 sha256sum 或 shasum，无法生成发布清单\n' >&2
    return 1
}

write_manifest() {
    local output="$1"
    local relative path digest
    : >"$output"
    while IFS= read -r relative; do
        case "$relative" in
            release-manifest.txt|scripts/install-native.sh|scripts/install-docker.sh)
                continue
                ;;
        esac
        path="$ROOT_DIR/$relative"
        [[ -f "$path" && ! -L "$path" ]] || {
            printf '清单路径不是普通文件：%s\n' "$relative" >&2
            return 1
        }
        digest="$(sha256_file "$path")"
        printf '%s  %s\n' "$digest" "$relative" >>"$output"
    done < <(git -C "$ROOT_DIR" ls-files | LC_ALL=C sort)
}

if [[ "${1:-}" == "--check" ]]; then
    temporary_file="$(mktemp "${TMPDIR:-/tmp}/wps-release-manifest.XXXXXX")"
    trap 'rm -f -- "$temporary_file"' EXIT
    write_manifest "$temporary_file"
    cmp -s "$MANIFEST_FILE" "$temporary_file" || {
        printf '%s is out of date\n' "$MANIFEST_FILE" >&2
        exit 1
    }
    exit 0
fi

[[ $# -eq 0 ]] || {
    printf '用法：%s [--check]\n' "$0" >&2
    exit 2
}
write_manifest "$MANIFEST_FILE"
printf '已生成 %s\n' "$MANIFEST_FILE"
