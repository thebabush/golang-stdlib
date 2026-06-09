# Repo Guidelines

- Format Go code with `gofmt -w` before committing.
- Validate `generate_std_usage.go` by running `go build generate_std_usage.go` followed by `go run generate_std_usage.go`.
- The generator must create `output/std_usage/main.go` and compile a binary at `output/std_usage/main`.
- Do **not** commit anything under `output/`.
- To count functions in the generated binary, run `go tool nm output/std_usage/main | grep ' T ' | wc -l`.
- To run the generator and count the functions in the generated binary, use `./score.sh`
- Do not build the generated program with `-gcflags=all=-l`; disabling inlining across
  the entire standard library is not allowed. You may disable inlining for the main
  package only with `-gcflags=-l`.
- Generic functions should be referenced so that their instantiations remain present in the compiled binary.
