package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var ambiguousResetPaths = []string{
	"/x/reset_rate_limits;a=1", "/x/reset_rate_limits.json", "/x/reset_rate_limits%20",
	"/x/reset_rate_limits%00", "/x%252Freset_rate_limits", "/x%5Creset_rate_limits",
	"/x/reset_rate_limits%3Fa", "/x/re%2573et_rate_limits", "/x/reset_rate_limitſ",
	"/x/reset_rate_limits%09", "/x/reset_rate_limits%0a", "/x/reset_rate_limits%23a",
}

func TestBankedResetDenyBothManagementRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var resetCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, r.URL.Query().Get("target"), http.StatusTemporaryRedirect)
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
			paths := []string{
				"/api/org/reset_rate_limits", "/api/org/%72eset_rate_limits", "/api/org%2freset_rate_limits",
				"/api/org/reset_rate_limits/", "/api/org/ReSeT_RaTe_LiMiTs", "/api//org//reset_rate_limits",
				"/api/org/a/../reset_rate_limits?ignored=1", "/redirect307", "/redirect308", "/control",
			}
			for _, path := range ambiguousResetPaths {
				paths = append(paths, path, "/redirect?target="+url.QueryEscape(path))
			}
			for _, suffix := range paths {
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

func TestBankedResetQuotaProbeCannotSendClaim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, arm := range []string{"upload", "/v0/management/auth-files/fields", "/v8/management/credentials/fields"} {
		for _, plugin := range []string{"", "unknown-plugin"} {
			for _, redirect := range []bool{false, true} {
				t.Run(arm+"/"+plugin+"/redirect="+strconv.FormatBool(redirect), func(t *testing.T) {
					var resetCalls, hops atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/hop" {
							hops.Add(1)
							http.Redirect(w, r, "/api/org/reset_rate_limits", http.StatusTemporaryRedirect)
							return
						}
						resetCalls.Add(1)
					}))
					defer upstream.Close()
					cfg := &config.Config{AuthDir: t.TempDir()}
					cfg.RemoteManagement.SecretKey = "fixture-management-key"
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					h := management.NewHandler(cfg, "", manager)
					h.SetLocalPassword("fixture-management-key")
					s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
					s.managementRoutesEnabled.Store(true)
					s.registerManagementRoutes()
					call := func(method, route string, body any) *httptest.ResponseRecorder {
						t.Helper()
						data, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						req := httptest.NewRequest(method, route, strings.NewReader(string(data)))
						req.RemoteAddr = "127.0.0.1:1234"
						req.Header.Set("Authorization", "Bearer fixture-management-key")
						req.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						s.engine.ServeHTTP(response, req)
						return response
					}
					target := upstream.URL + "/api/org/reset_rate_limits"
					if redirect {
						target = upstream.URL + "/hop"
					}
					probe := map[string]any{"method": "POST", "url": target, "header": map[string]any{"Authorization": "Bearer $TOKEN$"}, "data": `{"program":"cedar_ember","grant_id":"fixture"}`}
					credential := map[string]any{"type": "claude", "access_token": "fixture-only", "proxy_url": "direct"}
					if arm == "upload" {
						credential["quota_probe"] = probe
					}
					response := call("POST", "/v8/management/credentials?name=fixture.json", credential)
					if response.Code != 200 {
						t.Fatalf("upload: %d %s", response.Code, response.Body.String())
					}
					if arm != "upload" {
						response = call("PATCH", arm, map[string]any{"name": "fixture.json", "quota_probe": probe})
						if response.Code != 200 {
							t.Fatalf("patch: %d %s", response.Code, response.Body.String())
						}
					}
					seats := manager.List()
					if len(seats) != 1 {
						t.Fatalf("seats: %d", len(seats))
					}
					response = call("POST", "/v0/management/quota/fetch", map[string]any{"auth_index": seats[0].Index, "plugin_id": plugin})
					if response.Code != 403 || !strings.Contains(response.Body.String(), "banked-reset deny rule (cp-bha)") {
						t.Fatalf("fetch: %d %s", response.Code, response.Body.String())
					}
					if resetCalls.Load() != 0 {
						t.Fatalf("claim reached upstream %d times", resetCalls.Load())
					}
					if !redirect && hops.Load() != 0 {
						t.Fatal("direct refusal sent upstream")
					}
				})
			}
		}
	}
}
