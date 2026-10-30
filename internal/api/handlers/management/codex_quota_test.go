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

type cachedCodexTestPoller struct{ cachedUsagePoller }

func (*cachedCodexTestPoller) Identifier() string { return "codex" }

func TestCodexCachedUsageRoutesAndList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{AuthDir: t.TempDir(), RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		m := coreauth.NewManager(nil, nil, nil)
		m.SetConfig(cfg)
		seat, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: "codex", FileName: "codex.json", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": "never-leak-token", "account_id": "fixture-account"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		h := &Handler{cfg: cfg, authManager: m}
		poller := &cachedCodexTestPoller{}
		m.RegisterExecutor(poller)
		call := func(handler gin.HandlerFunc, method, path, body string, v8 bool) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
			ctx.Set(ConfigV8ContextKey, v8)
			handler(ctx)
			if strings.Contains(rec.Body.String(), "never-leak-token") {
				t.Fatal("credential leaked")
			}
			return rec
		}
		path := "/v8/management/credentials/quota?auth_index=" + seat.Index
		before := call(h.GetClaudeCredentialQuota, "GET", path, "", true)
		if before.Code != 200 || !strings.Contains(before.Body.String(), `"status":"not_polled_yet"`) || poller.calls.Load() != 0 {
			t.Fatalf("before: %s", before.Body)
		}
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		for _, method := range []string{"GET", "POST"} {
			for range 3 {
				response := call(h.GetClaudeCredentialQuota, method, path, `{"auth_index":"`+seat.Index+`"}`, true)
				if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"used_percent":15`) || !strings.Contains(response.Body.String(), `"limit_window_seconds":604800`) {
					t.Fatalf("quota: %s", response.Body)
				}
			}
		}
		apiBody := `{"method":"GET","url":"https://chatgpt.com/backend-api/wham/usage","auth_index":"` + seat.Index + `"}`
		checkAPICall := func(want int) {
			response := call(h.APICall, "POST", "/v8/management/requests/api-call", apiBody, true)
			var wrapped apiCallResponse
			if errDecode := json.Unmarshal(response.Body.Bytes(), &wrapped); errDecode != nil {
				t.Fatal(errDecode)
			}
			if response.Code != 200 || wrapped.StatusCode != want || !strings.Contains(wrapped.Body, `"used_percent":15`) {
				t.Fatalf("api-call: %s", response.Body)
			}
		}
		checkAPICall(200)
		if poller.calls.Load() != 1 {
			t.Fatal("management reads bypassed poll budget")
		}
		for _, v8 := range []bool{true, false} {
			response := call(h.ListAuthFiles, "GET", "/v8/management/credentials", "", v8)
			if strings.Contains(response.Body.String(), `"codex_usage"`) != v8 || strings.Contains(response.Body.String(), `"claude_usage"`) {
				t.Fatalf("list: %s", response.Body)
			}
		}
		poller.fail.Store(true)
		<-time.After(70 * time.Second)
		synctest.Wait()
		failed := call(h.GetClaudeCredentialQuota, "GET", path, "", true)
		if !strings.Contains(failed.Body.String(), `"stale":true`) || !strings.Contains(failed.Body.String(), `"last_status":429`) || !strings.Contains(failed.Body.String(), `"used_percent":15`) {
			t.Fatalf("failure: %s", failed.Body)
		}
		checkAPICall(http.StatusServiceUnavailable)
		cfg = cfg.CloneForRuntime()
		cfg.Claude.Quota.Enabled = false
		m.SetConfig(cfg)
		if response := call(h.GetClaudeCredentialQuota, "GET", path, "", true); response.Code != 404 {
			t.Fatalf("disabled: %s", response.Body)
		}
	})
}
