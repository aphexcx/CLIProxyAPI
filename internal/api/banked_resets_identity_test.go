package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBankedResetFieldsCannotRepurposeCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"/v0/management/auth-files/fields", "/v8/management/credentials/fields"} {
		t.Run(route, func(t *testing.T) {
			cfg := &config.Config{AuthDir: t.TempDir()}
			cfg.RemoteManagement.SecretKey = "fixture-key"
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			h := management.NewHandler(cfg, "", manager)
			h.SetLocalPassword("fixture-key")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(true)
			s.registerManagementRoutes()
			call := func(method, path string, payload any) *httptest.ResponseRecorder {
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(method, path, bytes.NewReader(body))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Authorization", "Bearer fixture-key")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				s.engine.ServeHTTP(rec, req)
				return rec
			}
			if response := call("POST", "/v8/management/credentials?name=fixture.json", map[string]any{"type": "claude", "access_token": "fixture-only"}); response.Code != 200 {
				t.Fatalf("upload=%d", response.Code)
			}
			path := filepath.Join(cfg.AuthDir, "fixture.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			patches := []map[string]any{{"type": "kimi", "base_url": "https://api.anthropic.com/api/organizations/fixture/reset_rate_limits?x="}}
			for _, field := range []string{"type", "provider", "base_url", "base-url", "type.nested", "base_url.nested", " type ", "api_base_url"} {
				patches = append(patches, map[string]any{field: nil, "label": "must-not-commit"})
			}
			for _, patch := range patches {
				patch["name"] = "fixture.json"
				response := call("PATCH", route, patch)
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "immutable") {
					t.Fatalf("patch %v: %d %s", patch, response.Code, response.Body)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) || manager.List()[0].Provider != "claude" {
					t.Fatal("refused patch changed persisted identity")
				}
			}
		})
	}
}
