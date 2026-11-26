#!/usr/bin/env bash
set -euo pipefail

# docker-entrypoint-slot-test.sh verifies the persistent container binary slot
# managed by deploy/docker-entrypoint.sh across fresh bootstrap, in-app UI
# upgrades, in-app UI rollbacks, image upgrades, utility commands, and read-only
# fallbacks without requiring root privileges or Docker daemon access.

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/tokenrouter-entrypoint-slot.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

fail() {
    printf 'docker entrypoint slot test failed: %s\n' "$1" >&2
    exit 1
}

write_stub_binary() {
    path=$1
    label=$2
    cat > "$path" <<EOF
#!/bin/sh
printf '%s|%s\n' '$label' "\$*"
EOF
    chmod 0755 "$path"
}

image_dir="$tmp_dir/image"
data_dir="$tmp_dir/data"
slot_dir="$data_dir/bin"
image_bin="$image_dir/tokenrouter"
mkdir -p "$image_dir" "$data_dir"

export DATA_DIR="$data_dir"
export TOKENROUTER_IMAGE_BIN="$image_bin"
export TOKENROUTER_PERSISTENT_BIN_DIR="$slot_dir"

# When running tests inside a root container without su-exec or the tokenrouter
# user, provide an isolated su-exec shim that verifies the drop-target user and
# re-execs with id shadowed to 1000.
if [ "$(id -u)" = "0" ]; then
    shim_dir="$tmp_dir/bin"
    mkdir -p "$shim_dir"
    cat > "$shim_dir/su-exec" <<'EOF'
#!/bin/sh
set -e
if [ "${1:-}" != "tokenrouter" ]; then
    printf 'unexpected su-exec user: %s\n' "${1:-}" >&2
    exit 1
fi
shift
export TOKENROUTER_TEST_UID=1000
exec "$@"
EOF
    cat > "$shim_dir/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "-u" ] && [ -n "${TOKENROUTER_TEST_UID:-}" ]; then
    printf '%s\n' "$TOKENROUTER_TEST_UID"
    exit 0
fi
exec /usr/bin/id "$@" 2>/dev/null || exec /bin/id "$@"
EOF
    chmod 0755 "$shim_dir/su-exec" "$shim_dir/id"
    export PATH="$shim_dir:$PATH"
fi

# 1. Fresh startup copies the image binary into the persistent slot and runs it.
write_stub_binary "$image_bin" "image-v1.0.0"
out=$(sh deploy/docker-entrypoint.sh "$image_bin")
[ "$out" = "image-v1.0.0|" ] || fail "fresh startup output mismatch: $out"
[ -x "$slot_dir/tokenrouter" ] || fail "persistent slot binary was not created"
[ -s "$slot_dir/.image-identity" ] || fail "image identity file was not written"

# 2. Simulated in-app UI upgrade updates the slot binary while image stays v1.0.0.
write_stub_binary "$slot_dir/tokenrouter" "slot-v1.0.1"
out=$(sh deploy/docker-entrypoint.sh "$image_bin")
[ "$out" = "slot-v1.0.1|" ] || fail "restart did not retain UI-upgraded slot binary: $out"

# 3. Simulated container recreation with the same v1.0.0 image still retains v1.0.1.
out=$(sh deploy/docker-entrypoint.sh --serve-flag)
[ "$out" = "slot-v1.0.1|--serve-flag" ] || fail "same-image recreate overwrote upgraded slot: $out"

# 4. Simulated in-app UI rollback sets slot to v1.0.0 while image is v1.0.2;
# restarting on the same v1.0.2 image must keep the rolled-back slot binary.
write_stub_binary "$image_bin" "image-v1.0.2"
out=$(sh deploy/docker-entrypoint.sh "$image_bin")
[ "$out" = "image-v1.0.2|" ] || fail "new image did not refresh slot: $out"
[ -x "$slot_dir/tokenrouter.backup" ] || fail "image upgrade did not back up previous slot binary"
backup_out=$("$slot_dir/tokenrouter.backup")
[ "$backup_out" = "slot-v1.0.1|" ] || fail "backup binary did not retain prior slot version: $backup_out"

write_stub_binary "$slot_dir/tokenrouter" "slot-rolled-back-v1.0.0"
out=$(sh deploy/docker-entrypoint.sh "$image_bin")
[ "$out" = "slot-rolled-back-v1.0.0|" ] || fail "restart reverted UI rollback: $out"

# 5. If image catches up to exact same binary already in slot, identity updates without overwriting backup.
write_stub_binary "$slot_dir/tokenrouter.backup" "keep-me-backup"
cp -p "$slot_dir/tokenrouter" "$image_bin"
out=$(sh deploy/docker-entrypoint.sh "$image_bin")
[ "$out" = "slot-rolled-back-v1.0.0|" ] || fail "matching slot/image binary execution mismatch: $out"
backup_out=$("$slot_dir/tokenrouter.backup")
[ "$backup_out" = "keep-me-backup|" ] || fail "matching image update overwrote existing backup: $backup_out"

# 6. Utility CLI flags bypass slot replacement and invoke the image binary directly.
write_stub_binary "$image_bin" "image-cli"
write_stub_binary "$slot_dir/tokenrouter" "slot-should-not-run"
out=$(sh deploy/docker-entrypoint.sh "$image_bin" --version)
[ "$out" = "image-cli|--version" ] || fail "utility flag did not run image binary directly: $out"
[ "$("$slot_dir/tokenrouter")" = "slot-should-not-run|" ] || fail "utility flag modified slot binary"

# 7. Explicit opt-out or non-writable DATA_DIR falls back cleanly to image binary.
out=$(TOKENROUTER_DISABLE_PERSISTENT_BIN=true sh deploy/docker-entrypoint.sh "$image_bin")
[ "$out" = "image-cli|" ] || fail "disable env var did not fall back to image binary: $out"

# Test read-only data_dir directly as uid 1000 (skipping root chown/bypass).
chmod 0555 "$data_dir"
out=$(TOKENROUTER_TEST_UID=1000 sh deploy/docker-entrypoint.sh "$image_bin" 2>/dev/null)
chmod 0755 "$data_dir"
[ "$out" = "image-cli|" ] || fail "read-only data dir did not fall back to image binary: $out"

printf 'docker entrypoint slot tests passed\n'
