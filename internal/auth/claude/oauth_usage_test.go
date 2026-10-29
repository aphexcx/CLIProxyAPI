package claude

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseOAuthUsage(t *testing.T) {
	reset := time.Date(2026, 11, 2, 2, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
		want       map[string]OAuthUsageWindow
		fail       bool
	}{
		{"all windows", `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-02T02:59:00Z"},"seven_day":{"utilization":100,"resets_at":"2026-11-02T02:59:00Z"},"limits":[{"display_name":"Fable","percent":100.5,"resets_at":"2026-11-02T02:59:00Z"}]}`, map[string]OAuthUsageWindow{"five_hour": {12.5, reset}, "seven_day": {100, reset}, "Fable": {100.5, reset}}, false},
		{"zero is valid", `{"limits":[{"display_name":"Fable","percent":0,"resets_at":"2026-11-02T02:59:00.000Z"}]}`, map[string]OAuthUsageWindow{"Fable": {0, reset}}, false},
		{"missing is not zero", `{"five_hour":null,"limits":[{"display_name":"Fable","resets_at":"2026-11-02T02:59:00Z"}]}`, map[string]OAuthUsageWindow{}, false},
		{"invalid reset and negative usage", `{"five_hour":{"utilization":99,"resets_at":"invalid"},"seven_day":{"utilization":-1,"resets_at":"2026-11-02T02:59:00Z"}}`, map[string]OAuthUsageWindow{}, false},
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
