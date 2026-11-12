package management

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Keep every management client behind the same transport. Scanning constructors
// catches a future handler that bypasses the guard even if its route test omits it.
func TestManagementHTTPClientsUseGuardedConstructor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if selector, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "http" {
					switch selector.Sel.Name {
					case "Client":
						if name != "upstream_guard.go" {
							t.Errorf("%s: construct clients with newManagementHTTPClient", fset.Position(n.Pos()))
						}
					case "DefaultClient", "Get", "Post", "PostForm":
						t.Errorf("%s: unguarded default HTTP client", fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
	}
}
