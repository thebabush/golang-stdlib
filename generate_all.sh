#!/usr/bin/env bash
# Build the stdlib-coverage program for every target, cross-compiling them all
# from a single host. The generator builds pure Go (cgo disabled), so NO C
# toolchain is needed for any GOOS/GOARCH — one Linux runner builds them all.
set -euo pipefail

version=$(go version | awk '{print $3}')
version="${version#go}" # "go1.26.4" -> "1.26.4" (no "go" prefix in artifact names)

# Per-(version, target) skip list — drop only the broken platform, build the rest.
# shellcheck source=scripts/skiplist.sh
source "$(dirname "$0")/scripts/skiplist.sh"
load_skips

# Build the generator natively for the host; it cross-builds each target.
ext=$(go env GOEXE)
gen="$PWD/gen${ext}"
go build -o "$gen" ./generate_std_usage.go

TARGETS=(
    "linux amd64"
    "linux arm64"
    "windows amd64"
    "darwin arm64"
)

for target in "${TARGETS[@]}"; do
    IFS=' ' read -r goos goarch <<<"$target"
    if is_skipped "$version" "${goos}/${goarch}"; then
        echo "Skipping ${goos}/${goarch} for ${version} (in skip-versions.txt)" >&2
        continue
    fi
    outdir="output/${goos}/${goarch}"
    mkdir -p "$outdir"
    echo "Generating ${goos}/${goarch} (${version})..."
    env GOOS="$goos" GOARCH="$goarch" "$gen" "$outdir" "go.${version}.${goos}.${goarch}"
    echo "Wrote binary into $outdir/"
done
