package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
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

func TestCodexQuotaPollRetainsSanitizedResetCreditSummary(t *testing.T) {
	for _, tc := range []struct {
		name, summary, want string
	}{
		{"counts", `"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":0,"access_token":"secret","credits":[{"id":"private"}]}`, `{"available_count":2,"applicable_available_count":0}`},
		{"aliases", `"rateLimitResetCredits":{"availableCount":"1","applicableAvailableCount":"0"}`, `{"available_count":1,"applicable_available_count":0}`},
		{"zero", `"rate_limit_reset_credits":{"available_count":0}`, `{"available_count":0}`},
		{"invalid", `"rate_limit_reset_credits":{"available_count":-1,"applicable_available_count":true}`, `null`},
		{"fractional", `"rate_limit_reset_credits":{"available_count":1.5,"applicable_available_count":"NaN"}`, `null`},
		{"nonfinite", `"rate_limit_reset_credits":{"available_count":"Infinity","applicable_available_count":{}}`, `null`},
		{"array", `"rate_limit_reset_credits":[{"available_count":1}]`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := `{"rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":1893456000}},` + tc.summary + `}`
			rt := codexQuotaFixtureTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fixture))}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
			auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "fixture", "account_id": "account"}}
			_, usage, errPoll := NewCodexExecutor(&config.Config{}).PollQuotaUsage(ctx, auth)
			if errPoll != nil {
				t.Fatal(errPoll)
			}
			var payload map[string]json.RawMessage
			if errDecode := json.Unmarshal(usage, &payload); errDecode != nil {
				t.Fatal(errDecode)
			}
			got := payload["rate_limit_reset_credits"]
			if tc.want == "null" {
				if len(got) != 0 {
					t.Fatalf("retained invalid summary: %s", got)
				}
			} else {
				var gotCounts, wantCounts map[string]float64
				if errDecode := json.Unmarshal(got, &gotCounts); errDecode != nil {
					t.Fatalf("missing or invalid reset credit summary: %s (%v)", got, errDecode)
				}
				if errDecode := json.Unmarshal([]byte(tc.want), &wantCounts); errDecode != nil {
					t.Fatal(errDecode)
				}
				if !reflect.DeepEqual(gotCounts, wantCounts) {
					t.Fatalf("reset credit summary = %s, want %s", got, tc.want)
				}
			}
			if strings.Contains(string(usage), "secret") || strings.Contains(string(usage), "private") || strings.Contains(string(usage), "access_token") {
				t.Fatalf("retained arbitrary credit metadata: %s", usage)
			}
		})
	}
}
