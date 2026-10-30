package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type cachedUsagePoller struct {
	coreauth.ProviderExecutor
	calls atomic.Int64
	fail  atomic.Bool
	stall atomic.Bool
}

func (*cachedUsagePoller) Identifier() string { return "claude" }
func (p *cachedUsagePoller) PollQuota(ctx context.Context, _ *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	p.calls.Add(1)
	if p.stall.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.fail.Load() {
		return nil, &coreauth.Error{HTTPStatus: 429}
	}
	return map[string]coreauth.QuotaWindow{
		"five_hour": {Utilization: 15, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
		"seven_day": {Utilization: 20, ResetsAt: time.Now().Add(24 * time.Hour), ObservedAt: time.Now()},
		"Fable":     {Utilization: 100, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
	}, nil
}

func TestClaudePluginQuotaRejectsStaleSnapshots(t *testing.T) {
	for _, state := range []string{"never polled", "failed", "stalled", "paused"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{RequestRetry: 3}
				cfg.Claude.Quota.Enabled = true
				m := coreauth.NewManager(nil, nil, nil)
				m.SetConfig(cfg)
				seat, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: "seat", Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "test"}})
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				h := &Handler{cfg: cfg, authManager: m}
				host := pluginhost.New()
				host.RegisterPluginForTest("claude", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{identifier: "claude", fetchFn: func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
					t.Fatal("stale snapshot reached plugin normalizer")
					return pluginapi.QuotaFetchResponse{}, nil
				}}}})
				h.SetPluginHost(host)
				poller := &cachedUsagePoller{}
				m.RegisterExecutor(poller)
				if state != "never polled" {
					m.StartQuotaPolling(context.Background())
					t.Cleanup(m.StopQuotaPolling)
					synctest.Wait()
					switch state {
					case "failed":
						poller.fail.Store(true)
					case "stalled":
						poller.stall.Store(true)
					case "paused":
						seat.Disabled = true
						if _, errUpdate := m.Update(context.Background(), seat); errUpdate != nil {
							t.Fatal(errUpdate)
						}
					}
					<-time.After(121 * time.Second)
					synctest.Wait()
				}
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/plugins/claude/quota?auth_index="+seat.Index, nil)
				ctx.Set(ConfigV8ContextKey, true)
				ctx.Params = gin.Params{{Key: "id", Value: "claude"}}
				h.GetPluginQuota(ctx)
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "/claude-seats.html") {
					t.Fatalf("%s reported as fresh: HTTP %d %s", state, rec.Code, rec.Body)
				}
			})
		})
	}
}

func TestCachedClaudeQuotaClientCannotCallUpstream(t *testing.T) {
	client := cachedClaudeQuotaClient{body: []byte(`{}`)}
	for _, req := range []pluginapi.HTTPRequest{
		{Method: http.MethodPost, URL: "https://api.anthropic.com/api/oauth/usage"},
		{Method: http.MethodGet, URL: "https://example.invalid/api/oauth/usage"},
		{Method: http.MethodGet, URL: "https://api.anthropic.com/api/oauth/profile"},
	} {
		if _, errDo := client.Do(context.Background(), req); errDo == nil {
			t.Fatal("accepted uncached request")
		}
	}
	if _, errStream := client.DoStream(context.Background(), pluginapi.HTTPRequest{}); errStream == nil {
		t.Fatal("accepted streaming")
	}
}

func (p *cachedUsagePoller) PollQuotaUsage(ctx context.Context, auth *coreauth.Auth) (map[string]coreauth.QuotaWindow, json.RawMessage, error) {
	windows, errPoll := p.PollQuota(ctx, auth)
	body, _ := json.Marshal(gin.H{
		"five_hour": windows["five_hour"], "seven_day": windows["seven_day"],
		"limits":         []gin.H{{"kind": "weekly_scoped", "scope": gin.H{"model": gin.H{"display_name": "Fable"}}, "percent": 100, "resets_at": time.Now().Add(time.Hour)}},
		"extra_usage":    gin.H{"is_enabled": true, "used_credits": 12},
		"iguana_necktie": gin.H{"limit_dollars": 50, "remaining_dollars": 37},
	})
	return windows, body, errPoll
}

