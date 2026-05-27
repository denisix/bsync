#!/usr/bin/env bash

set -euo pipefail

BSYNC_BIN="${BSYNC_BIN:-./bsync}"
REMOTE="${REMOTE:-}"

# Matrix dimensions. Every transfer is verified by checksum (uploads, server
# side) or byte-for-byte compare (downloads) so a single wrong byte fails fast.
WORKERS=(1 2 4 8)
DEFAULT_BS=65536
BLOCK_SIZES=(4096 65536 1048576)
PATTERNS=(zero ff mixed unaligned)
# Representative subset used to sweep block sizes and compression/encryption
# modes without exploding the full matrix.
SWEEP_PATTERNS=(mixed unaligned)
SWEEP_BLOCK_SIZES=(4096 1048576)
# "label|flags" — flags are word-split intentionally when passed to bsync.
MODES=(
  "default|"
  "no-compression|-n"
  "fast|-L fast"
  "best|-L best"
  "encrypted|-e"
  "enc-nocomp|-e -n"
)

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

ssh_remote() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" "$@"
}

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
  orphan=$(ssh_remote "find /tmp -maxdepth 1 -name 'bsync-$transfer_pid-*' -print -quit")
  if [ -n "$orphan" ]; then
    echo "remote executable was not removed: $orphan" >&2
    exit 1
  fi
}

remote_hash() {
  ssh_remote "sha256sum '$1'" | cut -d' ' -f1
}

# Pre-fill a remote/local destination with non-zero (0xFF) bytes so each test
# proves bsync overwrites stale data and clears zero ranges.
seed_remote_nonzero() {
  ssh_remote "head -c $2 /dev/zero | tr '\\000' '\\377' > '$1'"
}

seed_local_nonzero() {
  head -c "$2" /dev/zero | tr '\000' '\377' > "$1"
}

assert_remote_equals_file() {
  local source=$1 destination=$2 source_hash destination_hash
  source_hash=$(sha256sum "$source" | cut -d' ' -f1)
  destination_hash=$(remote_hash "$destination")
  if [ "$source_hash" != "$destination_hash" ]; then
    echo "remote upload mismatch: dst=$destination source=$source_hash destination=$destination_hash" >&2
    exit 1
  fi
}

assert_local_equals_file() {
  local source=$1 destination=$2
  if ! cmp -s "$source" "$destination"; then
    echo "remote download mismatch: $destination" >&2
    exit 1
  fi
}

# Block count scaled per block size: small blocks get more blocks, large blocks
# fewer, keeping fixtures multi-block but bounded in total size.
blocks_for_bs() {
  case "$1" in
    4096) echo 16 ;;
    65536) echo 8 ;;
    *) echo 4 ;;
  esac
}

# make_pattern <file> <pattern> <block_size> <block_count>
make_pattern() {
  local file=$1 pattern=$2 bs=$3 count=$4 idx
  case "$pattern" in
    zero)
      head -c $((bs * count)) /dev/zero > "$file"
      ;;
    ff)
      head -c $((bs * count)) /dev/zero | tr '\000' '\377' > "$file"
      ;;
    mixed)
      dd if=/dev/urandom of="$file" bs="$bs" count="$count" status=none
      # Zero out even-indexed blocks to exercise zero-omission and clearing.
      idx=0
      while [ "$idx" -lt "$count" ]; do
        dd if=/dev/zero of="$file" bs="$bs" seek="$idx" count=1 conv=notrunc status=none
        idx=$((idx + 2))
      done
      ;;
    unaligned)
      make_pattern "$file" mixed "$bs" "$count"
      # Append a partial trailing block so the file size is not a block multiple.
      dd if=/dev/urandom bs=$((bs / 2)) count=1 status=none >> "$file"
      ;;
    *)
      echo "unknown pattern: $pattern" >&2
      exit 2
      ;;
  esac
}

# Generate each fixture once and reuse it. This is called via command
# substitution ($(get_local_fixture ...)), which runs in a subshell, so an
# in-memory cache (associative array) set here would NOT persist to the parent
# and make_pattern would re-run on every call, producing fresh random content
# each time. Cache by file existence instead, which survives across subshells.
get_local_fixture() {
  local pattern=$1 bs=$2 f="$local_dir/fix-$pattern-$bs.img"
  if [ ! -f "$f" ]; then
    make_pattern "$f" "$pattern" "$bs" "$(blocks_for_bs "$bs")"
  fi
  printf '%s' "$f"
}

