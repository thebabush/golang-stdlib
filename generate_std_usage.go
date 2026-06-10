// generate_std_usage.go builds a program that references as much of the Go
// standard library as possible, so the compiled binary contains the maximum
// number of stdlib function/method symbols (including many generic
// instantiations).
//
// It loads each package's type information via go/importer (no module, no
// dependencies) and asks the type checker for the API surface. Generic
// functions and types are instantiated across a shape-diverse pool of type
// arguments; every tuple is validated in-process with
// types.Instantiate(validate=true) before emission, so a generic that can't be
// satisfied is skipped rather than producing a program that won't compile.
//
// Type arguments span distinct gcshapes (integer widths, float, bool, string,
// pointer, struct, slice, map): the compiler stencils generics per gcshape, so
// each distinct shape yields a distinct compiled body — and ~94% of them are
// byte-distinct machine code, i.e. genuinely different symbols.
//
// GOOS/GOARCH are read from the environment (set by generate_all.sh), so the
// same host binary emits target-specific output for every platform.
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
	case p == "runtime/cgo", p == "runtime/race":
		return true
	case p == "unsafe":
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

func isInternal(path string) bool {
	return strings.Contains(path, "/internal/") || strings.HasPrefix(path, "internal/") || strings.HasSuffix(path, "/internal")
}

// importable reports whether a type can be named in generated code: every named
// type it references must live in a non-internal package and be exported.
func importable(t types.Type) bool {
	ok := true
	seen := map[types.Type]bool{}
	var walk func(types.Type)
	walk = func(t types.Type) {
		if t == nil || seen[t] || !ok {
			return
		}
		seen[t] = true
		switch x := t.(type) {
		case *types.Named:
			if obj := x.Obj(); obj != nil && obj.Pkg() != nil {
				if isInternal(obj.Pkg().Path()) || !obj.Exported() {
					ok = false
					return
				}
			}
			for i := 0; i < x.TypeArgs().Len(); i++ {
				walk(x.TypeArgs().At(i))
			}
		case *types.Pointer:
			walk(x.Elem())
		case *types.Slice:
			walk(x.Elem())
		case *types.Array:
			walk(x.Elem())
		case *types.Map:
			walk(x.Key())
			walk(x.Elem())
		}
	}
	walk(t)
	return ok
}

// Candidate type-argument pool and per-generic instantiation cap.
var (
	gPool = poolDiverse()
	gCap  = 10
)

// poolDiverse spans distinct gcshapes: integer widths, float, bool, string, a
// pointer (one shape for all pointers), an empty struct, and slice/map shapes.
// Each distinct shape stencils to a distinct compiled body, so a richer pool
// yields more distinct symbols.
func poolDiverse() []types.Type {
	i, s := types.Typ[types.Int], types.Typ[types.String]
	return []types.Type{
		i,
		types.Typ[types.Int64],
		types.Typ[types.Float64],
		types.Typ[types.Bool],
		s,
		types.NewPointer(i),
		types.NewStruct(nil, nil),
		types.NewSlice(i),
		types.NewSlice(s),
		types.NewMap(i, i),
	}
}

// validTuples searches the cartesian product of per-parameter candidates and
// returns up to gCap distinct tuples that types.Instantiate(validate=true)
// accepts as a whole — so interdependent params (e.g. [S ~[]E, E any]) resolve
// and nothing that fails to compile is ever emitted.
func validTuples(tps *types.TypeParamList, origin types.Type) [][]types.Type {
	n := tps.Len()
	if n == 0 || n > 3 { // cap the search; >3 type params is vanishingly rare
		return nil
	}
	perParam := make([][]types.Type, n)
	for i := 0; i < n; i++ {
		cands := append([]types.Type{}, gPool...)
		// The constraint itself is a valid type argument only when it's a pure
		// *method* interface (e.g. hash.Hash); comparable and type-set
		// interfaces (cmp.Ordered) can't be used outside constraint position —
		// and types.Instantiate(validate) wrongly accepts them, so guard here.
		if c := tps.At(i).Constraint(); c != nil {
			if iface, ok := c.Underlying().(*types.Interface); ok && iface.IsMethodSet() && iface.NumMethods() > 0 {
				cands = append(cands, c)
			}
		}
		var keep []types.Type
		for _, c := range cands {
			if importable(c) {
				keep = append(keep, c)
			}
		}
		perParam[i] = keep
	}

	ctxt := types.NewContext()
	var out [][]types.Type
	var rec func(i int, acc []types.Type)
	rec = func(i int, acc []types.Type) {
		if len(out) >= gCap {
			return
		}
		if i == n {
			if _, err := types.Instantiate(ctxt, origin, acc, true); err == nil {
				out = append(out, append([]types.Type(nil), acc...))
			}
			return
		}
		for _, c := range perParam[i] {
			if len(out) >= gCap {
				return
			}
			next := make([]types.Type, i+1)
			copy(next, acc)
			next[i] = c
			rec(i+1, next)
		}
	}
	rec(0, nil)
	return out
}