func TestClaudeCachedUsageRoutesAndList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{AuthDir: t.TempDir(), RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		m := coreauth.NewManager(nil, nil, nil)
		m.SetConfig(cfg)
		seat, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: "test-seat", FileName: "test.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": "never-leak-token"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		h := &Handler{cfg: cfg, authManager: m}
		if _, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: "codex-seat", FileName: "codex.json", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": "codex-fixture"}}); errRegister != nil {
			t.Fatal(errRegister)
		}
		host := pluginhost.New()
		host.RegisterPluginForTest("claude", pluginapi.Plugin{Metadata: pluginapi.Metadata{Name: "Claude quota"}, Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{
			identifier: "claude", fetchFn: func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
				response, errFetch := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://api.anthropic.com/api/oauth/usage"})
				if errFetch != nil {
					t.Fatal(errFetch)
				}
				var usage struct {
					ExtraUsage struct {
						UsedCredits float64 `json:"used_credits"`
					} `json:"extra_usage"`
				}
				if errDecode := json.Unmarshal(response.Body, &usage); errDecode != nil {
					t.Fatal(errDecode)
				}
				return pluginapi.QuotaFetchResponse{
					Subscription: &pluginapi.QuotaSubscription{Plan: "selected-plugin-plan", TierName: "test-tier"},
					Summary:      []pluginapi.QuotaMetric{{Key: "credits", Value: usage.ExtraUsage.UsedCredits}},
					Groups:       []pluginapi.QuotaGroup{{DisplayName: "selected-plugin-window"}},
				}, nil
			},
		}}})
		h.SetPluginHost(host)
		poller := &cachedUsagePoller{}
		m.RegisterExecutor(poller)
		call := func(handler gin.HandlerFunc, method, path, body string, v8 bool) *httptest.ResponseRecorder {
			t.Helper()
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
			ctx.Set(ConfigV8ContextKey, v8)
			ctx.Params = gin.Params{{Key: "id", Value: "claude"}}
			handler(ctx)
			if strings.Contains(rec.Body.String(), "never-leak-token") {
				t.Fatal("credential leaked")
			}
			return rec
		}
		path := "/v8/management/credentials/quota?auth_index=" + seat.Index
		fresh := call(h.GetClaudeCredentialQuota, http.MethodGet, path, "", true)
		if fresh.Code != 200 || !strings.Contains(fresh.Body.String(), `"status":"not_polled_yet"`) || !strings.Contains(fresh.Body.String(), `"age_seconds":null`) {
			t.Fatalf("fresh=%s", fresh.Body)
		}
		if poller.calls.Load() != 0 {
			t.Fatal("cache read called upstream")
		}
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		<-time.After(12 * time.Second)
		for range 10 {
			response := call(h.GetClaudeCredentialQuota, "POST", path, `{"auth_index":"`+seat.Index+`"}`, true)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"age_seconds":12`) {
				t.Fatalf("cache=%s", response.Body)
			}
		}
		usageRequest := `{"method":"GET","url":"https://api.anthropic.com/api/oauth/usage","auth_index":"` + seat.Index + `","header":{"Authorization":"Bearer $TOKEN$"}}`
		apiCall := call(h.APICall, "POST", "/v8/management/requests/api-call", usageRequest, true)
		var wrapped apiCallResponse
		if errDecode := json.Unmarshal(apiCall.Body.Bytes(), &wrapped); errDecode != nil {
			t.Fatal(errDecode)
		}
		if wrapped.StatusCode != 200 || !strings.Contains(wrapped.Body, `"percent":100`) || !strings.Contains(wrapped.Body, `"display_name":"Fable"`) {
			t.Fatalf("api-call=%s", apiCall.Body)
		}
		if !strings.Contains(wrapped.Body, `"used_credits":12`) || !strings.Contains(wrapped.Body, `"remaining_dollars":37`) {
			t.Fatal("cache lost optional usage and dollar balances")
		}
		plugin := call(h.GetPluginQuota, "GET", "/v8/management/plugins/claude/quota?auth_index="+seat.Index, "", true)
		if plugin.Code != 200 || !strings.Contains(plugin.Body.String(), `"groups"`) {
			t.Fatalf("plugin=%s", plugin.Body)
		}
		var normalized pluginapi.QuotaFetchResponse
		if errDecode := json.Unmarshal(plugin.Body.Bytes(), &normalized); errDecode != nil {
			t.Fatal(errDecode)
		}
		if normalized.Subscription == nil || normalized.Subscription.Plan != "selected-plugin-plan" || normalized.Subscription.TierName != "test-tier" || len(normalized.Summary) != 1 || normalized.Summary[0].Value != 12 || len(normalized.Groups) != 1 || normalized.Groups[0].DisplayName != "selected-plugin-window" {
			t.Fatalf("plugin normalization lost: %+v", normalized)
		}
		h.SetPluginHost(nil)
		missingPlugin := call(h.GetPluginQuota, "GET", "/v8/management/plugins/claude/quota?auth_index="+seat.Index, "", true)
		if missingPlugin.Code != http.StatusNotFound {
			t.Fatal("cache bypassed plugin validation")
		}
		for _, v8 := range []bool{true, false} {
			list := call(h.ListAuthFiles, "GET", "/v8/management/credentials", "", v8)
			var entries struct {
				Files []map[string]any `json:"files"`
			}
			if errDecode := json.Unmarshal(list.Body.Bytes(), &entries); errDecode != nil {
				t.Fatal(errDecode)
			}
			if len(entries.Files) != 2 {
				t.Fatalf("mixed provider list lost entries: %s", list.Body)
			}
			for _, entry := range entries.Files {
				_, hasUsage := entry["claude_usage"]
				if hasUsage != (v8 && entry["provider"] == "claude") {
					t.Fatalf("incorrect provider cache projection: %v", entry)
				}
			}
			if strings.Contains(list.Body.String(), `"claude_usage"`) != v8 {
				t.Fatalf("list v8=%v: %s", v8, list.Body)
			}
			if v8 && (!strings.Contains(list.Body.String(), `"polls":1`) || !strings.Contains(list.Body.String(), `"age_seconds":12`)) {
				t.Fatalf("list=%s", list.Body)
			}
		}
		synctest.Wait()
		if poller.calls.Load() != 1 {
			t.Fatalf("refresh made %d calls", poller.calls.Load())
		}
		poller.fail.Store(true)
		<-time.After(70 * time.Second)
		synctest.Wait()
		stale := call(h.APICall, "POST", "/v8/management/requests/api-call", usageRequest, true)
		if errDecode := json.Unmarshal(stale.Body.Bytes(), &wrapped); errDecode != nil {
			t.Fatal(errDecode)
		}
		if wrapped.StatusCode != http.StatusServiceUnavailable || !strings.Contains(wrapped.Body, `"age_seconds":82`) || !strings.Contains(wrapped.Body, `"used_credits":12`) {
			t.Fatalf("legacy client received false fresh success or lost retained usage: %s", stale.Body)
		}
		cached := call(h.GetClaudeCredentialQuota, "GET", path, "", true)
		if cached.Code != 200 || !strings.Contains(cached.Body.String(), `"stale":true`) {
			t.Fatal("cache-aware route lost stale usage")
		}
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.Enabled = false
		m.SetConfig(cfg)
		off := call(h.GetClaudeCredentialQuota, "GET", path, "", true)
		if off.Code != 404 {
			t.Fatalf("disabled route=%s", off.Body)
		}
		list := call(h.ListAuthFiles, "GET", "/v8/management/credentials", "", true)
		if strings.Contains(list.Body.String(), `"claude_usage"`) {
			t.Fatal("disabled feature exposed cache")
		}
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Set(ConfigV8ContextKey, true)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/plugins/claude/quota", nil)
		if h.serveCachedClaudeAPICall(ctx, seat) || h.serveCachedClaudePluginQuota(ctx, seat, host, "claude") {
			t.Fatal("disabled feature intercepted upstream request")
		}
	})
}
