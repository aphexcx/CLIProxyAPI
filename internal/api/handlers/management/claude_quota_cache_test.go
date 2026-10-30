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
}

func (*cachedUsagePoller) Identifier() string { return "claude" }
func (p *cachedUsagePoller) PollQuota(context.Context, *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	p.calls.Add(1)
	if p.fail.Load() {
		return nil, &coreauth.Error{HTTPStatus: 429}
	}
	return map[string]coreauth.QuotaWindow{
		"five_hour": {Utilization: 15, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
		"seven_day": {Utilization: 20, ResetsAt: time.Now().Add(24 * time.Hour), ObservedAt: time.Now()},
		"Fable":     {Utilization: 100, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
	}, nil
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
		host := pluginhost.New()
		host.RegisterPluginForTest("claude", pluginapi.Plugin{Metadata: pluginapi.Metadata{Name: "Claude quota"}, Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{
			identifier: "claude", fetchFn: func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
				t.Fatal("Claude quota plugin fetched upstream instead of using the cache")
				return pluginapi.QuotaFetchResponse{}, nil
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
		h.SetPluginHost(nil)
		missingPlugin := call(h.GetPluginQuota, "GET", "/v8/management/plugins/claude/quota?auth_index="+seat.Index, "", true)
		if missingPlugin.Code != http.StatusNotFound {
			t.Fatal("cache bypassed plugin validation")
		}
		for _, v8 := range []bool{true, false} {
			list := call(h.ListAuthFiles, "GET", "/v8/management/credentials", "", v8)
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
		if h.serveCachedClaudeAPICall(ctx, seat) || h.serveCachedClaudePluginQuota(ctx, seat) {
			t.Fatal("disabled feature intercepted upstream request")
		}
	})
}