# Upload a verified remote source fixture once per (pattern, block size) and
# reuse it as the source for all download profiles.
declare -A remote_src_path
declare -A remote_src_local
prepare_remote_source() {
  local pattern=$1 bs=$2 key="$pattern.$bs" lf rf
  if [ -n "${remote_src_path[$key]:-}" ]; then return; fi
  lf=$(get_local_fixture "$pattern" "$bs")
  rf="$remote_dir/srcfix-$pattern-$bs.img"
  run_transfer -q -b "$bs" -w 2 -f "$lf" -t "$REMOTE:$rf"
  assert_remote_equals_file "$lf" "$rf"
  remote_src_path[$key]="$rf"
  remote_src_local[$key]="$lf"
}

# do_upload <label> <pattern> <bs> <workers> [flags...]
do_upload() {
  local label=$1 pattern=$2 bs=$3 w=$4; shift 4
  local flags="$*" f bytes dst
  f=$(get_local_fixture "$pattern" "$bs")
  bytes=$(stat -c%s "$f")
  dst="$remote_dir/up-$label.img"
  echo "Running SSH upload: $label"
  seed_remote_nonzero "$dst" $((bytes + bs))
  # shellcheck disable=SC2086
  run_transfer -q -b "$bs" -w "$w" -f "$f" -t "$REMOTE:$dst" $flags
  assert_remote_equals_file "$f" "$dst"
}

# do_download <label> <pattern> <bs> <workers> [flags...]
do_download() {
  local label=$1 pattern=$2 bs=$3 w=$4; shift 4
  local flags="$*" key="$pattern.$bs" rf lf bytes dst
  prepare_remote_source "$pattern" "$bs"
  rf="${remote_src_path[$key]}"
  lf="${remote_src_local[$key]}"
  bytes=$(stat -c%s "$lf")
  dst="$local_dir/dl-$label.img"
  echo "Running SSH download: $label"
  seed_local_nonzero "$dst" $((bytes + bs))
  # shellcheck disable=SC2086
  run_transfer -q -d -b "$bs" -w "$w" -f "$dst" -t "$REMOTE:$rf" $flags
  assert_local_equals_file "$lf" "$dst"
}

echo "Checking SSH access to $REMOTE"
ssh_remote "mkdir -p '$remote_dir'"

# --- Grid A: every data pattern x every worker count, default block size/mode.
for pattern in "${PATTERNS[@]}"; do
  for w in "${WORKERS[@]}"; do
    do_upload "A-$pattern-w$w" "$pattern" "$DEFAULT_BS" "$w"
    do_download "A-$pattern-w$w" "$pattern" "$DEFAULT_BS" "$w"
  done
done

# --- Grid B: block-size sweep on representative patterns x every worker count.
for pattern in "${SWEEP_PATTERNS[@]}"; do
  for bs in "${SWEEP_BLOCK_SIZES[@]}"; do
    for w in "${WORKERS[@]}"; do
      do_upload "B-$pattern-bs$bs-w$w" "$pattern" "$bs" "$w"
      do_download "B-$pattern-bs$bs-w$w" "$pattern" "$bs" "$w"
    done
  done
done

# --- Grid C: compression/encryption mode sweep x every worker count.
for entry in "${MODES[@]}"; do
  mode_label="${entry%%|*}"
  mode_flags="${entry#*|}"
  for pattern in "${SWEEP_PATTERNS[@]}"; do
    for w in "${WORKERS[@]}"; do
      do_upload "C-$mode_label-$pattern-w$w" "$pattern" "$DEFAULT_BS" "$w" $mode_flags
      do_download "C-$mode_label-$pattern-w$w" "$pattern" "$DEFAULT_BS" "$w" $mode_flags
    done
  done
done

# --- Quoted path: punctuation/spaces must arrive as path data, not shell input.
quoted_fixture=$(get_local_fixture mixed "$DEFAULT_BS")
quoted_bytes=$(stat -c%s "$quoted_fixture")
quoted_remote_dst="$remote_dir/upload with spaces;still-data.img"
echo "Running SSH upload: quoted-remote-path"
seed_remote_nonzero "$quoted_remote_dst" $((quoted_bytes + DEFAULT_BS))
run_transfer -q -b "$DEFAULT_BS" -w 2 -f "$quoted_fixture" -t "$REMOTE:$quoted_remote_dst" -L fast
assert_remote_equals_file "$quoted_fixture" "$quoted_remote_dst"

