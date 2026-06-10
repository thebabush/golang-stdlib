#!/usr/bin/env bash
# Shared helper: the manual "skip these builds" list (skip-versions.txt at the
# repo root). Skips are per (version, target) so a single broken platform
# doesn't drop the whole version. Source this file, call load_skips once, then
# test with is_skipped <version> <goos/goarch>.
#
# File format: one line per version, "<version> <goos/goarch> [<goos/goarch>...]".
# "#" comments and blank lines are ignored; a leading "go" on the version is
# optional. Example:  1.23.0 windows/amd64
#
# Override the file location with SKIP_FILE (used in tests).

load_skips() {
  local skipfile="${SKIP_FILE:-$(dirname "${BASH_SOURCE[0]}")/../skip-versions.txt}"
  SKIPS=""
  [[ -f "$skipfile" ]] || return 0
  local line ver t
  local -a fields
  while IFS= read -r line; do
    line="${line%%#*}" # strip comment
    read -ra fields <<<"$line"
    [[ ${#fields[@]} -lt 2 ]] && continue # need a version and >=1 target
    ver="${fields[0]#go}"
    for t in "${fields[@]:1}"; do
      SKIPS+="${ver} ${t}"$'\n'
    done
  done <"$skipfile"
}

# is_skipped <version> <goos/goarch>
is_skipped() { grep -qxF "${1#go} $2" <<<"$SKIPS"; }
