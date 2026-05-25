#!/usr/bin/env bash

set -euo pipefail

BSYNC_BIN="${BSYNC_BIN:-./bsync}"
BLOCK_SIZE=65536
tmp_dir=$(mktemp -d /tmp/bsync-stress.XXXXXX)
trap 'rm -rf "$tmp_dir"' EXIT

src="$tmp_dir/source.img"
dd if=/dev/urandom of="$src" bs=1M count=8 status=none
dd if=/dev/zero of="$src" bs="$BLOCK_SIZE" seek=10 count=6 conv=notrunc status=none
truncate -s 8388731 "$src"

upload_flags=("" "-n" "-L fast" "-e -n")
pids=()
for i in "${!upload_flags[@]}"; do
  dst="$tmp_dir/upload-$i.img"
  # shellcheck disable=SC2086
  "$BSYNC_BIN" -q -b "$BLOCK_SIZE" -f "$src" -t "$dst" ${upload_flags[$i]} &
  pids+=("$!")
done
for i in "${!pids[@]}"; do
  wait "${pids[$i]}"
  cmp "$src" "$tmp_dir/upload-$i.img"
done

download_flags=("" "-n" "-L better" "-e")
pids=()
for i in "${!download_flags[@]}"; do
  dst="$tmp_dir/download-$i.img"
  # shellcheck disable=SC2086
  "$BSYNC_BIN" -q -d -b "$BLOCK_SIZE" -f "$dst" -t "$src" ${download_flags[$i]} &
  pids+=("$!")
done
for i in "${!pids[@]}"; do
  wait "${pids[$i]}"
  cmp "$src" "$tmp_dir/download-$i.img"
done

echo "Concurrent local -t stress matrix passed"
