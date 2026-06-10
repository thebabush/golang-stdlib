#!/usr/bin/env bash
# Decide which Go versions still need a stdlib build.
#
# Scrapes the official Go release feed, keeps the K most-recent stable
# versions, and drops any that already have a GitHub Release (the dedup
# key is the release tag "go<version>"). Emits a JSON array of the
# remaining versions (without the leading "go", ready for setup-go) plus
# an "any" flag, on $GITHUB_OUTPUT.
#
# Env:
#   LIMIT  how many most-recent stable versions to consider (default 6)
#   FORCE  "true" to ignore existing releases and rebuild everything
#   GH_TOKEN  token used by `gh release list` (set in CI)
set -euo pipefail

: "${LIMIT:=6}"
: "${FORCE:=false}"

feed="https://go.dev/dl/?mode=json&include=all"

# The feed is already ordered newest-first; preserve that order and take
# the first K stable versions (e.g. go1.26.3, go1.26.2, ...).
candidates=()
while IFS= read -r v; do
  [[ -n "$v" ]] && candidates+=("$v")
done < <(
  curl -fsSL "$feed" |
    jq -r --argjson k "$LIMIT" '[.[] | select(.stable == true) | .version][0:$k] | .[]'
)

if [[ ${#candidates[@]} -eq 0 ]]; then
  echo "no stable versions returned by $feed" >&2
  exit 1
fi

existing=""
if [[ "$FORCE" != "true" ]]; then
  # Tags look like "go1.26.3". Missing/unauth gh should not abort the run.
  existing=$(gh release list --limit 1000 --json tagName -q '.[].tagName' 2>/dev/null || true)
fi

needed=()
for v in "${candidates[@]}"; do # v is like "go1.26.3"
  if [[ "$FORCE" != "true" ]] && grep -qxF "${v#go}" <<<"$existing"; then
    echo "skip $v (release already exists)" >&2
    continue
  fi
  needed+=("${v#go}") # strip leading "go" -> "1.26.3" for setup-go / tag
done

if [[ ${#needed[@]} -eq 0 ]]; then
  matrix='[]'
  any='false'
else
  matrix=$(printf '%s\n' "${needed[@]}" | jq -R . | jq -s -c .)
  any='true'
fi

echo "planning to build: ${needed[*]:-<none>}" >&2

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "matrix=$matrix"
    echo "any=$any"
  } >>"$GITHUB_OUTPUT"
else
  echo "matrix=$matrix"
  echo "any=$any"
fi
