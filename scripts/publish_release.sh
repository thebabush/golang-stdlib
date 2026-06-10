#!/usr/bin/env bash
# Publish the generated stdlib binaries for one Go version as a GitHub Release.
#
# generate_all.sh writes, per target:
#   output/<GOOS>/<GOARCH>/go.<version>.<GOOS>.<GOARCH>[.exe]
#   output/<GOOS>/<GOARCH>/symbols.txt
# Binary names already encode GOOS/GOARCH; symbols.txt does not, so it is
# renamed per platform before upload to avoid asset collisions.
#
# Usage: publish_release.sh <tag>     # tag is the bare Go version, e.g. "1.26.3"
# Env:   GH_TOKEN  token with contents:write (set in CI)
set -euo pipefail

tag="${1:?usage: publish_release.sh <tag e.g. go1.26.3>}"

dist="$(mktemp -d)"
trap 'rm -rf "$dist"' EXIT

shopt -s nullglob
found=0
for bin in output/*/*/go.*; do
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

# Mark this release as "Latest" only if it's the highest version we have.
# GitHub can't infer that from the non-semver "go<ver>" tags, so left to itself
# it picks the most-recently-created release — which is wrong during backfills
# (older versions get published later). sort -V orders the go tags correctly.
all_tags=$(gh release list --limit 1000 --json tagName -q '.[].tagName' 2>/dev/null || true)
newest=$(printf '%s\n%s\n' "$all_tags" "$tag" | grep -v '^$' | sort -uV | tail -1)
if [[ "$tag" == "$newest" ]]; then
  latest=true
else
  latest=false
fi
echo "mark latest=$latest for $tag (newest is $newest)" >&2

# Create the release, or upload into it if it already exists. The trailing
# `|| upload` covers the create-vs-create race (two jobs publish the same tag):
# whichever loses the create falls back to clobbering assets in.
publish() {
  if gh release view "$tag" >/dev/null 2>&1; then
    gh release upload "$tag" "$dist"/* --clobber
    gh release edit "$tag" --latest="$latest"
  else
    gh release create "$tag" "$dist"/* --title "$tag" --notes "$notes" --latest="$latest" ||
      { gh release upload "$tag" "$dist"/* --clobber && gh release edit "$tag" --latest="$latest"; }
  fi
}

retry publish
