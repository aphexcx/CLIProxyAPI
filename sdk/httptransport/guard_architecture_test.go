package httptransport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Inspect every production package, including client constructors, rather than
// just management handlers. Guarding at send time also covers injected clients
// and Transport assignments after construction. Import aliases cannot bypass
// the default-client/convenience-function check.
func TestAllHTTPPackagesUseGuardedSend(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	clientPackages := map[string]bool{}
	for _, tree := range []string{"cmd", "internal", "sdk"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			httpNames := map[string]bool{}
			for _, imp := range file.Imports {
				value, _ := strconv.Unquote(imp.Path.Value)
				if value == "net/http" {
					name := "http"
					if imp.Name != nil {
						name = imp.Name.Name
					}
					if name == "." {
						t.Errorf("%s: dot-imported net/http bypasses send audit", rel)
					}
					httpNames[name] = true
				}
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					if literal, ok := n.(*ast.CompositeLit); ok {
						if sel, ok := literal.Type.(*ast.SelectorExpr); ok {
							if pkg, ok := sel.X.(*ast.Ident); ok && httpNames[pkg.Name] && sel.Sel.Name == "Client" {
								clientPackages[filepath.Dir(rel)] = true
							}
						}
					}
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if pkg, ok := sel.X.(*ast.Ident); ok && httpNames[pkg.Name] {
						switch sel.Sel.Name {
						case "Get", "Post", "PostForm", "Head":
							t.Errorf("%s: default HTTP send bypasses guard", fset.Position(call.Pos()))
						}
					}
					if sel.Sel.Name == "Do" && len(call.Args) == 1 {
						// sync.Once.Do accepts a function, not an HTTP request.
						if _, ok := call.Args[0].(*ast.FuncLit); ok {
							return true
						}
						if id, ok := call.Args[0].(*ast.Ident); ok && strings.HasPrefix(id.Name, "start") && strings.HasSuffix(id.Name, "Cleanup") {
							return true
						}
						if arg, ok := call.Args[0].(*ast.SelectorExpr); ok {
							switch arg.Sel.Name {
							case "cancel", "fn", "detect", "mark":
								return true
							}
						}
						if rel != "sdk/httptransport/guard.go" {
							t.Errorf("%s: use httptransport.Do at the final send", fset.Position(call.Pos()))
						}
					}
					if sel.Sel.Name == "RoundTrip" {
						// These are the transport implementations below the guarded
						// client, not alternate public request senders.
						allowed := fn.Name.Name == "RoundTrip" && fn.Recv != nil
						allowed = allowed || (rel == "internal/runtime/executor/helps/utls_client.go" && fn.Name.Name == "roundTripUtlsConnection")
						if !allowed {
							t.Errorf("%s: direct transport send bypasses guard", fset.Position(call.Pos()))
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, pkg := range []string{"internal/runtime/executor", "internal/runtime/executor/helps", "internal/pluginhost", "internal/auth/claude", "internal/api/handlers/management"} {
		if !clientPackages[pkg] {
			t.Errorf("client package missing from audit: %s", pkg)
		}
	}
	t.Logf("audited %d client-construction packages and every production HTTP send", len(clientPackages))
}
