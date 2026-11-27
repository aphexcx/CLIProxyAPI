package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Test 6: the cache meter route answers the documented JSON with the
// management key and refuses without it.
func TestClaudeCacheMeterRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		yaml        string
		ruleEnabled bool
	}{
		{name: "rule on", yaml: "{}\n", ruleEnabled: true},
		{name: "rule off", yaml: "claude-header-defaults: {one-hour-cache: false}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, errParse := config.ParseConfigBytes([]byte(tc.yaml))
			if errParse != nil {
				t.Fatal(errParse)
			}
			cfg.RemoteManagement.SecretKey = "fixture-management-key"
			h := management.NewHandler(cfg, "", nil)
			h.SetLocalPassword("fixture-management-key")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(true)
			s.registerManagementRoutes()
			send := func(key string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/claude-cache", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				rec := httptest.NewRecorder()
				s.engine.ServeHTTP(rec, req)
				return rec
			}
			for _, key := range []string{"", "wrong-key"} {
				if rec := send(key); rec.Code != http.StatusUnauthorized {
					t.Fatalf("key %q: status=%d body=%s", key, rec.Code, rec.Body)
				}
			}
			rec := send("fixture-management-key")
			if rec.Code != http.StatusOK {
				t.Fatalf("authorized: status=%d body=%s", rec.Code, rec.Body)
			}
			var reply struct {
				RuleEnabled *bool   `json:"rule_enabled"`
				Since       *string `json:"since"`
				Days        []struct {
					Day                     string                                `json:"day"`
					MainClientsWith5mWrites *int                                  `json:"main_clients_with_5m_writes"`
					Totals                  map[string]map[string]json.RawMessage `json:"totals"`
					Rule                    map[string]json.RawMessage            `json:"rule"`
					Clients                 []map[string]json.RawMessage          `json:"clients"`
				} `json:"days"`
			}
			if errDecode := json.Unmarshal(rec.Body.Bytes(), &reply); errDecode != nil {
				t.Fatalf("decode %s: %v", rec.Body, errDecode)
			}
			if reply.RuleEnabled == nil || *reply.RuleEnabled != tc.ruleEnabled {
				t.Fatalf("rule_enabled = %v, want %v: %s", reply.RuleEnabled, tc.ruleEnabled, rec.Body)
			}
			if reply.Since == nil {
				t.Fatalf("since missing: %s", rec.Body)
			}
			if _, errParse := time.Parse(time.RFC3339, *reply.Since); errParse != nil {
				t.Fatalf("since = %q: %v", *reply.Since, errParse)
			}
			if len(reply.Days) == 0 || reply.Days[0].Day != time.Now().UTC().Format("2006-01-02") {
				t.Fatalf("days[0] is not today: %s", rec.Body)
			}
			today := reply.Days[0]
			if today.MainClientsWith5mWrites == nil || today.Clients == nil {
				t.Fatalf("today lacks main_clients_with_5m_writes or clients: %s", rec.Body)
			}
			for _, kind := range []string{"main", "subagent", "helper"} {
				for _, field := range []string{"requests", "write_5m", "write_1h", "write_unsplit", "read"} {
					if _, ok := today.Totals[kind][field]; !ok {
						t.Fatalf("totals.%s.%s missing: %s", kind, field, rec.Body)
					}
				}
			}
			for _, field := range []string{"applied", "refused"} {
				if _, ok := today.Rule[field]; !ok {
					t.Fatalf("rule.%s missing: %s", field, rec.Body)
				}
			}
		})
	}
}
