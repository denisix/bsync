#!/usr/bin/env bash

set -euo pipefail

BSYNC_BIN="${BSYNC_BIN:-./bsync}"
tmp_dir=$(mktemp -d /tmp/bsync-cli-fault.XXXXXX)
trap 'rm -rf "$tmp_dir"' EXIT

if [ ! -x "$BSYNC_BIN" ]; then
  echo "bsync binary is not executable: $BSYNC_BIN" >&2
  exit 2
fi

expect_failure() {
  local name=$1
  shift
  if "$BSYNC_BIN" "$@" >"$tmp_dir/$name.log" 2>&1; then
    echo "expected failure succeeded: $name" >&2
    exit 1
  fi
}

src="$tmp_dir/source.img"
printf 'source-data' > "$src"

expect_failure zero-block-size -b 0 -f "$src" -r 127.0.0.1:1
expect_failure zero-workers -w 0 -f "$src" -r 127.0.0.1:1
expect_failure negative-prefetch -prefetch -1 -f "$src" -r 127.0.0.1:1
expect_failure invalid-key -K invalid -f "$src" -p 0
expect_failure direct-encryption-without-key -e -f "$src" -r 127.0.0.1:1
expect_failure missing-source -f "$tmp_dir/missing.img" -r 127.0.0.1:1

echo "CLI invalid-input matrix passed"
