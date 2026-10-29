package claude

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseOAuthUsage(t *testing.T) {
	reset := time.Date(2026, 11, 2, 2, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
		want       map[string]OAuthUsageWindow
		fail       bool
	}{
		{"all windows", `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-02T02:59:00Z"},"seven_day":{"utilization":100,"resets_at":"2026-11-02T02:59:00Z"},"limits":[{"display_name":"Fable","percent":100.5,"resets_at":"2026-11-02T02:59:00Z"}]}`, map[string]OAuthUsageWindow{"five_hour": {Utilization: 12.5, ResetsAt: reset}, "seven_day": {Utilization: 100, ResetsAt: reset}, "Fable": {Utilization: 100.5, ResetsAt: reset}}, false},
		{"zero is valid", `{"limits":[{"display_name":"Fable","percent":0,"resets_at":"2026-11-02T02:59:00.000Z"}]}`, map[string]OAuthUsageWindow{"Fable": {Utilization: 0, ResetsAt: reset}}, false},
		{"missing is not zero", `{"five_hour":null,"limits":[{"display_name":"Fable","resets_at":"2026-11-02T02:59:00Z"}]}`, map[string]OAuthUsageWindow{}, false},
		{"invalid reset and negative usage", `{"five_hour":{"utilization":99,"resets_at":"invalid"},"seven_day":{"utilization":-1,"resets_at":"2026-11-02T02:59:00Z"}}`, map[string]OAuthUsageWindow{}, false},
		{"invalid longer model window is omitted", `{"limits":[{"display_name":"Fable","percent":100,"resets_at":"2026-11-02T02:59:00Z"},{"display_name":"Additional","percent":100,"resets_at":"invalid"}]}`, map[string]OAuthUsageWindow{"Fable": {Utilization: 100, ResetsAt: reset}}, false},
		{"reserved name", `{"limits":[{"display_name":"five_hour","percent":100,"resets_at":"2026-11-02T02:59:00Z"}]}`, map[string]OAuthUsageWindow{}, false},
		{"malformed", `{"secret":`, nil, true},
		{"null", `null`, nil, true},
		{"wrong percentage type", `{"five_hour":{"utilization":"secret"}}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, errParse := parseOAuthUsage([]byte(tc.body))
			if (errParse != nil) != tc.fail || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("windows=%v err=%v, want %v fail=%v", got, errParse, tc.want, tc.fail)
			}
			if errParse != nil && strings.Contains(errParse.Error(), "secret") {
				t.Fatal("body leaked into error")
			}
		})
	}
}

func TestFetchOAuthUsageCurrentTokenOnly(t *testing.T) {
	calls := 0
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != UsageURL || req.Method != http.MethodGet || req.Body != nil {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer test-access" || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
			t.Fatal("usage headers missing")
		}
		if _, ok := req.Context().Deadline(); ok {
			t.Fatal("usage read added a deadline")
		}
		return jsonResponse(req, `{"limits":[{"display_name":"Fable","percent":100,"resets_at":"2026-11-02T02:59:00Z"}]}`), nil
	})}}
	windows, errFetch := auth.FetchOAuthUsage(context.Background(), "test-access")
	if errFetch != nil || calls != 1 || windows["Fable"].Utilization != 100 {
		t.Fatalf("calls=%d windows=%v err=%v", calls, windows, errFetch)
	}
}

type delayedUsageBody struct {
	io.Reader
	release <-chan struct{}
}

func (b *delayedUsageBody) Read(p []byte) (int, error) {
	<-b.release
	return b.Reader.Read(p)
}

func TestClaudeQuotaUsageCapturesReceiptBeforeBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bodyReady := make(chan struct{})
		done := make(chan struct{})
		receipt := time.Now()
		auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp := jsonResponse(req, "")
			resp.Body = io.NopCloser(&delayedUsageBody{
				Reader:  strings.NewReader(`{"limits":[{"display_name":"Fable","percent":0,"resets_at":"2026-11-02T02:59:00Z"}]}`),
				release: bodyReady,
			})
			return resp, nil
		})}}
		var windows map[string]OAuthUsageWindow
		var errFetch error
		go func() {
			windows, errFetch = auth.FetchOAuthUsage(context.Background(), "test-access")
			close(done)
		}()
		synctest.Wait()
		<-time.After(2 * time.Second)
		close(bodyReady)
		<-done
		if errFetch != nil {
			t.Fatal(errFetch)
		}
		if got := windows["Fable"].ObservedAt; !got.Equal(receipt) {
			t.Fatalf("usage receipt=%v, want headers at %v, completion at %v", got, receipt, time.Now())
		}
	})
}

type delayedBodyQuotaPoller struct {
	coreauth.ProviderExecutor
	auth *ClaudeAuth
}

func (*delayedBodyQuotaPoller) Identifier() string { return "claude" }

func (p *delayedBodyQuotaPoller) PollQuota(ctx context.Context, _ *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	windows, errFetch := p.auth.FetchOAuthUsage(ctx, "test-access")
	result := make(map[string]coreauth.QuotaWindow, len(windows))
	for name, window := range windows {
		result[name] = coreauth.QuotaWindow{Utilization: window.Utilization, ResetsAt: window.ResetsAt, ObservedAt: window.ObservedAt}
	}
	return result, errFetch
}

func TestClaudeQuotaDelayedUsageBodyCannotReleaseHeaderHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bodyReady := make(chan struct{})
		usage := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp := jsonResponse(req, "")
			resp.Body = io.NopCloser(&delayedUsageBody{
				Reader:  strings.NewReader(`{"limits":[{"display_name":"Fable","percent":0,"resets_at":"2026-11-02T02:59:00Z"}]}`),
				release: bodyReady,
			})
			return resp, nil
		})}}
		m := coreauth.NewManager(nil, nil, nil)
		cfg := &config.Config{RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		m.SetConfig(cfg)
		id := t.Name()
		if _, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "test-access"}}); errRegister != nil {
			t.Fatal(errRegister)
		}
		m.RegisterExecutor(&delayedBodyQuotaPoller{auth: usage})
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait() // T0: usage response headers received, body blocked.
		<-time.After(time.Second)
		ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.0"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {"2026-11-02T02:59:00Z"},
		})
		m.MarkResult(ctx, coreauth.Result{AuthID: id, Provider: "claude", Model: "claude-fable-5-1", Success: true}) // T1.
		<-time.After(time.Second)
		close(bodyReady) // T2: the older usage body completes.
		synctest.Wait()
		auth, _ := m.GetByID(id)
		state := auth.ModelStates["claude-fable-5-1"]
		if auth.Quota.Windows["Fable"].Utilization != 100 || state == nil || !state.Unavailable || state.Quota.Reason != "quota_cap" || !state.NextRetryAfter.After(time.Now()) {
			t.Fatalf("delayed usage body cleared the newer header hold: window=%+v state=%+v", auth.Quota.Windows["Fable"], state)
		}
	})
}
