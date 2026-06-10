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

type pkgInfo struct {
	ImportPath string
	Dir        string
	GoFiles    []string
}

// listStdPackages returns every standard-library package (with its source
// files resolved to absolute paths) in a single `go list` invocation.
// Previously this shelled out once per package — ~360 subprocesses — which
// dominated the generator's runtime; the streamed form is ~40x faster.
func listStdPackages() ([]pkgInfo, error) {
	c := exec.Command("go", "list", "-json", "std")
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, errb.String())
	}
	var pkgs []pkgInfo
	dec := json.NewDecoder(&out)
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			return nil, err
		}
		if skipPackage(p.ImportPath) {
			continue
		}
		for i, f := range p.GoFiles {
			p.GoFiles[i] = filepath.Join(p.Dir, f)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

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
	case p == "plugin" && os.Getenv("GOOS") != "linux":
		return true
	}
	return false
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
	kind    string
	name    string
	generic bool
	genOK   bool
	// type arguments to instantiate a generic with, one per type parameter.
	// Two parallel flavours give two instantiations (more symbol coverage)
	// where the constraint admits both; they're equal otherwise.
	typeArgs1 []string
	typeArgs2 []string
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

// isTypeSetElement reports whether an embedded interface element is a type-set
// term (a union, or a ~T approximation) rather than an embedded method
// interface.
func isTypeSetElement(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.BinaryExpr:
		return t.Op == token.OR
	case *ast.UnaryExpr:
		return t.Op == token.TILDE
	case *ast.ParenExpr:
		return isTypeSetElement(t.X)
	}
	return false
}

// firstTerm returns a concrete type name drawn from a type-set element,
// stripping a leading ~ and descending unions left-most-first. The result is
// guaranteed to be a member of the set, so it's a safe type argument.
func firstTerm(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.BinaryExpr:
		if t.Op == token.OR {
			return firstTerm(t.X)
		}
	case *ast.UnaryExpr:
		if t.Op == token.TILDE {
			return exprString(t.X)
		}
	case *ast.ParenExpr:
		return firstTerm(t.X)
	}
	return exprString(e)
}

// interfaceTypeArg picks a type argument for an interface constraint. If the
// interface carries a type set (unions / ~T), it returns a member of that set.
// Otherwise it's a pure method set, and the interface itself (named by self) is
// a valid argument. ok is false when neither applies (self == "").
func interfaceTypeArg(it *ast.InterfaceType, self string) (string, bool) {
	for _, f := range it.Methods.List {
		if len(f.Names) > 0 {
			continue // a method
		}
		if isTypeSetElement(f.Type) {
			return firstTerm(f.Type), true
		}
	}
	if self != "" {
		return self, true
	}
	return "", false
}

// typeArg resolves the type argument(s) for one type parameter's constraint,
// in two parallel flavours. typeDecls resolves in-package constraint names;
// alias qualifies an in-package interface when it's used as its own argument.
// ok is false when no nameable satisfying type is found (the generic is skipped).
func typeArg(c ast.Expr, typeDecls map[string]*ast.InterfaceType, alias string) (a1, a2 string, ok bool) {
	if isMapConstraint(c) {
		return "map[int]int", "map[string]string", true
	}
	if isSliceConstraint(c) {
		return "[]int", "[]string", true
	}
	switch t := c.(type) {
	case *ast.Ident:
		switch t.Name {
		case "any", "comparable":
			return "int", "string", true
		case "error":
			// error is an interface: it satisfies its own method-set constraint.
			return "error", "error", true
		}
		// In-package named constraint (Obj != nil), e.g. cmp.Ordered, rand.intType.
		if it, found := typeDecls[t.Name]; found {
			if arg, good := interfaceTypeArg(it, alias+"."+t.Name); good {
				return arg, arg, true
			}
		}
		return "", "", false
	case *ast.UnaryExpr:
		if t.Op == token.TILDE {
			return "int", "string", true
		}
	case *ast.BinaryExpr:
		if t.Op == token.OR {
			return "int", "string", true
		}
	case *ast.ParenExpr:
		return typeArg(t.X, typeDecls, alias)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok && (id.Name == "constraints" || id.Name == "cmp") {
			return "int", "string", true
		}
		// An interface from another package used as a constraint (e.g.
		// hash.Hash): an interface satisfies its own method-set constraint.
		s := exprString(c)
		return s, s, true
	case *ast.InterfaceType:
		if arg, good := interfaceTypeArg(t, ""); good {
			return arg, arg, true
		}
	}
	return "", "", false
}

