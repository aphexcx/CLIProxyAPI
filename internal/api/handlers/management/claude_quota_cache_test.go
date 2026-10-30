package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type cachedUsagePoller struct {
	coreauth.ProviderExecutor
	calls int
}

func (*cachedUsagePoller) Identifier() string { return "claude" }
func (p *cachedUsagePoller) PollQuota(context.Context, *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	p.calls++
	return map[string]coreauth.QuotaWindow{
		"five_hour": {Utilization: 15, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
		"seven_day": {Utilization: 20, ResetsAt: time.Now().Add(24 * time.Hour), ObservedAt: time.Now()},
		"Fable":     {Utilization: 100, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()},
	}, nil
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
		if poller.calls != 0 {
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
		plugin := call(h.GetPluginQuota, "GET", "/v8/management/plugins/claude/quota?auth_index="+seat.Index, "", true)
		if plugin.Code != 200 || !strings.Contains(plugin.Body.String(), `"groups"`) {
			t.Fatalf("plugin=%s", plugin.Body)
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
		if poller.calls != 1 {
			t.Fatalf("refresh made %d calls", poller.calls)
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
