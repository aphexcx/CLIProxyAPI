package httptransport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestBankedResetClaimHasOneCallerChain enumerates every production reference
// to the reset guard's one exception (internal/resetclaim) and to each link
// above it. The only path that can open the exception is the management spend
// route, registered once in the v8 management group (behind the management key):
//
//	registerManagementV8Routes -> SpendClaudeLimitReset -> sendClaudeResetClaim
//	  -> ClaimLimitReset -> NewMarkedClaimRequest -> Do (ClaimPermitted, the
//	  preflight) -> RoundTrip (PermittedClaim, the one send)
//
// Each link may be referenced exactly once, at its one allowed site, and never
// from inside a function literal, a go or defer statement, or a loop, so a
// timer, a goroutine, a retry loop or a second registration fails this test.
// A tracked name in a string literal (reflection) fails it too.
func TestBankedResetClaimHasOneCallerChain(t *testing.T) {
	allowed := map[string]map[string]int{
		"claimMarkerKey":        {"internal/resetclaim/claim.go:NewMarkedClaimRequest": 1, "internal/resetclaim/claim.go:markerOf": 1},
		"ClaimPermitted":        {"sdk/httptransport/guard.go:Do": 1},
		"PermittedClaim":        {"sdk/httptransport/guard.go:RoundTrip": 1},
		"NewMarkedClaimRequest": {"internal/auth/claude/limit_reset_spend.go:ClaimLimitReset": 1},
		"NoRedirectClaimClient": {"internal/auth/claude/limit_reset_spend.go:ClaimLimitReset": 1},
		"ClaimLimitReset":       {"internal/api/handlers/management/claude_limit_reset_spend.go:sendClaudeResetClaim": 1},
		"sendClaudeResetClaim":  {"internal/api/handlers/management/claude_limit_reset_spend.go:SpendClaudeLimitReset": 1},
		"SpendClaudeLimitReset": {"internal/api/server_management_v8.go:registerManagementV8Routes": 1},
	}
	found := map[string]map[string]int{}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	for _, tree := range []string{"cmd", "internal", "sdk"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			declarations := map[*ast.Ident]bool{}
			for _, decl := range file.Decls {
				switch typed := decl.(type) {
				case *ast.FuncDecl:
					declarations[typed.Name] = true
				case *ast.GenDecl:
					for _, spec := range typed.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							declarations[ts.Name] = true
						}
					}
				}
			}
			for _, decl := range file.Decls {
				enclosing := ""
				if fn, ok := decl.(*ast.FuncDecl); ok {
					enclosing = fn.Name.Name
				}
				var stack []ast.Node
				ast.Inspect(decl, func(n ast.Node) bool {
					if n == nil {
						stack = stack[:len(stack)-1]
						return true
					}
					stack = append(stack, n)
					if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						value, _ := strconv.Unquote(literal.Value)
						for name := range allowed {
							if strings.Contains(value, name) {
								t.Errorf("%s: %s named in a string literal", fset.Position(n.Pos()), name)
							}
						}
					}
					ident, ok := n.(*ast.Ident)
					if !ok || declarations[ident] {
						return true
					}
					if _, tracked := allowed[ident.Name]; !tracked {
						return true
					}
					site := rel + ":" + enclosing
					for _, ancestor := range stack[:len(stack)-1] {
						switch ancestor.(type) {
						case *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt, *ast.ForStmt, *ast.RangeStmt:
							t.Errorf("%s: %s referenced inside a closure, go, defer or loop", fset.Position(ident.Pos()), ident.Name)
						}
					}
					if found[ident.Name] == nil {
						found[ident.Name] = map[string]int{}
					}
					found[ident.Name][site]++
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for site, want := range allowed[name] {
			if got := found[name][site]; got != want {
				t.Errorf("%s: %d references at %s, want %d", name, got, site, want)
			}
		}
		for site, got := range found[name] {
			if _, ok := allowed[name][site]; !ok {
				t.Errorf("%s referenced outside the spend route chain at %s (%d times)", name, site, got)
			}
		}
	}
}
