package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOAuthProfilePlanTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"max 20x", `{"organization":{"rate_limit_tier":"default_claude_max_20x","organization_type":"claude_max"}}`, "Max 20x"},
		{"max 5x", `{"organization":{"rate_limit_tier":"default_claude_max_5x","organization_type":"claude_max"}}`, "Max 5x"},
		{"team premium reuses max vocabulary", `{"organization":{"rate_limit_tier":"default_claude_max_5x","organization_type":"claude_team"}}`, "Team premium"},
		{"team standard", `{"organization":{"rate_limit_tier":"default_raven","organization_type":"claude_team"}}`, "Team standard"},
		{"team without tier", `{"organization":{"organization_type":"claude_team"}}`, "Team"},
		{"team with an unseen tier", `{"organization":{"rate_limit_tier":" default_new ","organization_type":"claude_team"}}`, "default_new"},
		{"enterprise", `{"organization":{"rate_limit_tier":"default_claude_max_20x","organization_type":"claude_enterprise"}}`, "Enterprise"},
		{"max flag without tier", `{"account":{"has_claude_max":true,"has_claude_pro":true}}`, "Max"},
		{"max with an unseen tier", `{"organization":{"rate_limit_tier":"default_claude_max_future","organization_type":"claude_max"}}`, "default_claude_max_future"},
		{"pro flag", `{"account":{"has_claude_max":false,"has_claude_pro":true}}`, "Pro"},
		{"pro tier", `{"organization":{"rate_limit_tier":"default_claude_pro"}}`, "Pro"},
		{"pro tier with suffix", `{"organization":{"rate_limit_tier":"default_claude_pro_trial"}}`, "Pro"},
		{"pro org", `{"organization":{"organization_type":"claude_pro"}}`, "Pro"},
		{"raw tier", `{"organization":{"rate_limit_tier":"default_claude_ai"}}`, "default_claude_ai"},
		{"raw org type", `{"organization":{"organization_type":"claude_something"}}`, "claude_something"},
		{"unknown placeholders", `{"organization":{"rate_limit_tier":"unknown","organization_type":"Unknown"}}`, ""},
		{"no tier facts", `{"account":{"uuid":"a"},"organization":{"uuid":"o","name":"Org"}}`, ""},
		{"non-bool flags are not facts", `{"account":{"has_claude_max":"true","has_claude_pro":1}}`, ""},
		{"non-string tier is not a fact", `{"organization":{"rate_limit_tier":20,"organization_type":["claude_max"]}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var profile OAuthProfile
			if errDecode := json.Unmarshal([]byte(tc.body), &profile); errDecode != nil {
				t.Fatalf("decode profile: %v", errDecode)
			}
			if got := profile.PlanTier(); got != tc.want {
				t.Fatalf("PlanTier() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (*OAuthProfile)(nil).PlanTier(); got != "" {
		t.Fatalf("nil PlanTier() = %q, want empty", got)
	}
}

const tierProfileBody = `{
	"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email":"user@example.com","has_claude_max":true,"has_claude_pro":false},
	"organization":{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","name":"Example Org","organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}
}`

func TestExchangeCodeForTokensKeepsPlanTier(t *testing.T) {
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case TokenURL:
			return jsonResponse(req, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,
				"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email_address":"user@example.com"},
				"organization":{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","name":"Example Org"}}`), nil
		case ProfileURL:
			return jsonResponse(req, tierProfileBody), nil
		case RolesURL:
			return jsonResponse(req, `{"roles":[]}`), nil
		}
		t.Fatalf("unexpected OAuth request URL %s", req.URL)
		return nil, nil
	})}}

	bundle, errExchange := auth.ExchangeCodeForTokens(context.Background(), "code", "state", &PKCECodes{CodeVerifier: "verifier"})
	if errExchange != nil {
		t.Fatalf("ExchangeCodeForTokens() error = %v", errExchange)
	}
	if bundle.TokenData.PlanTier != "Max 20x" {
		t.Fatalf("token plan tier = %q, want Max 20x", bundle.TokenData.PlanTier)
	}
	storage := auth.CreateTokenStorage(bundle)
	encoded, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !strings.Contains(string(encoded), `"plan_tier":"Max 20x"`) {
		t.Fatalf("stored credential omits plan_tier: %s", encoded)
	}
}

func TestRefreshTokensKeepsPlanTier(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case RefreshTokenURL:
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","expires_in":3600}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		case ProfileURL:
			return jsonResponse(req, tierProfileBody), nil
		}
		t.Fatalf("unexpected OAuth request URL %s", req.URL)
		return nil, nil
	})}}

	tokenData, errRefresh := auth.RefreshTokens(t.Context(), "placeholder-refresh")
	if errRefresh != nil {
		t.Fatalf("RefreshTokens() error = %v", errRefresh)
	}
	if tokenData.PlanTier != "Max 20x" {
		t.Fatalf("refreshed plan tier = %q, want Max 20x", tokenData.PlanTier)
	}
}

func TestUpdateTokenStoragePreservesPlanTierWhenRefreshOmitsIt(t *testing.T) {
	storage := &ClaudeTokenStorage{PlanTier: "Max 5x"}
	(&ClaudeAuth{}).UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "new-access"})
	if storage.PlanTier != "Max 5x" {
		t.Fatalf("plan tier = %q, want preserved", storage.PlanTier)
	}
	(&ClaudeAuth{}).UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "newer", PlanTier: "Max 20x"})
	if storage.PlanTier != "Max 20x" {
		t.Fatalf("plan tier = %q, want the refreshed tier", storage.PlanTier)
	}
}
