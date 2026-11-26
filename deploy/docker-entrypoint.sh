#!/bin/sh
set -e

# Fix data directory permissions when running as root.
# Docker named volumes / host bind-mounts may be owned by root,
# preventing the non-root tokenrouter user from writing files.
data_dir="${DATA_DIR:-/app/data}"
if [ "$(id -u)" = "0" ]; then
    mkdir -p "$data_dir" 2>/dev/null || true
    # Use || true to avoid failure on read-only mounted files (e.g. config.yaml:ro)
    chown -R tokenrouter:tokenrouter "$data_dir" 2>/dev/null || true
    # Re-invoke this script as tokenrouter so the flag-detection and
    # persistent-slot synchronization below run under the non-root user.
    exec su-exec tokenrouter "$0" "$@"
fi

# Compatibility: if the first arg looks like a flag (e.g. --help),
# prepend the default binary so it behaves the same as the old
# ENTRYPOINT ["/app/tokenrouter"] style.
image_bin="${TOKENROUTER_IMAGE_BIN:-/app/tokenrouter}"
if [ "${1#-}" != "$1" ]; then
    set -- "$image_bin" "$@"
fi

# should_use_persistent_slot returns 0 when launching the main server binary
# without one-off CLI utility flags that intentionally inspect or migrate
# using the image-bundled executable.
should_use_persistent_slot() {
    if [ "${TOKENROUTER_DISABLE_PERSISTENT_BIN:-false}" = "true" ]; then
        return 1
    fi
    case "${1:-}" in
        "$image_bin"|/app/tokenrouter|/app/sub2api)
            ;;
        *)
            return 1
            ;;
    esac
    shift
    for arg in "$@"; do
        case "$arg" in
            -version|--version|-h|--help|-migrate-config|--migrate-config|-setup|--setup)
                return 1
                ;;
        esac
    done
    [ -x "$image_bin" ]
}

# file_sha256 prints the lowercase SHA-256 digest of a file when sha256sum is
# available and returns nonzero otherwise.
file_sha256() {
    command -v sha256sum >/dev/null 2>&1 || return 1
    sha256sum "$1" 2>/dev/null | awk '{print $1}'
}

# sync_persistent_binary copies image_bin into the writable persistent slot on
# first startup or whenever the container image binary itself changes. In-place
# UI upgrades and rollbacks keep the same image binary SHA-256, so restarts and
# container recreations preserve the operator-selected binary in /app/data/bin.
sync_persistent_binary() {
    data_dir="${DATA_DIR:-/app/data}"
    slot_dir="${TOKENROUTER_PERSISTENT_BIN_DIR:-${data_dir}/bin}"
    slot_bin="${slot_dir}/tokenrouter"
    slot_backup="${slot_bin}.backup"
    image_id_file="${slot_dir}/.image-identity"

    if [ ! -d "$data_dir" ] || [ ! -w "$data_dir" ]; then
        return 1
    fi
    if ! mkdir -p "$slot_dir" 2>/dev/null; then
        return 1
    fi

    image_sha="$(file_sha256 "$image_bin" || true)"
    if [ -z "$image_sha" ]; then
        return 1
    fi

    recorded_sha=""
    if [ -f "$image_id_file" ]; then
        recorded_sha="$(tr -d ' \t\r\n' < "$image_id_file" 2>/dev/null || true)"
    fi

    if [ -x "$slot_bin" ] && [ "$recorded_sha" = "$image_sha" ]; then
        printf '%s\n' "$slot_bin"
        return 0
    fi

    slot_sha=""
    if [ -x "$slot_bin" ]; then
        slot_sha="$(file_sha256 "$slot_bin" || true)"
    fi

    # If the container image caught up to a binary already installed in-app,
    # record the matching identity without overwriting the existing backup.
    if [ -x "$slot_bin" ] && [ "$slot_sha" = "$image_sha" ]; then
        id_tmp="${image_id_file}.tmp.$$"
        if printf '%s\n' "$image_sha" > "$id_tmp" 2>/dev/null && mv -f "$id_tmp" "$image_id_file" 2>/dev/null; then
            printf '%s\n' "$slot_bin"
            return 0
        fi
        rm -f "$id_tmp" 2>/dev/null || true
        return 1
    fi

    if [ -x "$slot_bin" ]; then
        backup_tmp="${slot_backup}.tmp.$$"
        if cp -p "$slot_bin" "$backup_tmp" 2>/dev/null && chmod 0755 "$backup_tmp" 2>/dev/null; then
            mv -f "$backup_tmp" "$slot_backup" 2>/dev/null || rm -f "$backup_tmp" 2>/dev/null || true
        else
            rm -f "$backup_tmp" 2>/dev/null || true
        fi
    fi

    slot_tmp="${slot_bin}.tmp.$$"
    id_tmp="${image_id_file}.tmp.$$"
    if cp -p "$image_bin" "$slot_tmp" 2>/dev/null &&
        chmod 0755 "$slot_tmp" 2>/dev/null &&
        mv -f "$slot_tmp" "$slot_bin" 2>/dev/null &&
        printf '%s\n' "$image_sha" > "$id_tmp" 2>/dev/null &&
        mv -f "$id_tmp" "$image_id_file" 2>/dev/null; then
        printf '%s\n' "$slot_bin"
        return 0
    fi

    rm -f "$slot_tmp" "$id_tmp" 2>/dev/null || true
    return 1
}

if should_use_persistent_slot "$@"; then
    if resolved_bin="$(sync_persistent_binary)"; then
        shift
        set -- "$resolved_bin" "$@"
    else
        echo "Warning: failed to prepare persistent binary slot; starting image binary directly." >&2
    fi
fi

exec "$@"
