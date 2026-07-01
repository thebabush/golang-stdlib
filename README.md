# turbo-golang

<!-- last-checked -->Last checked: 2026-07-01 (latest stable: go1.26.4)<!-- /last-checked -->

This repo contains helper scripts to build the Go standard library in different formats.

- `build_std_static.sh` builds each package of the standard library as static `.a` archives and runs a small test program against them.
- `build_std_shared.sh` builds the entire standard library as a single shared library `libstd.so` and validates it with a test program.
- `generate_std_usage.sh` lists exported symbols of all public standard packages, writes a `main.go` importing each package to mark one symbol as used, and compiles the program.
- `generate_std_usage.go` is the current generator: it loads each stdlib package's
  type information via `go/importer` and references every exported function and
  method, instantiating generics across a shape-diverse pool of type arguments
  (validated with `go/types`) to maximize the distinct symbols in the binary.
- `generate_all.sh` cross-compiles the generator's output for every target and
  places the binaries under `./output/<GOOS>/<GOARCH>/go.$GO_VERSION.$GOOS.$GOARCH`.
  The build is pure Go (cgo disabled), so a single Linux host builds all
  targets with no C toolchain.
