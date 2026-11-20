package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The spend route is the one door past the reset guard, so it must sit behind
// the management key and exist only in the v8 management group.
func TestClaudeLimitResetSpendRouteRequiresManagementKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = "fixture-management-key"
	h := management.NewHandler(cfg, "", nil)
	h.SetLocalPassword("fixture-management-key")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	send := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"auth_index":"fixture","kind":"fable"}`))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "fixture-route-operation")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		return rec
	}
	const route = "/v8/management/claude/limit-resets/spend"
	spendRoutes := 0
	for _, info := range s.engine.Routes() {
		if strings.Contains(info.Handler, "SpendClaudeLimitReset") {
			spendRoutes++
			if info.Method != http.MethodPost || info.Path != route {
				t.Fatalf("spend handler registered as %s %s", info.Method, info.Path)
			}
		}
	}
	if spendRoutes != 1 {
		t.Fatalf("spend handler registered %d times", spendRoutes)
	}
	for _, key := range []string{"", "wrong-key"} {
		if rec := send(route, key); rec.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: status=%d body=%s", key, rec.Code, rec.Body)
		}
	}
	// With the key the handler runs; this hub has no seats, so nothing is spent.
	if rec := send(route, "fixture-management-key"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Claude seat not found") {
		t.Fatalf("authorized: status=%d body=%s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v0/management/claude/limit-resets/spend", "/v8/management/requests/claude/limit-resets/spend"} {
		if rec := send(path, "fixture-management-key"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Claude seat not found") {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body)
		}
	}
	// The exception does not reach the pass-through: the exact claim the spend
	// route builds is still refused there, on both management versions.
	claim := `{"method":"POST","url":"https://api.anthropic.com/api/organizations/00000000-0000-4000-8000-0000000000aa/reset_rate_limits","data":"{\"program\":\"cedar_ember\",\"grant_id\":\"fixture-grant\",\"request_id\":\"fixture-request\"}"}`
	for _, route := range []string{"/v0/management/api-call", "/v8/management/requests/api-call"} {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(claim))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer fixture-management-key")
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "banked-reset deny rule (cp-bha)") {
			t.Fatalf("%s: status=%d body=%s", route, rec.Code, rec.Body)
		}
	}
}
