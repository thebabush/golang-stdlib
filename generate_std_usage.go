package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func run(cmd string, args ...string) (string, error) {
	c := exec.Command(cmd, args...)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, out.String())
	}
	return out.String(), nil
}

type pkgInfo struct {
	Dir     string
	GoFiles []string
}

func listPackages() ([]string, error) {
	out, err := run("go", "list", "std")
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(out)
	var pkgs []string
	for _, p := range lines {
		if strings.Contains(p, "/internal/") || strings.HasPrefix(p, "internal/") || strings.HasSuffix(p, "/internal") {
			continue
		}
		if strings.Contains(p, "/vendor/") || strings.HasPrefix(p, "vendor/") {
			continue
		}
		if strings.HasPrefix(p, "cmd/") || p == "cmd" {
			continue
		}
		if p == "runtime/cgo" {
			continue
		}
		if p == "runtime/race" {
			continue
		}
		if p == "plugin" && os.Getenv("GOOS") != "linux" {
			continue
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

func packageInfo(pkg string) (pkgInfo, error) {
	out, err := run("go", "list", "-json", pkg)
	if err != nil {
		return pkgInfo{}, err
	}
	var info pkgInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return pkgInfo{}, err
	}
	for i, f := range info.GoFiles {
		info.GoFiles[i] = filepath.Join(info.Dir, f)
	}
	return info, nil
}

func sanitizeAlias(pkg string) string {
	alias := pkg
	alias = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, alias)
	return alias
}

type export struct {
	kind     string
	name     string
	generic  bool
	genKinds []string
	genOK    bool
}

func isMapConstraint(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.MapType:
		return true
	case *ast.UnaryExpr:
		return isMapConstraint(t.X)
	}
	return false
}

func isSliceConstraint(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.ArrayType:
		return t.Len == nil
	case *ast.UnaryExpr:
		return isSliceConstraint(t.X)
	}
	return false
}

func simpleConstraint(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		// A bare builtin identifier (Obj == nil) like `any` or `comparable` is
		// satisfied by our canned `int`/`string` instantiation. `error` is the
		// exception: it's also a bare builtin but an interface, so int/string
		// don't satisfy it — skip those generics rather than emit broken code.
		return t.Obj == nil && t.Name != "error"
	case *ast.UnaryExpr:
		if t.Op == token.TILDE {
			if id, ok := t.X.(*ast.Ident); ok {
				return id.Obj == nil
			}
		}
	case *ast.BinaryExpr:
		if t.Op == token.OR {
			return simpleConstraint(t.X) && simpleConstraint(t.Y)
		}
	case *ast.ParenExpr:
		return simpleConstraint(t.X)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			if id.Name == "constraints" || id.Name == "cmp" {
				return true
			}
		}
	}
	return false
}

func supportedConstraint(e ast.Expr) bool {
	if isMapConstraint(e) || isSliceConstraint(e) {
		return true
	}
	return simpleConstraint(e)
}

func exprString(e ast.Expr) string {
	var buf bytes.Buffer
	format.Node(&buf, token.NewFileSet(), e)
	return buf.String()
}

func recvString(e ast.Expr, alias string) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvString(t.X, alias)
	case *ast.Ident:
		return alias + "." + t.Name
	case *ast.IndexExpr:
		return recvString(t.X, alias) + "[" + exprString(t.Index) + "]"
	case *ast.IndexListExpr:
		parts := make([]string, len(t.Indices))
		for i, idx := range t.Indices {
			parts[i] = exprString(idx)
		}
		return recvString(t.X, alias) + "[" + strings.Join(parts, ", ") + "]"
	default:
		return exprString(e)
	}
}

func parseMethods(info pkgInfo, alias string) ([]string, error) {
	var methods []string
	fs := token.NewFileSet()
	for _, file := range info.GoFiles {
		f, err := parser.ParseFile(fs, file, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || d.Recv == nil {
				continue
			}
			if !d.Name.IsExported() {
				continue
			}
			recv := d.Recv.List[0].Type
			// skip generic methods or methods on generic receivers
			if d.Type.TypeParams != nil {
				continue
			}
			switch rt := recv.(type) {
			case *ast.IndexExpr, *ast.IndexListExpr:
				continue
			case *ast.StarExpr:
				switch rt.X.(type) {
				case *ast.IndexExpr, *ast.IndexListExpr:
					continue
				}
			}
			var id *ast.Ident
			switch rt := recv.(type) {
			case *ast.Ident:
				id = rt
			case *ast.StarExpr:
				if ident, ok := rt.X.(*ast.Ident); ok {
					id = ident
				}
			case *ast.IndexExpr:
				if ident, ok := rt.X.(*ast.Ident); ok {
					id = ident
				}
			case *ast.IndexListExpr:
				if ident, ok := rt.X.(*ast.Ident); ok {
					id = ident
				}
			}
			if id == nil || !id.IsExported() {
				continue
			}
			methods = append(methods, fmt.Sprintf("(%s).%s", recvString(recv, alias), d.Name.Name))
		}
	}
	return methods, nil
}

