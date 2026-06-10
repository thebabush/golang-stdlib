#!/usr/bin/env bash
# Plan a backfill of stdlib builds, choosing versions by one of three modes
# (first that applies):
#
#   VERSIONS    explicit space/comma-separated list (e.g. "1.23.11 go1.24.0")
#   FROM+COUNT  COUNT stable versions counting backwards (older) from FROM
#   COUNT       the COUNT most-recent stable versions
#
# Versions already having a GitHub Release are dropped (unless FORCE=true). The
# remaining versions are emitted as a JSON array (without the leading "go",
# ready for setup-go) plus an "any" flag, on $GITHUB_OUTPUT.
#
# Env:
#   VERSIONS  explicit list; takes precedence over FROM/COUNT
#   FROM      anchor version to count backwards from, e.g. "1.23.11"
#   COUNT     how many versions (default 10)
#   FORCE     "true" to ignore existing releases
#   GH_TOKEN  token used by `gh release list` (set in CI)
set -euo pipefail

: "${VERSIONS:=}"
: "${FROM:=}"
: "${COUNT:=10}"
: "${FORCE:=false}"

# Normalize a version to the release-tag form "go1.23.11".
norm() { printf 'go%s' "${1#go}"; }

feed="https://go.dev/dl/?mode=json&include=all"

# Full stable list, newest-first: "go1.26.4" "go1.26.3" ...
all=()
while IFS= read -r v; do
  [[ -n "$v" ]] && all+=("$v")
done < <(curl -fsSL "$feed" | jq -r '[.[] | select(.stable == true) | .version] | .[]')

if [[ ${#all[@]} -eq 0 ]]; then
  echo "no stable versions returned by $feed" >&2
  exit 1
fi

candidates=()
if [[ -n "$VERSIONS" ]]; then
  # Explicit list: split on commas and whitespace.
  for v in ${VERSIONS//,/ }; do
    candidates+=("$(norm "$v")")
  done
elif [[ -n "$FROM" ]]; then
  anchor="$(norm "$FROM")"
  start=-1
  for i in "${!all[@]}"; do
    if [[ "${all[$i]}" == "$anchor" ]]; then
      start=$i
      break
    fi
  done
  if [[ $start -lt 0 ]]; then
    echo "anchor $anchor not found in the release feed" >&2
    exit 1
  fi
  # Increasing index == older, so this counts backwards from the anchor.
  for ((i = start; i < start + COUNT && i < ${#all[@]}; i++)); do
    candidates+=("${all[$i]}")
  done
else
  for ((i = 0; i < COUNT && i < ${#all[@]}; i++)); do
    candidates+=("${all[$i]}")
  done
fi

existing=""
if [[ "$FORCE" != "true" ]]; then
  existing=$(gh release list --limit 1000 --json tagName -q '.[].tagName' 2>/dev/null || true)
fi

# shellcheck source=scripts/skiplist.sh
source "$(dirname "$0")/skiplist.sh"
load_skips

needed=()
for v in "${candidates[@]}"; do # v is like "go1.23.11"
  if is_skipped "$v"; then
    echo "skip $v (in skip-versions.txt)" >&2
    continue
  fi
  if [[ "$FORCE" != "true" ]] && grep -qxF "${v#go}" <<<"$existing"; then
    echo "skip $v (release already exists)" >&2
    continue
  fi
  needed+=("${v#go}") # strip leading "go" -> "1.23.11" for setup-go / tag
done

if [[ ${#needed[@]} -eq 0 ]]; then
  matrix='[]'
  any='false'
else
  matrix=$(printf '%s\n' "${needed[@]}" | jq -R . | jq -s -c .)
  any='true'
fi

echo "backfill plan: ${needed[*]:-<none>}" >&2

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "matrix=$matrix"
    echo "any=$any"
  } >>"$GITHUB_OUTPUT"
else
  echo "matrix=$matrix"
  echo "any=$any"
fi
