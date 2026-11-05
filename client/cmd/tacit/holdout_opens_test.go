package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// indexOpeners are the calls that open the search index directly. Code
// that shows facts to a session opens it only through openSessionIndex or
// openExactSessionIndex (the session's view, SPEC.md "Hold-outs").
var indexOpeners = map[string]bool{"openHookIndex": true, "openIndex": true, "openBuildIndex": true, "search.Open": true, "search.OpenRecent": true}

// directOpenAllowed is each function that may open the index directly, and
// why: it shows no fact to a session, or it is the view itself.
var directOpenAllowed = map[string]string{
	"openSessionIndex":      "the view",
	"openExactSessionIndex": "the view",
	"openHookIndex":         "the opener the view wraps",
	"openIndex":             "the opener the view wraps",
	"openBuildIndex":        "builds the index for sync and follow",
	"buildIndexForHooks":    "builds the index ahead of the hooks",
	"logNavigation":         "the delivery log's terms and names of a step: no output",
	"withProvenance":        "tacit_fact's provenance line of the fact asked for by name (the served pull)",
	"runCurate":             "the curator: no session",
	"runCurateLocal":        "the local layer's curator: no session",
	"openConflictIndex":     "the curator's conflict checks: no session",
	"followSync":            "follow builds ahead: no output",
	"runSync":               "sync builds ahead: no output",
	"(*reviewServer).rank":  "the owner's app: no agent session",
	"newRotator":            "the rotation's checks: no session",
}

// Every direct open of the index in the client's commands is in a function
// the list allows: a new path that shows facts and opens the index itself
// fails here until it reads the session's view (or is added, with why).
func TestIndexOpensGoThroughTheView(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	var bad []string
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		type body struct {
			name string
			node ast.Node
		}
		var bodies []body
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					bodies = append(bodies, body{funcName(d), d.Body})
				}
			case *ast.GenDecl:
				// A package variable holding a function (a seam tests
				// replace) is checked under the variable's name.
				for _, sp := range d.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok {
						for i, v := range vs.Values {
							if i < len(vs.Names) {
								bodies = append(bodies, body{vs.Names[i].Name, v})
							}
						}
					}
				}
			}
		}
		for _, b := range bodies {
			name := b.name
			ast.Inspect(b.node, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					callee = fn.Name
				case *ast.SelectorExpr:
					if x, ok := fn.X.(*ast.Ident); ok {
						callee = x.Name + "." + fn.Sel.Name
					}
				}
				if indexOpeners[callee] {
					seen[name] = true
					if _, ok := directOpenAllowed[name]; !ok {
						bad = append(bad, fset.Position(call.Pos()).String()+": "+name+" calls "+callee)
					}
				}
				return true
			})
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("direct index opens outside the view (use openSessionIndex or openExactSessionIndex, or allow with why):\n%s", strings.Join(bad, "\n"))
	}
	for name := range directOpenAllowed {
		if !seen[name] && name != "openHookIndex" && name != "openIndex" {
			t.Errorf("%s is allowed a direct open but makes none: drop it from the list", name)
		}
	}
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		if st, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := st.X.(*ast.Ident); ok {
				return "(*" + id.Name + ")." + fd.Name.Name
			}
		}
	}
	return fd.Name.Name
}
