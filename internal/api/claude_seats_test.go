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

func TestClaudeSeatsAvailabilityAndQuotaAccessControl(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, tc := range []struct {
		name                                    string
		enabled, management, home, disablePanel bool
		want                                    int
	}{
		{"enabled", true, true, false, false, 200},
		{"quota off", false, true, false, false, 404},
		{"management off", true, false, false, false, 404},
		{"home", true, true, true, false, 404},
		{"panel off", true, true, false, true, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RemoteManagement.SecretKey = "configured-test-key"
			cfg.Claude.Quota.Enabled, cfg.Home.Enabled = tc.enabled, tc.home
			cfg.RemoteManagement.DisableControlPanel = tc.disablePanel
			h := management.NewHandlerWithoutConfigFilePath(cfg, nil)
			h.SetLocalPassword("fixture-key")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(tc.management)
			s.engine.GET("/claude-seats.html", s.serveClaudeSeats)
			s.registerManagementV8Routes()
			rec := httptest.NewRecorder()
			s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/claude-seats.html", nil))
			if rec.Code != tc.want {
				t.Fatalf("page status=%d want=%d", rec.Code, tc.want)
			}
			if rec.Code == 200 && (!strings.Contains(rec.Body.String(), "Management key") || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")) {
				t.Fatal("shell lacks key prompt or frame protection")
			}
			if tc.management && !tc.home {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					req := httptest.NewRequest(method, "/v8/management/credentials/quota?auth_index=seat", strings.NewReader(`{"auth_index":"seat"}`))
					req.RemoteAddr = "127.0.0.1:1234"
					rec := httptest.NewRecorder()
					s.engine.ServeHTTP(rec, req)
					if rec.Code != 401 {
						t.Fatalf("quota without key: %d", rec.Code)
					}
				}
			}
		})
	}
}
