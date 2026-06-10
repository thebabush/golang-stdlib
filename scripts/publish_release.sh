#!/usr/bin/env bash
# Publish the generated stdlib binaries for one Go version as a GitHub Release.
#
# generate_all.sh writes, per target:
#   output/<GOOS>/<GOARCH>/golang-std.<goversion>.<GOOS>.<GOARCH>[.exe]
#   output/<GOOS>/<GOARCH>/symbols.txt
# Binary names already encode GOOS/GOARCH; symbols.txt does not, so it is
# renamed per platform before upload to avoid asset collisions.
#
# Usage: publish_release.sh <tag>     # tag is the Go version, e.g. "go1.26.3"
# Env:   GH_TOKEN  token with contents:write (set in CI)
set -euo pipefail

tag="${1:?usage: publish_release.sh <tag e.g. go1.26.3>}"

dist="$(mktemp -d)"
trap 'rm -rf "$dist"' EXIT

shopt -s nullglob
found=0
for bin in output/*/*/golang-std.*; do
  cp "$bin" "$dist/$(basename "$bin")"
  found=1
done
for sym in output/*/*/symbols.txt; do
  rel="${sym#output/}"        # linux/amd64/symbols.txt
  goos="${rel%%/*}"           # linux
  rest="${rel#*/}"            # amd64/symbols.txt
  goarch="${rest%%/*}"        # amd64
  cp "$sym" "$dist/symbols.${tag}.${goos}.${goarch}.txt"
done

if [[ "$found" -eq 0 ]]; then
  echo "no binaries found under output/ for $tag" >&2
  exit 1
fi

echo "assets for $tag:" >&2
ls -1 "$dist" >&2

notes="Symbol-maximizing Go standard-library binaries for ${tag}: a single program that references as much of the stdlib as possible, compiled per GOOS/GOARCH, with the matching exported-symbol list."

# Retry transient GitHub API failures (sporadic HTTP 401/5xx, secondary rate
# limits) with exponential backoff.
retry() {
  local n=0 max="${RETRY_MAX:-5}" delay="${RETRY_DELAY:-5}"
  until "$@"; do
    n=$((n + 1))
    if [[ $n -ge $max ]]; then
      echo "command failed after ${max} attempts: $*" >&2
      return 1
    fi
    echo "attempt ${n}/${max} failed; retrying in ${delay}s: $*" >&2
    sleep "$delay"
    delay=$((delay * 2))
  done
}

# Create the release, or upload into it if it already exists. The trailing
# `|| upload` covers the create-vs-create race (two jobs publish the same tag):
# whichever loses the create falls back to clobbering assets in.
publish() {
  if gh release view "$tag" >/dev/null 2>&1; then
    gh release upload "$tag" "$dist"/* --clobber
  else
    gh release create "$tag" "$dist"/* --title "$tag" --notes "$notes" ||
      gh release upload "$tag" "$dist"/* --clobber
  fi
}

retry publish
