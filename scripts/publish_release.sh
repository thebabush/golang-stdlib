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

if gh release view "$tag" >/dev/null 2>&1; then
  gh release upload "$tag" "$dist"/* --clobber
else
  gh release create "$tag" "$dist"/* --title "$tag" --notes "$notes"
fi