func renderList(ts []types.Type, q types.Qualifier) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = types.TypeString(t, q)
	}
	return strings.Join(parts, ", ")
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

	// Pre-assign an alias per package; the qualifier reuses it and records which
	// packages end up referenced (directly or via a rendered type).
	alias := map[string]string{}
	used := map[string]bool{}
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
	// The qualifier marks a package as needed (alias-imported) whenever a
	// rendered type names it — e.g. hash.Hash appearing in a type argument.
	qualifier := func(p *types.Package) string {
		used[p.Path()] = true
		return aliasFor(p.Path())
	}

	var fnEntries []string
	var syms bytes.Buffer
	contributed := map[string]bool{}
	add := func(path string) { contributed[path] = true }

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
				// Only funcs and methods produce text (T) symbols. Consts/vars
				// are recorded in symbols.txt but not referenced (no T symbol,
				// and large untyped constants overflow a `var _ =` sink).
				sig := o.Type().(*types.Signature)
				if sig.TypeParams().Len() == 0 {
					fnEntries = append(fnEntries, fmt.Sprintf("%s.%s", a, name))
					add(pp)
					continue
				}
				// Emit each distinct valid instantiation from the pool.
				seen := map[string]bool{}
				for _, targs := range validTuples(sig.TypeParams(), sig) {
					ref := fmt.Sprintf("%s.%s[%s]", a, name, renderList(targs, qualifier))
					if !seen[ref] {
						seen[ref] = true
						fnEntries = append(fnEntries, ref)
						add(pp)
					}
				}
			case *types.TypeName:
				named, ok := o.Type().(*types.Named)
				if !ok {
					continue
				}
				// For generic types, instantiate with each distinct valid tuple
				// and reference each instance's methods; plain types once.
				type instance struct {
					recv string
					typ  *types.Named
				}
				var instances []instance
				if tps := named.TypeParams(); tps.Len() > 0 {
					seen := map[string]bool{}
					for _, targs := range validTuples(tps, named) {
						it, err := types.Instantiate(types.NewContext(), named, targs, true)
						if err != nil {
							continue
						}
						n, ok := it.(*types.Named)
						if !ok {
							continue
						}
						recv := fmt.Sprintf("%s.%s[%s]", a, name, renderList(targs, qualifier))
						if !seen[recv] {
							seen[recv] = true
							instances = append(instances, instance{recv, n})
						}
					}
				} else {
					instances = append(instances, instance{fmt.Sprintf("%s.%s", a, name), named})
				}
				// Method expressions off the pointer method set — this recovers
				// methods on generic types too.
				for _, in := range instances {
					mset := types.NewMethodSet(types.NewPointer(in.typ))
					for i := 0; i < mset.Len(); i++ {
						fn, ok := mset.At(i).Obj().(*types.Func)
						if !ok || !fn.Exported() {
							continue
						}
						fnEntries = append(fnEntries, fmt.Sprintf("(*%s).%s", in.recv, fn.Name()))
						add(pp)
					}
				}
			}
		}
	}

	// Emit the program.
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, pp := range pkgs {
		// Alias-import packages we reference by name or via a rendered type;
		// blank-import the rest so the import isn't unused.
		if contributed[pp] || used[pp] {
			fmt.Fprintf(&b, "\t%s \"%s\"\n", alias[pp], pp)
		} else {
			fmt.Fprintf(&b, "\t_ \"%s\"\n", pp)
		}
	}
	b.WriteString(")\n\n")
	b.WriteString("var fns = []interface{}{\n")
	for _, fn := range fnEntries {
		fmt.Fprintf(&b, "\t%s,\n", fn)
	}
	b.WriteString("}\n\n//go:noinline\nfunc keep(_ interface{}) {}\nfunc init() { keep(fns) }\n\n")
	b.WriteString("func main() {}\n")

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
