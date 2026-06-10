# turbo-golang

<!-- last-checked -->Last checked: 2026-06-10 (latest stable: go1.26.4)<!-- /last-checked -->

This repo contains helper scripts to build the Go standard library in different formats.

- `build_std_static.sh` builds each package of the standard library as static `.a` archives and runs a small test program against them.
- `build_std_shared.sh` builds the entire standard library as a single shared library `libstd.so` and validates it with a test program.
- `generate_std_usage.sh` lists exported symbols of all public standard packages, writes a `main.go` importing each package to mark one symbol as used, and compiles the program.
- `generate_std_usage.go` is a Go reimplementation of the above script that performs the same steps using the Go parser and build tools.
- `generate_all.sh` cross-compiles the generator's output for every target and
  places the binaries under `./output/<GOOS>/<GOARCH>/golang-std.$GO_VERSION.$GOOS.$GOARCH`.
  The build is pure Go (cgo disabled), so a single Linux host builds all
  targets with no C toolchain.
