package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBankedResetDenyBothManagementRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var resetCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect307":
			http.Redirect(w, r, "/api/org/RESET_rate_limits/", http.StatusTemporaryRedirect)
		case "/redirect308":
			http.Redirect(w, r, "/api/org/%72eset_rate_limits", http.StatusPermanentRedirect)
		default:
			resetCalls.Add(1)
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = "fixture-management-key"
	h := management.NewHandler(cfg, "", nil)
	h.SetLocalPassword("fixture-management-key")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	for _, route := range []string{"/v0/management/api-call", "/v8/management/requests/api-call"} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CUSTOM"} {
			for _, suffix := range []string{
				"/api/org/reset_rate_limits", "/api/org/%72eset_rate_limits", "/api/org%2freset_rate_limits",
				"/api/org/reset_rate_limits/", "/api/org/ReSeT_RaTe_LiMiTs", "/api//org//reset_rate_limits",
				"/api/org/a/../reset_rate_limits?ignored=1", "/redirect307", "/redirect308", "/control",
			} {
				t.Run(route+"/"+method+suffix, func(t *testing.T) {
					body, err := json.Marshal(map[string]string{"method": method, "url": upstream.URL + suffix, "data": "{}", "proxy_url": "direct"})
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(body)))
					req.RemoteAddr = "127.0.0.1:1234"
					req.Header.Set("Authorization", "Bearer fixture-management-key")
					response := httptest.NewRecorder()
					s.engine.ServeHTTP(response, req)
					want := http.StatusForbidden
					if suffix == "/control" {
						want = http.StatusOK
					}
					if response.Code != want {
						t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
					}
					if want == http.StatusForbidden && !strings.Contains(response.Body.String(), "banked-reset deny rule (cp-bha)") {
						t.Fatalf("missing rule attribution: %s", response.Body.String())
					}
				})
			}
		}
	}
	if resetCalls.Load() != 0 {
		t.Fatalf("reset upstream reached %d times", resetCalls.Load())
	}
}
