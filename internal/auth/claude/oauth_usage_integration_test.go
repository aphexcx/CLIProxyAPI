package claude_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/sjson"
)

type usageRoundTripper func(*http.Request) (*http.Response, error)

func (f usageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Only HTTP I/O and the executor's result-type adapter are test doubles. The
// FetchOAuthUsage request/parser/receipt, polling, reconcile and v8 view are real.
type usagePoller struct {
	coreauth.ProviderExecutor
	client *claude.ClaudeAuth
}

func (*usagePoller) Identifier() string { return "claude" }

func (p *usagePoller) PollQuota(ctx context.Context, auth *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	token, _ := auth.Metadata["access_token"].(string)
	windows, errFetch := p.client.FetchOAuthUsage(ctx, token)
	result := make(map[string]coreauth.QuotaWindow, len(windows))
	for name, window := range windows {
		result[name] = coreauth.QuotaWindow{Utilization: window.Utilization, ResetsAt: window.ResetsAt, ObservedAt: window.ObservedAt}
	}
	return result, errFetch
}

func scopedUsageFixture(t *testing.T, percent float64, reset time.Time, includeFable bool) string {
	t.Helper()
	body, errRead := os.ReadFile("testdata/oauth_usage_scoped_limits.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	for path, value := range map[string]any{"limits.0.percent": percent, "limits.0.resets_at": reset.Format(time.RFC3339Nano)} {
		var errSet error
		body, errSet = sjson.SetBytes(body, path, value)
		if errSet != nil {
			t.Fatal(errSet)
		}
	}
	if !includeFable {
		var errDelete error
		body, errDelete = sjson.DeleteBytes(body, "limits")
		if errDelete != nil {
			t.Fatal(errDelete)
		}
	}
	return string(body)
}

func usageResponse(req *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func registerUsageSeat(t *testing.T, m *coreauth.Manager, id string) {
	t.Helper()
	_, errRegister := m.Register(context.Background(), &coreauth.Auth{
		ID: id, Provider: "claude", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": id}, Attributes: map[string]string{"runtime_only": "true", "priority": "4"},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
}

func TestClaudeQuotaScopedUsagePollV8Credentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		reset := now.Add(time.Hour)
		bodies := map[string]string{
			"at-cap":   scopedUsageFixture(t, 100, reset, true),
			"no-fable": scopedUsageFixture(t, 0, reset, false),
			"soonest":  scopedUsageFixture(t, 20, reset, true),
			"later":    scopedUsageFixture(t, 20, reset.Add(time.Hour), true),
		}
		var mu sync.Mutex
		calls := make(map[string]int)
		client := claude.NewClaudeAuthForTest(&http.Client{Transport: usageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || req.URL.String() != claude.UsageURL || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
				t.Error("unexpected usage request")
			}
			id := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			calls[id]++
			mu.Unlock()
			body, ok := bodies[id]
			if !ok {
				t.Error("unexpected test credential")
			}
			return usageResponse(req, body), nil
		})})
		m := coreauth.NewManager(nil, nil, nil)
		cfg := &config.Config{RequestRetry: 3, AuthDir: t.TempDir()}
		cfg.Claude.Quota.Enabled, cfg.Claude.Quota.PriorityAtReset = true, true
		m.SetConfig(cfg)
		m.RegisterExecutor(&usagePoller{client: client})
		for id := range bodies {
			registerUsageSeat(t, m, id)
		}
		m.StartQuotaPolling(context.Background())
		defer m.StopQuotaPolling()
		synctest.Wait()
		mu.Lock()
		for id := range bodies {
			if calls[id] != 1 {
				t.Errorf("seat %s: polls=%d, want exactly one", id, calls[id])
			}
		}
		mu.Unlock()

		h := &management.Handler{}
		h.SetConfig(cfg)
		h.SetAuthManager(m)
		router := gin.New()
		router.GET("/v8/management/credentials", func(c *gin.Context) {
			c.Set(management.ConfigV8ContextKey, true)
			h.ListAuthFiles(c)
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("credentials status=%d", rec.Code)
		}
		var payload struct {
			Files []struct {
				ID       string `json:"id"`
				Priority int    `json:"priority"`
				Rank     int    `json:"quota_rank"`
				Quota    struct {
					Windows map[string]coreauth.QuotaWindow `json:"windows"`
				} `json:"quota"`
				Cooldowns []coreauth.CooldownView `json:"cooldowns"`
			} `json:"files"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(payload.Files) != len(bodies) {
			t.Fatalf("credentials count=%d", len(payload.Files))
		}
		for _, entry := range payload.Files {
			wantRank := 0
			if entry.ID == "soonest" {
				wantRank = 1
			}
			if entry.Priority != 4 || entry.Rank != wantRank {
				t.Errorf("seat %s: priority=%d rank=%d, want 4/%d", entry.ID, entry.Priority, entry.Rank, wantRank)
			}
			window, hasFable := entry.Quota.Windows["Fable"]
			if entry.ID == "no-fable" {
				if hasFable {
					t.Error("missing model limit manufactured a Fable window")
				}
			} else {
				wantPercent, wantReset := 20.0, reset
				if entry.ID == "at-cap" {
					wantPercent = 100
				}
				if entry.ID == "later" {
					wantReset = reset.Add(time.Hour)
				}
				if !hasFable || window.Utilization != wantPercent || !window.ResetsAt.Equal(wantReset) || !window.ObservedAt.Equal(now) {
					t.Errorf("seat %s: incorrect Fable window %+v", entry.ID, window)
				}
			}
			auth, _ := m.GetByID(entry.ID)
			if auth.Disabled || auth.Quota.Exceeded || len(entry.Quota.Windows) < 2 {
				t.Errorf("seat %s: account windows missing or seat disabled/held", entry.ID)
			}
			if entry.ID == "at-cap" {
				state := auth.ModelStates["claude-fable-5-1"]
				if state == nil || !state.Unavailable || state.Quota.Reason != "quota_cap" || !state.NextRetryAfter.Equal(reset) || !state.Quota.NextRecoverAt.Equal(reset) {
					t.Errorf("missing Fable hold: %+v", state)
				}
				if len(entry.Cooldowns) != 1 || entry.Cooldowns[0].Scope != "model" || entry.Cooldowns[0].Reason != "quota_cap" {
					t.Errorf("incorrect hold projection: %+v", entry.Cooldowns)
				}
			} else if len(entry.Cooldowns) != 0 {
				t.Errorf("seat %s unexpectedly held: %+v", entry.ID, entry.Cooldowns)
			}
		}
	})
}

func TestClaudeQuotaScopedUsagePollHeaderReceiptMerge(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pollPercent    float64
		headerFraction string
		headerOffset   time.Duration
		wantPercent    float64
	}{
		{"newer poll releases header hold", 0, "1.0", -time.Second, 0},
		{"newer poll holds after under-cap header", 100, "0.0", -time.Second, 100},
		{"newer header holds despite delayed poll", 0, "1.0", time.Second, 100},
		{"newer header releases despite delayed poll", 100, "0.0", time.Second, 0},
		{"tie keeps header hold", 0, "1.0", 0, 100},
		{"tie accepts poll hold", 100, "0.0", 0, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := coreauth.NewManager(nil, nil, nil)
				cfg := &config.Config{RequestRetry: 3}
				cfg.Claude.Quota.Enabled = true
				m.SetConfig(cfg)
				registerUsageSeat(t, m, "test-seat")
				reset := time.Now().Add(time.Hour)
				header := func() {
					ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
					internallogging.SetResponseHeaders(ctx, http.Header{
						"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {tc.headerFraction},
						"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {reset.Format(time.RFC3339Nano)},
					})
					m.MarkResult(ctx, coreauth.Result{AuthID: "test-seat", Provider: "claude", Model: "claude-fable-5-1", Success: true})
				}
				if tc.headerOffset < 0 {
					header()
					<-time.After(-tc.headerOffset)
				}
				bodyReady := make(chan struct{})
				body := scopedUsageFixture(t, tc.pollPercent, reset, true)
				client := claude.NewClaudeAuthForTest(&http.Client{Transport: usageRoundTripper(func(req *http.Request) (*http.Response, error) {
					resp := usageResponse(req, body)
					resp.Body = &waitingUsageBody{ReadCloser: resp.Body, ready: bodyReady}
					return resp, nil
				})})
				m.RegisterExecutor(&usagePoller{client: client})
				m.StartQuotaPolling(context.Background())
				defer m.StopQuotaPolling()
				synctest.Wait() // Poll headers received; body is still blocked.
				if tc.headerOffset >= 0 {
					if tc.headerOffset > 0 {
						<-time.After(tc.headerOffset)
					}
					header()
				}
				<-time.After(time.Second)
				close(bodyReady)
				synctest.Wait()
				auth, _ := m.GetByID("test-seat")
				if window := auth.Quota.Windows["Fable"]; window.Utilization != tc.wantPercent || !window.ResetsAt.Equal(reset) {
					t.Errorf("merged window=%+v, want percent=%v reset=%v", window, tc.wantPercent, reset)
				}
				state := auth.ModelStates["claude-fable-5-1"]
				held := state != nil && state.Unavailable && state.Quota.Reason == "quota_cap" && state.NextRetryAfter.Equal(reset)
				if held != (tc.wantPercent == 100) {
					t.Errorf("incorrect hold after receipt merge: %+v", state)
				}
			})
		})
	}
}

type waitingUsageBody struct {
	io.ReadCloser
	ready <-chan struct{}
}

func (b *waitingUsageBody) Read(p []byte) (int, error) {
	<-b.ready
	return b.ReadCloser.Read(p)
}
