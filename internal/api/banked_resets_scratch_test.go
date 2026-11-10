package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type scratchResetPoller struct {
	coreauth.ProviderExecutor
	client *claudeauth.ClaudeAuth
}

func (*scratchResetPoller) Identifier() string { return "claude" }
func (p *scratchResetPoller) PollQuota(ctx context.Context, auth *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	windows, _, err := p.PollQuotaUsage(ctx, auth)
	return windows, err
}
func (p *scratchResetPoller) PollQuotaUsage(ctx context.Context, auth *coreauth.Auth) (map[string]coreauth.QuotaWindow, json.RawMessage, error) {
	windows, body, err := p.client.FetchOAuthUsageSnapshot(ctx, auth.Metadata["access_token"].(string))
	converted := make(map[string]coreauth.QuotaWindow, len(windows))
	for name, w := range windows {
		converted[name] = coreauth.QuotaWindow{Utilization: w.Utilization, ResetsAt: w.ResetsAt, ObservedAt: w.ObservedAt}
	}
	return converted, body, err
}

func (p *scratchResetPoller) PollLimitResets(ctx context.Context, auth *coreauth.Auth) (json.RawMessage, error) {
	return p.client.FetchOAuthLimitResets(ctx, auth.Metadata["access_token"].(string))
}

type scratchResetTransport struct{ handler http.Handler }

func (s scratchResetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "api.anthropic.com" {
		return nil, fmt.Errorf("fixture transport refuses external traffic")
	}
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, req)
	return response.Result(), nil
}

// This fixture exercises ClaudeAuth and the real quota loop through its existing
// HTTP-client injection seam. The usage stub is in-process and cannot dial.
func TestBankedResetsScratch(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	// The handler's process-lifetime cleanup goroutine must live outside the
	// virtual-time bubble; polling itself is controlled by the bubble's clock.
	h := management.NewHandler(&config.Config{}, "", nil)
	if os.Getenv("CP_BHA_PROOF_DIR") != "" {
		runBankedResetsScratch(t, h)
		return
	}
	synctest.Test(t, func(t *testing.T) { runBankedResetsScratch(t, h) })
}

func runBankedResetsScratch(t *testing.T, h *management.Handler) {
	now := time.Now()
	var upstreamCalls atomic.Int32
	fixture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != "GET" || (r.URL.RequestURI() != "/api/oauth/usage?cedar_ember=1&skip_spend=1" && r.URL.RequestURI() != "/api/oauth/usage") || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || r.Header.Get("Authorization") != "Bearer scratch-only-token" {
			t.Error("unexpected fixture request shape")
			http.Error(w, "fixture request rejected", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gin.H{
			"five_hour":   gin.H{"utilization": 60, "resets_at": now.Add(4 * time.Hour)},
			"seven_day":   gin.H{"utilization": 85, "resets_at": now.Add(72 * time.Hour)},
			"limits":      []gin.H{{"scope": gin.H{"model": gin.H{"display_name": "Fable"}}, "percent": 100, "resets_at": now.Add(72 * time.Hour)}},
			"cedar_ember": gin.H{"eligible": true, "ineligible_reason": nil, "at_limit": true, "weekly_resets_at": now.Add(72 * time.Hour), "cooldown_until": nil, "grants": []gin.H{{"id": "scratch-launch", "label": "Fixture launch", "resets_left": 2, "resets_total": 3, "ends_at": now.Add(7 * 24 * time.Hour), "type": "weekly", "clears": []string{"seven_day_overage_included"}, "usable_now": true}}},
		})
	})
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("host: 127.0.0.1\nport: 0\nauth-dir: %q\nremote-management: {secret-key: scratch-only-key}\n", authDir)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Claude.Quota.Enabled = true
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "scratch-seat", FileName: "scratch-seat.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"path": filepath.Join(authDir, "scratch-seat.json")}, Metadata: map[string]any{"access_token": "scratch-only-token"}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "scratch-seat.json"), []byte(`{"type":"claude","access_token":"scratch-only-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	m.RegisterExecutor(&scratchResetPoller{client: claudeauth.NewClaudeAuth(cfg, claudeauth.WithHTTPClient(&http.Client{Transport: scratchResetTransport{fixture}}))})
	m.StartQuotaPolling(context.Background())
	defer m.StopQuotaPolling()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(80 * time.Second)
	defer deadline.Stop()
	for quota, _ := m.ClaudeQuotaCached(auth.ID); m.ClaudeLimitResetsCached(auth.ID).CedarEmber == nil || quota.Stale; quota, _ = m.ClaudeQuotaCached(auth.ID) {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("fixture usage poll did not populate reset status")
		}
	}
	h.SetConfig(cfg)
	h.SetAuthManager(m)
	h.SetLocalPassword("scratch-only-key")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	s.engine.GET("/claude-seats.html", s.serveClaudeSeats)
	if panelFile := os.Getenv("CP_BHA_PANEL_HTML"); panelFile != "" {
		s.engine.GET("/management.html", func(c *gin.Context) { c.File(panelFile) })
	}
	done := make(chan struct{})
	var finish sync.Once
	if os.Getenv("CP_BHA_PROOF_DIR") != "" {
		s.engine.POST("/proof/done", func(c *gin.Context) { finish.Do(func() { close(done) }); c.Status(204) })
	}
	hub := httptest.NewServer(s.engine)
	defer hub.Close()
	request := func(method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, hub.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer scratch-only-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hub.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s HTTP %d: %s", path, resp.StatusCode, data)
		}
		return data
	}
	for _, route := range []string{"/v0/management/api-call", "/v8/management/requests/api-call"} {
		request("POST", route, `{"method":"POST","url":"https://fixture.invalid/api/org/%72eset_rate_limits/"}`, 403)
	}
	status := request("GET", "/v8/management/claude/limit-resets?auth=scratch-seat.json", "", 200)
	if !strings.Contains(string(status), `"best_click_rank":1`) || !strings.Contains(string(status), `"resets_left":2`) {
		t.Fatalf("wrong cached status: %s", status)
	}
	request("GET", "/claude-seats.html", "", 200)
	if files := request("GET", "/v8/management/credentials", "", 200); !strings.Contains(string(files), `"name":"scratch-seat.json"`) {
		t.Fatal("fixture seat missing from credentials listing")
	}
	for range 5 {
		request("GET", "/v8/management/claude/limit-resets", "", 200)
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("management GETs caused %d upstream calls", upstreamCalls.Load())
	}
	if proofDir := os.Getenv("CP_BHA_PROOF_DIR"); proofDir != "" {
		manifest, _ := json.MarshalIndent(gin.H{"hub": hub.URL, "fixture": "in-process usage handler", "config": configPath, "auth_dir": authDir, "upstream_calls": upstreamCalls.Load(), "status": json.RawMessage(status), "both_routes": 403, "fixture_only": true}, "", "  ")
		if err := os.WriteFile(filepath.Join(proofDir, "scratch-proof.json"), manifest, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log("scratch fixture ready for browser proof at " + hub.URL)
		select {
		case <-done:
		case <-time.After(15 * time.Minute):
			t.Fatal("browser proof did not finish")
		}
	}
}
