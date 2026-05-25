#!/usr/bin/env bash

set -euo pipefail

BSYNC_BIN="${BSYNC_BIN:-./bsync}"
REMOTE="${REMOTE:-}"
BLOCK_SIZE=4096
BLOCK_COUNT=6
SKIP_BLOCKS=2
STRESS_BLOCK_SIZE=65536

if [ -z "$REMOTE" ]; then
  echo "usage: REMOTE=user@host BSYNC_BIN=/path/to/bsync bash ./test_remote_ssh.sh" >&2
  exit 2
fi
if [ ! -x "$BSYNC_BIN" ]; then
  echo "bsync binary is not executable: $BSYNC_BIN" >&2
  exit 2
fi

run_id="$$.$RANDOM"
local_dir=$(mktemp -d /tmp/bsync-remote-e2e.local.XXXXXX)
remote_dir="/tmp/bsync-remote-e2e.$run_id"
remote_bin_pids=()

cleanup() {
  local status=$?
  trap - EXIT
  remote_cmd="rm -rf '$remote_dir'"
  for transfer_pid in "${remote_bin_pids[@]}"; do
    remote_cmd="$remote_cmd; rm -f /tmp/bsync-$transfer_pid-*"
  done
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" "$remote_cmd" >/dev/null 2>&1 || true
  rm -rf "$local_dir"
  exit "$status"
}
trap cleanup EXIT

run_transfer() {
  "$BSYNC_BIN" "$@" &
  local transfer_pid=$!
  remote_bin_pids+=("$transfer_pid")
  wait "$transfer_pid"
  assert_no_remote_binary "$transfer_pid"
}

assert_no_remote_binary() {
  local transfer_pid=$1
  local orphan
  orphan=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" \
    "find /tmp -maxdepth 1 -name 'bsync-$transfer_pid-*' -print -quit")
  if [ -n "$orphan" ]; then
    echo "remote executable was not removed: $orphan" >&2
    exit 1
  fi
}

src="$local_dir/source.img"
empty_src="$local_dir/empty-source.img"
resume_expected="$local_dir/resume-expected.img"

remote_hash() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" "sha256sum '$1'" | cut -d' ' -f1
}

seed_remote_nonzero() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" \
    "dd if=/dev/zero bs=$BLOCK_SIZE count=$BLOCK_COUNT 2>/dev/null | tr '\\000' '\\377' > '$1'"
}

seed_local_nonzero() {
  dd if=/dev/zero bs="$BLOCK_SIZE" count="$BLOCK_COUNT" 2>/dev/null | tr '\000' '\377' > "$1"
}

assert_remote_equals_source() {
  local destination=$1
  assert_remote_equals_file "$src" "$destination"
}

assert_remote_equals_file() {
  local source=$1
  local destination=$2
  local source_hash destination_hash
  source_hash=$(sha256sum "$source" | cut -d' ' -f1)
  destination_hash=$(remote_hash "$destination")
  if [ "$source_hash" != "$destination_hash" ]; then
    echo "remote upload mismatch: source=$source_hash destination=$destination_hash" >&2
    exit 1
  fi
}

assert_local_equals_source() {
  local destination=$1
  assert_local_equals_file "$src" "$destination"
}

assert_local_equals_file() {
  local source=$1
  local destination=$2
  if ! cmp -s "$source" "$destination"; then
    echo "remote download mismatch: $destination" >&2
    exit 1
  fi
}

run_upload_case() {
  local name=$1
  shift
  local destination="$remote_dir/upload-$name.img"
  echo "Running SSH upload: $name"
  seed_remote_nonzero "$destination"
  run_transfer -q -b "$BLOCK_SIZE" -f "$src" -t "$REMOTE:$destination" "$@"
  assert_remote_equals_source "$destination"
}

run_download_case() {
  local name=$1
  shift
  local destination="$local_dir/download-$name.img"
  echo "Running SSH download: $name"
  seed_local_nonzero "$destination"
  run_transfer -q -d -b "$BLOCK_SIZE" -f "$destination" -t "$REMOTE:$remote_source" "$@"
  assert_local_equals_source "$destination"
}

echo "Checking SSH access to $REMOTE"
ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" "mkdir -p '$remote_dir'"

# Alternate zero and random blocks so pre-filled destinations test zero clearing.
dd if=/dev/urandom of="$src" bs="$BLOCK_SIZE" count="$BLOCK_COUNT" status=none
dd if=/dev/zero of="$src" bs="$BLOCK_SIZE" count=1 conv=notrunc status=none
dd if=/dev/zero of="$src" bs="$BLOCK_SIZE" seek=2 count=1 conv=notrunc status=none
dd if=/dev/zero of="$src" bs="$BLOCK_SIZE" seek=4 count=1 conv=notrunc status=none

# Upload profiles exercise the local SSH sender's compression and encryption.
run_upload_case default-compression -w 1
run_upload_case no-compression -n -w 2
run_upload_case compression-fast -L fast -w 2
run_upload_case compression-better -L better -w 2
run_upload_case compression-best -L best -w 2
run_upload_case encrypted-compression -e -w 2
run_upload_case encrypted-no-compression -e -n -w 2

# Paths containing spaces and shell punctuation prove SSH command arguments
# arrive as path data rather than being interpreted by the remote shell.
quoted_remote_dst="$remote_dir/upload with spaces;still-data.img"
echo "Running SSH upload: quoted-remote-path"
seed_remote_nonzero "$quoted_remote_dst"
run_transfer -q -b "$BLOCK_SIZE" -f "$src" -t "$REMOTE:$quoted_remote_dst" -L fast -w 2
assert_remote_equals_source "$quoted_remote_dst"

