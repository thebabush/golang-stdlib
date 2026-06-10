// Legacy generator for pre-1.18 Go toolchains (no generics).
//
// The main generator (../generate_std_usage.go) uses go/types generics APIs
// (types.Instantiate, TypeParams, IsMethodSet, ...) that don't exist before Go
// 1.18, so it won't even compile on older toolchains. This variant enumerates
// exported funcs and methods via go/importer + go/types using only pre-1.18
// APIs. Pre-1.18 stdlib has no generics, so there's nothing to instantiate.
//
// For cross targets on pre-1.20, pre-build the target stdlib first
// (GOOS=.. GOARCH=.. go install std) so go/importer can read its export data.
//
// Usage: generate_std_usage.go [outdir] [binname]   (defaults: output/std_usage main)
package main

import (
	"bytes"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var fset = token.NewFileSet()

func skipPackage(p string) bool {
	switch {
	case strings.Contains(p, "/internal/") || strings.HasPrefix(p, "internal/") || strings.HasSuffix(p, "/internal"):
		return true
	case strings.Contains(p, "/vendor/") || strings.HasPrefix(p, "vendor/"):
		return true
	case strings.HasPrefix(p, "cmd/") || p == "cmd":
		return true
	case p == "runtime/cgo", p == "runtime/race", p == "unsafe":
		return true
	}
	return false
}

func listStd() ([]string, error) {
	out, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, p := range strings.Fields(string(out)) {
		if !skipPackage(p) {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

func sanitize(p string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, p)
}

func objKind(o types.Object) string {
	switch o.(type) {
	case *types.Func:
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Const:
		return "const"
	case *types.Var:
		return "var"
	}
	return "other"
}

func main() {
	outDir := "output/std_usage"
	binName := "main"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if len(os.Args) > 2 {
		binName = os.Args[2]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	pkgs, err := listStd()
	if err != nil {
		log.Fatal(err)
	}
	imp := importer.ForCompiler(fset, "gc", nil)

	alias := map[string]string{}
	taken := map[string]bool{}
	aliasFor := func(path string) string {
		if a, ok := alias[path]; ok {
			return a
		}
		base := sanitize(path)
		a := base
		for n := 1; taken[a]; n++ {
			a = fmt.Sprintf("%s_%d", base, n)
		}
		taken[a] = true
		alias[path] = a
		return a
	}

	var fnEntries []string
	var syms bytes.Buffer
	contributed := map[string]bool{}

	for _, pp := range pkgs {
		tpkg, err := imp.Import(pp)
		if err != nil {
			log.Printf("skip %s: %v", pp, err)
			continue
		}
		a := aliasFor(pp)
		scope := tpkg.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if !obj.Exported() {
				continue
			}
			fmt.Fprintf(&syms, "%s: %s %s\n", pp, objKind(obj), name)
			switch o := obj.(type) {
			case *types.Func:
				// Pre-1.18: no generic funcs, reference directly.
				fnEntries = append(fnEntries, fmt.Sprintf("%s.%s", a, name))
				contributed[pp] = true
			case *types.TypeName:
				named, ok := o.Type().(*types.Named)
				if !ok {
					continue
				}
				recv := fmt.Sprintf("%s.%s", a, name)
				mset := types.NewMethodSet(types.NewPointer(named))
				for i := 0; i < mset.Len(); i++ {
					fn, ok := mset.At(i).Obj().(*types.Func)
					if !ok || !fn.Exported() {
						continue
					}
					fnEntries = append(fnEntries, fmt.Sprintf("(*%s).%s", recv, fn.Name()))
					contributed[pp] = true
				}
			}
		}
	}

	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, pp := range pkgs {
		if contributed[pp] {
			fmt.Fprintf(&b, "\t%s \"%s\"\n", alias[pp], pp)
		} else {
			fmt.Fprintf(&b, "\t_ \"%s\"\n", pp)
		}
	}
	b.WriteString(")\n\nvar fns = []interface{}{\n")
	for _, fn := range fnEntries {
		fmt.Fprintf(&b, "\t%s,\n", fn)
	}
	b.WriteString("}\n\n//go:noinline\nfunc keep(_ interface{}) {}\nfunc init() { keep(fns) }\n\nfunc main() {}\n")

	symsPath := filepath.Join(outDir, "symbols.txt")
	if err := os.WriteFile(symsPath, syms.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	mainPath := filepath.Join(outDir, "main.go")
	if err := os.WriteFile(mainPath, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s and %s (%d fn/method refs)\n", symsPath, mainPath, len(fnEntries))

	outBin := filepath.Join(outDir, binName)
	if os.Getenv("GOOS") == "windows" {
		outBin += ".exe"
	}
	cmd := exec.Command("go", "build", "-gcflags=-l", "-o", outBin, mainPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("build failed: %v", err)
	}
	fmt.Printf("compiled %s\n", outBin)
}
