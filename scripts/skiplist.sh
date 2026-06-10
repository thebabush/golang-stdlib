#!/usr/bin/env bash
# Shared helper: the manual "never build these" list (skip-versions.txt at the
# repo root). Source this file, call load_skips once, then test with
# is_skipped <version>. Versions are compared with the leading "go" stripped.
#
# Override the file location with SKIP_FILE (used in tests).

load_skips() {
  local skipfile="${SKIP_FILE:-$(dirname "${BASH_SOURCE[0]}")/../skip-versions.txt}"
  SKIPS=""
  [[ -f "$skipfile" ]] || return 0
  local line
  while IFS= read -r line; do
    line="${line%%#*}"            # strip comment
    line="${line//[[:space:]]/}"  # strip all whitespace
    [[ -z "$line" ]] && continue
    SKIPS+="${line#go}"$'\n'      # store without leading "go"
  done <"$skipfile"
}

is_skipped() { grep -qxF "${1#go}" <<<"$SKIPS"; }
