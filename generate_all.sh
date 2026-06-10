#!/usr/bin/env bash
# Build the stdlib-coverage program for every target, cross-compiling them all
# from a single host. The generator builds pure Go (cgo disabled), so NO C
# toolchain is needed for any GOOS/GOARCH — one Linux runner builds them all.
set -euo pipefail

version=$(go version | awk '{print $3}')
version="${version#go}"                                      # "go1.26.4" -> "1.26.4"
[[ "$version" =~ ^[0-9]+\.[0-9]+$ ]] && version="${version}.0" # "1.20" -> "1.20.0"

# Per-(version, target) skip list — drop only the broken platform, build the rest.
# shellcheck source=scripts/skiplist.sh
source "$(dirname "$0")/scripts/skiplist.sh"
load_skips

# Pre-1.20 ships precompiled stdlib only for the host arch, so go/importer (used
# by the generator to enumerate the API) can't read cross-target export data. For
# those versions, build the target's stdlib first so the importer has it. From
# 1.20 the toolchain builds export data on demand, so this is unnecessary.
prebuild=false
[[ "$(printf '%s\n1.20\n' "$version" | sort -V | head -1)" != "1.20" ]] && prebuild=true

# Pick the generator by version: the main one uses go/types generics APIs and
# only compiles on Go >= 1.18; the legacy one is generics-free for < 1.18 (and
# would emit broken bare-generic refs on >= 1.18). Mandatory, not an optimization.
if [[ "$(printf '%s\n1.18\n' "$version" | sort -V | head -1)" != "1.18" ]]; then
    gensrc="./legacy/generate_std_usage.go"
    tolerant=true # old toolchains have per-target quirks (e.g. pre-1.16
                  # darwin/arm64 means iOS and needs cgo); skip failing targets
                  # and ship the rest rather than dropping the whole version.
    echo "Using LEGACY (generics-free) generator for ${version}" >&2
else
    gensrc="./generate_std_usage.go"
    tolerant=false # 1.18+: every target must build, so regressions surface.
fi

# Build the generator natively for the host; it cross-builds each target.
ext=$(go env GOEXE)
gen="$PWD/gen${ext}"
go build -o "$gen" "$gensrc"

# Targets this toolchain actually supports (e.g. darwin/arm64 only exists from
# Go 1.16). Auto-skip the rest so old versions ship their valid arches instead
# of failing the whole job.
supported=$(go tool dist list)

TARGETS=(
    "linux amd64"
    "linux arm64"
    "windows amd64"
    "darwin arm64"
)

for target in "${TARGETS[@]}"; do
    IFS=' ' read -r goos goarch <<<"$target"
    if ! grep -qx "${goos}/${goarch}" <<<"$supported"; then
        echo "Skipping ${goos}/${goarch} for ${version} (not supported by this toolchain)" >&2
        continue
    fi
    if is_skipped "$version" "${goos}/${goarch}"; then
        echo "Skipping ${goos}/${goarch} for ${version} (in skip-versions.txt)" >&2
        continue
    fi
    outdir="output/${goos}/${goarch}"
    mkdir -p "$outdir"
    ok=true
    if $prebuild; then
        echo "Pre-building stdlib for ${goos}/${goarch} (Go ${version} < 1.20)..."
        GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go install std >/dev/null || ok=false
    fi
    if $ok; then
        echo "Generating ${goos}/${goarch} (${version})..."
        env GOOS="$goos" GOARCH="$goarch" "$gen" "$outdir" "go.${version}.${goos}.${goarch}" || ok=false
    fi
    if $ok; then
        echo "Wrote binary into $outdir/"
    else
        rm -rf "$outdir" # drop partial output so publish doesn't ship an orphan target
        if $tolerant; then
            echo "WARNING: ${goos}/${goarch} failed for ${version} — skipping (tolerant lane)" >&2
            continue
        fi
        echo "ERROR: ${goos}/${goarch} build failed for ${version}" >&2
        exit 1
    fi
done