# Keep a verified remote fixture as the source for all download profiles.
run_upload_case download-source -e -w 2
remote_source="$remote_dir/upload-download-source.img"

# Download profiles exercise the SSH-launched sender, including forwarded -L.
run_download_case default-compression -w 1
run_download_case no-compression -n -w 2
run_download_case compression-fast -L fast -w 2
run_download_case compression-better -L better -w 2
run_download_case compression-best -L best -w 2
run_download_case encrypted-compression -e -w 2
run_download_case encrypted-no-compression -e -n -w 2

# Resume tests intentionally preserve the skipped FF prefix and transfer only
# the suffix. Matching this expected output proves -s is applied remotely.
seed_local_nonzero "$resume_expected"
dd if="$src" of="$resume_expected" bs="$BLOCK_SIZE" skip="$SKIP_BLOCKS" seek="$SKIP_BLOCKS" conv=notrunc status=none

upload_resume_dst="$remote_dir/upload-resume.img"
echo "Running SSH upload: resume-skip"
seed_remote_nonzero "$upload_resume_dst"
run_transfer -q -b "$BLOCK_SIZE" -s "$SKIP_BLOCKS" -w 2 -f "$src" -t "$REMOTE:$upload_resume_dst"
expected_hash=$(sha256sum "$resume_expected" | cut -d' ' -f1)
resume_hash=$(remote_hash "$upload_resume_dst")
if [ "$expected_hash" != "$resume_hash" ]; then
  echo "remote upload resume mismatch: expected=$expected_hash destination=$resume_hash" >&2
  exit 1
fi

download_resume_dst="$local_dir/download-resume.img"
echo "Running SSH download: resume-skip"
seed_local_nonzero "$download_resume_dst"
run_transfer -q -d -b "$BLOCK_SIZE" -s "$SKIP_BLOCKS" -w 2 -f "$download_resume_dst" -t "$REMOTE:$remote_source"
if ! cmp -s "$resume_expected" "$download_resume_dst"; then
  echo "remote download resume mismatch" >&2
  exit 1
fi

# Empty-file transfers must truncate existing data through both SSH directions.
: > "$empty_src"
empty_remote_dst="$remote_dir/upload-empty.img"
echo "Running SSH upload: empty-source"
seed_remote_nonzero "$empty_remote_dst"
run_transfer -q -e -b "$BLOCK_SIZE" -f "$empty_src" -t "$REMOTE:$empty_remote_dst"
if [ "$(remote_hash "$empty_remote_dst")" != "$(sha256sum "$empty_src" | cut -d' ' -f1)" ]; then
  echo "remote empty upload did not truncate destination" >&2
  exit 1
fi

empty_download_dst="$local_dir/download-empty.img"
echo "Running SSH download: empty-source"
seed_local_nonzero "$empty_download_dst"
run_transfer -q -e -d -b "$BLOCK_SIZE" -f "$empty_download_dst" -t "$REMOTE:$empty_remote_dst"
if [ -s "$empty_download_dst" ]; then
  echo "remote empty download did not truncate destination" >&2
  exit 1
fi

# Moderate concurrent transfers exercise automatic per-process port selection,
# copied binary lifetime, larger payload boundaries, and simultaneous SSH work.
stress_src="$local_dir/stress-source.img"
dd if=/dev/urandom of="$stress_src" bs=1M count=8 status=none
dd if=/dev/zero of="$stress_src" bs="$STRESS_BLOCK_SIZE" seek=10 count=6 conv=notrunc status=none
truncate -s 8388731 "$stress_src"
stress_flags=("" "-n" "-L fast" "-e -n")
pids=()
echo "Running SSH uploads: concurrent-stress"
for i in "${!stress_flags[@]}"; do
  destination="$remote_dir/stress-upload-$i.img"
  # shellcheck disable=SC2086
  "$BSYNC_BIN" -q -b "$STRESS_BLOCK_SIZE" -w 2 -f "$stress_src" -t "$REMOTE:$destination" ${stress_flags[$i]} &
  transfer_pid=$!
  pids+=("$transfer_pid")
  remote_bin_pids+=("$transfer_pid")
done
for i in "${!pids[@]}"; do
  wait "${pids[$i]}"
  assert_no_remote_binary "${pids[$i]}"
  assert_remote_equals_file "$stress_src" "$remote_dir/stress-upload-$i.img"
done

remote_stress_source="$remote_dir/stress-upload-0.img"
pids=()
echo "Running SSH downloads: concurrent-stress"
for i in "${!stress_flags[@]}"; do
  destination="$local_dir/stress-download-$i.img"
  # shellcheck disable=SC2086
  "$BSYNC_BIN" -q -d -b "$STRESS_BLOCK_SIZE" -w 2 -f "$destination" -t "$REMOTE:$remote_stress_source" ${stress_flags[$i]} &
  transfer_pid=$!
  pids+=("$transfer_pid")
  remote_bin_pids+=("$transfer_pid")
done
for i in "${!pids[@]}"; do
  wait "${pids[$i]}"
  assert_no_remote_binary "${pids[$i]}"
  assert_local_equals_file "$stress_src" "$local_dir/stress-download-$i.img"
done

echo "Remote SSH upload/download profile matrix passed: $REMOTE"