// resolveTypeArgs computes type arguments for every type parameter of a generic
// declaration. ok is false if any constraint can't be satisfied.
func resolveTypeArgs(tparams *ast.FieldList, typeDecls map[string]*ast.InterfaceType, alias string) (args1, args2 []string, ok bool) {
	for _, tp := range tparams.List {
		a1, a2, good := typeArg(tp.Type, typeDecls, alias)
		if !good {
			return nil, nil, false
		}
		n := len(tp.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			args1 = append(args1, a1)
			args2 = append(args2, a2)
		}
	}
	return args1, args2, true
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

// collectInterfaceDecls maps every in-package type name to its interface
// definition (exported or not), so constraint identifiers like cmp.Ordered or
// rand.intType can be resolved when choosing a generic's type arguments.
func collectInterfaceDecls(files []*ast.File) map[string]*ast.InterfaceType {
	decls := make(map[string]*ast.InterfaceType)
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts := sp.(*ast.TypeSpec)
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					decls[ts.Name.Name] = it
				}
			}
		}
	}
	return decls
}

// genericArgs fills in the type arguments (or marks genOK=false) for a generic
// declaration's type parameters.
func genericArgs(e *export, tparams *ast.FieldList, typeDecls map[string]*ast.InterfaceType, alias string) {
	e.generic = true
	args1, args2, ok := resolveTypeArgs(tparams, typeDecls, alias)
	if !ok {
		e.genOK = false
		return
	}
	e.typeArgs1 = args1
	e.typeArgs2 = args2
}

func parseExports(info pkgInfo, alias string) ([]export, export, error) {
	var exports []export
	var first export
	fs := token.NewFileSet()
	var files []*ast.File
	for _, file := range info.GoFiles {
		f, err := parser.ParseFile(fs, file, nil, 0)
		if err != nil {
			continue
		}
		files = append(files, f)
	}
	typeDecls := collectInterfaceDecls(files)
	for _, f := range files {
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
								genericArgs(&e, ts.TypeParams, typeDecls, alias)
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
				e := export{kind: "func", name: d.Name.Name, genOK: true}
				if d.Type.TypeParams != nil && len(d.Type.TypeParams.List) > 0 {
					genericArgs(&e, d.Type.TypeParams, typeDecls, alias)
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
			return fmt.Sprintf("type _ = %s.%s[%s]", alias, e.name, strings.Join(e.typeArgs1, ", ")), nil
		}
		return fmt.Sprintf("type _ = %s.%s", alias, e.name), nil
	case "func":
		if e.generic {
			if !e.genOK {
				return fmt.Sprintf("// %s.%s requires type parameters, skipping", alias, e.name), nil
			}
			insts := []string{fmt.Sprintf("%s.%s[%s]", alias, e.name, strings.Join(e.typeArgs1, ", "))}
			// Emit a second instantiation only when it differs (more coverage).
			if strings.Join(e.typeArgs2, ",") != strings.Join(e.typeArgs1, ",") {
				insts = append(insts, fmt.Sprintf("%s.%s[%s]", alias, e.name, strings.Join(e.typeArgs2, ", ")))
			}
			return "", insts
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

	pkgs, err := listStdPackages()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Fprintln(mainFile, "package main")

	fmt.Fprintln(mainFile, "import (")
	var assigns []string
	var fnEntries []string
	aliasCount := make(map[string]int)
	for _, info := range pkgs {
		pkg := info.ImportPath
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
		exports, first, err := parseExports(info, alias)
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
