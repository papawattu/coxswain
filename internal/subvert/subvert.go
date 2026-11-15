// Package subvert scans verified deliverable source for subversion patterns
// the verified file adds over its base (ADR-0005 D17.2).
package subvert

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// Hit is one subversion pattern the verified file adds over its base (ADR-0005 D17.2).
type Hit struct {
	Path    string
	Pattern string
	Line    int
}

// Pattern names (the Hit.Pattern values):
const (
	PatternTestingTesting = "testing.Testing()"
	PatternExitInInit     = "os.Exit in init"
	PatternLinkname       = "//go:linkname"
	PatternFlagLookupTest = `flag.Lookup("test.`
	PatternCanaryName     = "zzz_canary"
)

var patternOrder = []string{PatternTestingTesting, PatternExitInInit, PatternFlagLookupTest, PatternLinkname, PatternCanaryName}

// ScanGo reports the patterns the verified Go source has MORE of than the base
// (base nil = a new file). It parses with go/parser (ParseComments); a file
// that does not parse returns an error, never "clean". A base that does not
// parse counts as zero hits (report, never hide).
func ScanGo(path string, base, verified []byte) ([]Hit, error) {
	fset := token.NewFileSet()
	verifiedFile, err := parser.ParseFile(fset, path, verified, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	vc := count(fset, verifiedFile)

	var bc map[string][]int
	if base != nil {
		if baseFile, berr := parser.ParseFile(fset, path, base, parser.ParseComments); berr == nil {
			bc = count(fset, baseFile)
		}
		// A base that does not parse counts as zero hits.
	}

	var hits []Hit
	for _, name := range patternOrder {
		vl := vc[name]
		if len(vl) <= len(bc[name]) {
			continue
		}
		hits = append(hits, Hit{Path: path, Pattern: name, Line: vl[0]})
	}
	return hits, nil
}

// importNames resolves, from file.Imports, the local names of the standard
// library packages "testing", "os" and "flag": an explicit alias wins, a
// dot-import records the bare-ident form, otherwise the last path element.
func importNames(file *ast.File) (names map[string]string, dot map[string]bool) {
	names = map[string]string{}
	dot = map[string]bool{}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		last := p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			last = p[i+1:]
		}
		if last != "testing" && last != "os" && last != "flag" {
			continue
		}
		switch {
		case imp.Name == nil:
			names[last] = last
		case imp.Name.Name == ".":
			dot[last] = true
			delete(names, last)
		default:
			names[last] = imp.Name.Name
		}
	}
	return names, dot
}

// isCall reports whether call is a call to pkg's local name qualified with sel
// (name.sel) or, when pkg is dot-imported, the bare ident sel.
func isCall(fun ast.Expr, names map[string]string, dot map[string]bool, pkg, sel string) bool {
	if s, ok := fun.(*ast.SelectorExpr); ok {
		x, ok := s.X.(*ast.Ident)
		return ok && x.Name == names[pkg] && s.Sel.Name == sel
	}
	if dot[pkg] {
		if id, ok := fun.(*ast.Ident); ok {
			return id.Name == sel
		}
	}
	return false
}

// inInit reports whether pos lies inside the body of a top-level func decl
// named init with no receiver. Func literals inside init are covered by the
// body span.
func inInit(file *ast.File, pos token.Pos) bool {
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "init" || fd.Recv != nil || fd.Body == nil {
			continue
		}
		if pos >= fd.Body.Lbrace && pos <= fd.Body.Rbrace {
			return true
		}
	}
	return false
}

// count maps each pattern name to the 1-based lines of its occurrences in
// file.
func count(fset *token.FileSet, file *ast.File) map[string][]int {
	names, dot := importNames(file)
	out := map[string][]int{}
	add := func(name string, pos token.Pos) {
		out[name] = append(out[name], fset.Position(pos).Line)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if isCall(n.Fun, names, dot, "testing", "Testing") {
				add(PatternTestingTesting, n.Fun.Pos())
			}
			if isCall(n.Fun, names, dot, "os", "Exit") && inInit(file, n.Fun.Pos()) {
				add(PatternExitInInit, n.Fun.Pos())
			}
			if isCall(n.Fun, names, dot, "flag", "Lookup") && len(n.Args) > 0 {
				if lit, ok := n.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if val, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(val, "test.") {
						add(PatternFlagLookupTest, n.Fun.Pos())
					}
				}
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if val, err := strconv.Unquote(n.Value); err == nil && strings.Contains(val, "zzz_canary") {
					add(PatternCanaryName, n.Pos())
				}
			}
		}
		return true
	})

	for _, g := range file.Comments {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "//go:linkname") {
				add(PatternLinkname, c.Pos())
			}
			if strings.Contains(c.Text, "zzz_canary") {
				add(PatternCanaryName, c.Pos())
			}
		}
	}

	return out
}