func parseExports(info pkgInfo) ([]export, export, error) {
	var exports []export
	var first export
	fs := token.NewFileSet()
	for _, file := range info.GoFiles {
		f, err := parser.ParseFile(fs, file, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok == token.VAR || d.Tok == token.CONST {
					kind := strings.ToLower(d.Tok.String())
					for _, sp := range d.Specs {
						vs := sp.(*ast.ValueSpec)
						for _, name := range vs.Names {
							if name.IsExported() {
								e := export{kind: kind, name: name.Name}
								exports = append(exports, e)
								if first.name == "" {
									first = e
								}
							}
						}
					}
				} else if d.Tok == token.TYPE {
					kind := "type"
					for _, sp := range d.Specs {
						ts := sp.(*ast.TypeSpec)
						if ts.Name.IsExported() {
							e := export{kind: kind, name: ts.Name.Name, genOK: true}
							if ts.TypeParams != nil && len(ts.TypeParams.List) > 0 {
								e.generic = true
								for _, tp := range ts.TypeParams.List {
									kind := "simple"
									if isMapConstraint(tp.Type) {
										kind = "map"
									} else if isSliceConstraint(tp.Type) {
										kind = "slice"
									}
									if !supportedConstraint(tp.Type) {
										e.genOK = false
									}
									for range tp.Names {
										e.genKinds = append(e.genKinds, kind)
									}
								}
							}
							exports = append(exports, e)
							if first.name == "" {
								first = e
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil {
					// Skip methods; only consider top-level functions.
					continue
				}
				if !d.Name.IsExported() {
					continue
				}
				// d.Recv is nil so no receiver checks needed
				e := export{kind: "func", name: d.Name.Name, genOK: true}
				if d.Type.TypeParams != nil && len(d.Type.TypeParams.List) > 0 {
					e.generic = true
					for _, tp := range d.Type.TypeParams.List {
						kind := "simple"
						if isMapConstraint(tp.Type) {
							kind = "map"
						} else if isSliceConstraint(tp.Type) {
							kind = "slice"
						}
						if !supportedConstraint(tp.Type) {
							e.genOK = false
						}
						for range tp.Names {
							e.genKinds = append(e.genKinds, kind)
						}
					}
				}
				exports = append(exports, e)
				if first.name == "" {
					first = e
				}
			}
		}
	}
	return exports, first, nil
}

func assignment(alias string, e export) (string, []string) {
	// Filter out syscall functions on Windows only
	if os.Getenv("GOOS") == "windows" && strings.HasSuffix(alias, "syscall") && e.kind == "func" {
		// Filter SyscallN
		if e.name == "SyscallN" {
			return fmt.Sprintf("// %s.%s filtered out", alias, e.name), nil
		}
		// Filter SyscallXXX where XXX >= 16
		if strings.HasPrefix(e.name, "Syscall") && len(e.name) > 7 {
			numPart := e.name[7:] // Get the number part after "Syscall"
			if numPart != "" {
				if num, err := strconv.Atoi(numPart); err == nil && num >= 16 {
					return fmt.Sprintf("// %s.%s filtered out", alias, e.name), nil
				}
			}
		}
	}

	switch e.kind {
	case "var", "const":
		return fmt.Sprintf("var _ = %s.%s", alias, e.name), nil
	case "type":
		if e.generic {
			if !e.genOK {
				return fmt.Sprintf("// %s.%s requires type parameters, skipping", alias, e.name), nil
			}
			types := make([]string, len(e.genKinds))
			for i, k := range e.genKinds {
				switch k {
				case "map":
					types[i] = "map[int]int"
				case "slice":
					types[i] = "[]int"
				default:
					types[i] = "int"
				}
			}
			return fmt.Sprintf("type _ = %s.%s[%s]", alias, e.name, strings.Join(types, ", ")), nil
		}
		return fmt.Sprintf("type _ = %s.%s", alias, e.name), nil
	case "func":
		if e.generic {
			if !e.genOK {
				return fmt.Sprintf("// %s.%s requires type parameters, skipping", alias, e.name), nil
			}
			types1 := make([]string, len(e.genKinds))
			types2 := make([]string, len(e.genKinds))
			for i, k := range e.genKinds {
				switch k {
				case "map":
					types1[i] = "map[int]int"
					types2[i] = "map[string]string"
				case "slice":
					types1[i] = "[]int"
					types2[i] = "[]string"
				default:
					types1[i] = "int"
					types2[i] = "string"
				}
			}
			return "", []string{
				fmt.Sprintf("%s.%s[%s]", alias, e.name, strings.Join(types1, ", ")),
				fmt.Sprintf("%s.%s[%s]", alias, e.name, strings.Join(types2, ", ")),
			}
		}
		return "", []string{fmt.Sprintf("%s.%s", alias, e.name)}
	}
	return "", nil
}

func main() {
	flag.Parse()
	outDir := "output/std_usage"
	outBinName := "main"
	if flag.NArg() > 0 {
		outDir = flag.Arg(0)
	}
	if flag.NArg() > 1 {
		outBinName = flag.Arg(1)
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Fatal(err)
	}

	symsPath := filepath.Join(outDir, "symbols.txt")
	mainPath := filepath.Join(outDir, "main.go")

	symsFile, err := os.Create(symsPath)
	if err != nil {
		log.Fatal(err)
	}
	defer symsFile.Close()
	mainFile, err := os.Create(mainPath)
	if err != nil {
		log.Fatal(err)
	}
	defer mainFile.Close()

	pkgs, err := listPackages()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Fprintln(mainFile, "package main")

	fmt.Fprintln(mainFile, "import (")
	var assigns []string
	var fnEntries []string
	aliasCount := make(map[string]int)
	for _, pkg := range pkgs {
		log.Printf("processing %s", pkg)
		baseAlias := sanitizeAlias(pkg)
		cnt := aliasCount[baseAlias]
		aliasCount[baseAlias] = cnt + 1
		alias := baseAlias
		if cnt > 0 {
			alias = fmt.Sprintf("%s_%d", baseAlias, cnt)
		}
		aliasLine := fmt.Sprintf("    %s \"%s\"", alias, pkg)
		if pkg == "unsafe" {
			aliasLine = fmt.Sprintf("    _ \"%s\"", pkg)
			assigns = append(assigns, "// skipping unsafe package")
			fmt.Fprintln(mainFile, aliasLine)
			continue
		}
		info, err := packageInfo(pkg)
		if err != nil {
			aliasLine = fmt.Sprintf("    _ \"%s\"", pkg)
			assigns = append(assigns, fmt.Sprintf("// failed to load %s: %v", pkg, err))
			fmt.Fprintln(mainFile, aliasLine)
			continue
		}
		exports, first, err := parseExports(info)
		if err != nil {
			aliasLine = fmt.Sprintf("    _ \"%s\"", pkg)
			assigns = append(assigns, fmt.Sprintf("// failed to parse %s: %v", pkg, err))
			fmt.Fprintln(mainFile, aliasLine)
			continue
		}
		for _, e := range exports {
			fmt.Fprintf(symsFile, "%s: %s %s\n", pkg, e.kind, e.name)
		}
		methods, err := parseMethods(info, alias)
		if err == nil {
			fnEntries = append(fnEntries, methods...)
		}
		for i, e := range exports {
			if i == 0 || e.kind != "func" {
				continue
			}
			assignF, funcs := assignment(alias, e)
			if assignF != "" {
				assigns = append(assigns, assignF)
			}
			fnEntries = append(fnEntries, funcs...)
		}
		if first.name == "" {
			aliasLine = fmt.Sprintf("    _ \"%s\"", pkg)
			assigns = append(assigns, fmt.Sprintf("// %s has no exported symbols", pkg))
		} else {
			assign, funcs := assignment(alias, first)
			if assign != "" {
				if strings.HasPrefix(assign, "//") {
					aliasLine = fmt.Sprintf("    _ \"%s\"", pkg)
				}
				assigns = append(assigns, assign)
			}
			fnEntries = append(fnEntries, funcs...)
		}
		fmt.Fprintln(mainFile, aliasLine)
	}
	fmt.Fprintln(mainFile, ")")
	// store references to instantiated functions so the compiler keeps them
	fmt.Fprintln(mainFile, "var fns = []interface{}{")
	for _, fn := range fnEntries {
		fmt.Fprintf(mainFile, "    %s,\n", fn)
	}
	fmt.Fprintln(mainFile, "}")
	fmt.Fprintln(mainFile, "//go:noinline")
	// keep prevents the compiler from discarding fns
	fmt.Fprintln(mainFile, "func keep(_ interface{}) {}")
	fmt.Fprintln(mainFile, "func init() { keep(fns) }")
	for _, a := range assigns {
		fmt.Fprintln(mainFile, a)
	}
	fmt.Fprintln(mainFile, "func main(){}")

	fmt.Printf("Generated symbols list at %s\n", symsPath)
	fmt.Printf("Generated main program at %s\n", mainPath)

	outBin := filepath.Join(outDir, outBinName)
	if os.Getenv("GOOS") == "windows" {
		outBin += ".exe"
	}
	cmd := exec.Command("go", "build", "-gcflags=-l", "-o", outBin, mainPath)

	// Pure-Go build: cgo adds no stdlib symbols (measured), only C glue, so we
	// keep it off everywhere. This also means no C toolchain is required and
	// every GOOS/GOARCH builds identically. GOOS/GOARCH come from the ambient
	// environment (set by generate_all.sh per target).
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("failed to build: %v", err)
	}
	fmt.Printf("Compiled binary at %s\n", outBin)
}
