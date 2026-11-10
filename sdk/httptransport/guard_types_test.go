package httptransport

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Type-check the send audit as well as scanning syntax: an alias, a method
// value, an injected *http.Client, or client.Get must not bypass the rule.
// Cached compiler export data avoids a new tools dependency. No tests or
// binaries are executed by go list, and only this module's source is checked.
func TestHTTPMethodValuesCannotBypassGuard(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "-export", "-compiled", "-json", "./...")
	command.Dir = filepath.Join("..", "..")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("enumerate compiler exports: %v", err)
	}
	type listedPackage struct {
		ImportPath, Dir, Export  string
		GoFiles, CompiledGoFiles []string
	}
	var packages []listedPackage
	exports := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	const module = "github.com/router-for-me/CLIProxyAPI/v8"
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[pkg.ImportPath] = pkg.Export
		if strings.HasPrefix(pkg.ImportPath, module+"/") {
			packages = append(packages, pkg)
		}
	}
	fset := token.NewFileSet()
	lookup := func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) }
	compilerImporter := importer.ForCompiler(fset, "gc", lookup)
	clientPackages := 0
	for _, pkg := range packages {
		var files []*ast.File
		names := pkg.CompiledGoFiles
		if len(names) == 0 {
			names = pkg.GoFiles
		}
		for _, name := range names {
			if !filepath.IsAbs(name) {
				name = filepath.Join(pkg.Dir, name)
			}
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		if len(files) == 0 {
			continue
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
		configuration := types.Config{Importer: compilerImporter}
		if _, err := configuration.Check(pkg.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", pkg.ImportPath, err)
		}
		hasClient := false
		for _, file := range files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					if literal, ok := n.(*ast.CompositeLit); ok && namedHTTPType(info.TypeOf(literal), "net/http", "Client") {
						hasClient = true
					}
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					selection := info.Selections[sel]
					if selection == nil {
						if obj := info.Uses[sel.Sel]; obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "net/http" {
							switch obj.Name() {
							case "Get", "Post", "PostForm", "Head":
								t.Errorf("%s: default HTTP function bypasses shared guard", fset.Position(sel.Pos()))
							}
						}
						return true
					}
					obj := selection.Obj()
					signature, ok := obj.Type().(*types.Signature)
					if !ok {
						return true
					}
					isHTTPDo := obj.Name() == "Do" && signature.Params().Len() == 1 && namedHTTPType(signature.Params().At(0).Type(), "net/http", "Request")
					isConvenience := namedHTTPType(selection.Recv(), "net/http", "Client") && (obj.Name() == "Get" || obj.Name() == "Post" || obj.Name() == "PostForm" || obj.Name() == "Head")
					if (isHTTPDo || isConvenience) && !(pkg.ImportPath == module+"/sdk/httptransport" && fn.Name.Name == "Do") {
						t.Errorf("%s: HTTP send or method value bypasses shared guard", fset.Position(sel.Pos()))
					}
					isWebSocket := obj.Pkg() != nil && obj.Pkg().Path() == "github.com/gorilla/websocket" && (obj.Name() == "Dial" || obj.Name() == "DialContext")
					if isWebSocket && !(pkg.ImportPath == module+"/sdk/httptransport" && fn.Name.Name == "DialWebSocket") {
						t.Errorf("%s: unguarded WebSocket handshake", fset.Position(sel.Pos()))
					}
					return true
				})
			}
		}
		if hasClient {
			clientPackages++
		}
	}
	if clientPackages < 10 {
		t.Fatalf("incomplete client audit: %d packages", clientPackages)
	}
	t.Logf("type-checked %d packages including %d client-construction packages", len(packages), clientPackages)
}

func namedHTTPType(value types.Type, path, name string) bool {
	value = types.Unalias(value)
	if pointer, ok := value.(*types.Pointer); ok {
		value = pointer.Elem()
	}
	named, ok := types.Unalias(value).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == path && named.Obj().Name() == name
}
