package httptransport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestBankedResetClaimHasOneCallerChain enumerates every production reference
// to the guard exception and to each link above it. The only path that can
// open the exception is the management spend route, registered once in the
// v8 management group (behind the management key):
//
//	registerManagementV8Routes -> SpendClaudeLimitReset -> ClaimLimitReset
//	  -> SendBankedResetClaim -> Do/RoundTrip -> bankedResetClaimPermitted
//
// A timer, a quota hold, a retry loop, a plugin, an executor or any other
// caller that references a link fails this test, including through a method
// value or a package-level variable.
func TestBankedResetClaimHasOneCallerChain(t *testing.T) {
	allowed := map[string]map[string]bool{
		"bankedResetClaimKey":       {"sdk/httptransport/guard_claim.go:SendBankedResetClaim": true, "sdk/httptransport/guard_claim.go:bankedResetClaimPermitted": true},
		"bankedResetClaimPermitted": {"sdk/httptransport/guard.go:Do": true, "sdk/httptransport/guard.go:RoundTrip": true},
		"SendBankedResetClaim":      {"internal/auth/claude/limit_reset_spend.go:ClaimLimitReset": true},
		"ClaimLimitReset":           {"internal/api/handlers/management/claude_limit_reset_spend.go:sendClaudeResetClaim": true},
		"sendClaudeResetClaim":      {"internal/api/handlers/management/claude_limit_reset_spend.go:SpendClaudeLimitReset": true},
		"SpendClaudeLimitReset":     {"internal/api/server_management_v8.go:registerManagementV8Routes": true},
	}
	found := map[string][]string{}
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
				ast.Inspect(decl, func(n ast.Node) bool {
					ident, ok := n.(*ast.Ident)
					if !ok || declarations[ident] {
						return true
					}
					if _, tracked := allowed[ident.Name]; tracked {
						found[ident.Name] = append(found[ident.Name], rel+":"+enclosing)
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
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if len(found[name]) == 0 {
			t.Errorf("%s: no production reference found; the audited chain is stale", name)
		}
		for _, site := range found[name] {
			if !allowed[name][site] {
				t.Errorf("%s referenced outside the spend route chain at %s", name, site)
			}
		}
	}
}
