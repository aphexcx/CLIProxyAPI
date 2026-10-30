package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codexQuotaFixtureTransport func(*http.Request) (*http.Response, error)

func (f codexQuotaFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexQuotaPollUsesOuterSeatAndSanitizesUsage(t *testing.T) {
	const fixture = `{"access_token":"must-not-survive","plan_type":"pro","credits":{"balance":"12","unlimited":false},"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":1893456000},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":123}},"additional_rate_limits":[{"limit_name":"Spark","rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":18000,"reset_at":1893456000}}}],"code_review_rate_limit":{"primary_window":{"used_percent":7,"limit_window_seconds":604800,"reset_at":1893456000}}}`
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{
		"access_token": "fixture-outer", "account_id": "fixture-account", "use_siwc": true,
		"siwc": map[string]any{"access_token": "fixture-siwc"},
	}}
	calls := 0
	rt := codexQuotaFixtureTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "GET" || req.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || req.Body != nil {
			t.Fatal("unexpected usage request")
		}
		if req.Header.Get("Authorization") != "Bearer fixture-outer" || req.Header.Get("Chatgpt-Account-Id") != "fixture-account" || req.Header.Get("Originator") != "codex-tui" || req.Header.Get("User-Agent") == "" {
			t.Fatal("usage request did not use the original seat headers")
		}
		if _, deadline := req.Context().Deadline(); deadline {
			t.Fatal("established usage read has a deadline")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fixture))}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
	// The service binds the auto executor, which must expose the poll interfaces.
	executor := NewCodexAutoExecutor(&config.Config{})
	windows, usage, errPoll := executor.PollQuotaUsage(ctx, auth)
	if errPoll != nil || calls != 1 || len(windows) != 4 || windows["five_hour"].Utilization != 42 || windows["seven_day"].Utilization != 25 || windows["additional/spark/five_hour"].Utilization != 9 || windows["code_review/seven_day"].Utilization != 7 {
		t.Fatalf("unexpected fixture result: windows=%+v err=%v calls=%d", windows, errPoll, calls)
	}
	if windows["five_hour"].ObservedAt.IsZero() || strings.Contains(string(usage), "token") || strings.Contains(string(usage), "fixture-") || !strings.Contains(string(usage), `"plan_type":"pro"`) {
		t.Fatalf("invalid retained usage: %s", usage)
	}
}

func TestCodexQuotaPollErrorsDoNotRetainUpstreamBody(t *testing.T) {
	for _, status := range []int{200, 302, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			rt := codexQuotaFixtureTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://example.invalid/secret"}}, Body: io.NopCloser(strings.NewReader(`{"error":"fixture-secret"}`))}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
			auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "fixture", "account_id": "account"}}
			windows, usage, errPoll := NewCodexExecutor(&config.Config{}).PollQuotaUsage(ctx, auth)
			if errPoll == nil || strings.Contains(errPoll.Error(), "fixture-secret") || len(windows) != 0 || len(usage) != 0 || calls != 1 {
				t.Fatalf("invalid error handling: %v calls=%d", errPoll, calls)
			}
		})
	}
}