# --- Resume (-s): skipped prefix is preserved, only the suffix is transferred.
RBS=4096
RCOUNT=6
RSKIP=2
resume_src="$local_dir/resume-src.img"
resume_expected="$local_dir/resume-expected.img"
make_pattern "$resume_src" mixed "$RBS" "$RCOUNT"
resume_bytes=$(stat -c%s "$resume_src")
seed_local_nonzero "$resume_expected" "$resume_bytes"
dd if="$resume_src" of="$resume_expected" bs="$RBS" skip="$RSKIP" seek="$RSKIP" conv=notrunc status=none

upload_resume_dst="$remote_dir/upload-resume.img"
echo "Running SSH upload: resume-skip"
seed_remote_nonzero "$upload_resume_dst" "$resume_bytes"
run_transfer -q -b "$RBS" -s "$RSKIP" -w 2 -f "$resume_src" -t "$REMOTE:$upload_resume_dst"
expected_hash=$(sha256sum "$resume_expected" | cut -d' ' -f1)
resume_hash=$(remote_hash "$upload_resume_dst")
if [ "$expected_hash" != "$resume_hash" ]; then
  echo "remote upload resume mismatch: expected=$expected_hash destination=$resume_hash" >&2
  exit 1
fi

# Upload a verified remote copy of the resume source to download from.
remote_resume_source="$remote_dir/resume-source.img"
run_transfer -q -b "$RBS" -w 2 -f "$resume_src" -t "$REMOTE:$remote_resume_source"
assert_remote_equals_file "$resume_src" "$remote_resume_source"

download_resume_dst="$local_dir/download-resume.img"
echo "Running SSH download: resume-skip"
seed_local_nonzero "$download_resume_dst" "$resume_bytes"
run_transfer -q -d -b "$RBS" -s "$RSKIP" -w 2 -f "$download_resume_dst" -t "$REMOTE:$remote_resume_source"
if ! cmp -s "$resume_expected" "$download_resume_dst"; then
  echo "remote download resume mismatch" >&2
  exit 1
fi

# --- Empty source: must truncate an existing destination in both directions.
empty_src="$local_dir/empty-source.img"
: > "$empty_src"
empty_remote_dst="$remote_dir/upload-empty.img"
echo "Running SSH upload: empty-source"
seed_remote_nonzero "$empty_remote_dst" "$DEFAULT_BS"
run_transfer -q -e -b "$DEFAULT_BS" -f "$empty_src" -t "$REMOTE:$empty_remote_dst"
if [ "$(remote_hash "$empty_remote_dst")" != "$(sha256sum "$empty_src" | cut -d' ' -f1)" ]; then
  echo "remote empty upload did not truncate destination" >&2
  exit 1
fi

empty_download_dst="$local_dir/download-empty.img"
echo "Running SSH download: empty-source"
seed_local_nonzero "$empty_download_dst" "$DEFAULT_BS"
run_transfer -q -e -d -b "$DEFAULT_BS" -f "$empty_download_dst" -t "$REMOTE:$empty_remote_dst"
if [ -s "$empty_download_dst" ]; then
  echo "remote empty download did not truncate destination" >&2
  exit 1
fi

# --- Concurrent transfers: per-process port selection, copied-binary lifetime,
# larger payload boundaries, and simultaneous SSH work under multiple workers.
STRESS_BLOCK_SIZE=65536
stress_src="$local_dir/stress-source.img"
dd if=/dev/urandom of="$stress_src" bs=1M count=8 status=none
dd if=/dev/zero of="$stress_src" bs="$STRESS_BLOCK_SIZE" seek=10 count=6 conv=notrunc status=none
truncate -s 8388731 "$stress_src"
stress_flags=("" "-n" "-L fast" "-e -n")
stress_workers=(1 2 4 4)
pids=()
echo "Running SSH uploads: concurrent-stress"
for i in "${!stress_flags[@]}"; do
  destination="$remote_dir/stress-upload-$i.img"
  # shellcheck disable=SC2086
  "$BSYNC_BIN" -q -b "$STRESS_BLOCK_SIZE" -w "${stress_workers[$i]}" -f "$stress_src" -t "$REMOTE:$destination" ${stress_flags[$i]} &
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
  "$BSYNC_BIN" -q -d -b "$STRESS_BLOCK_SIZE" -w "${stress_workers[$i]}" -f "$destination" -t "$REMOTE:$remote_stress_source" ${stress_flags[$i]} &
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
