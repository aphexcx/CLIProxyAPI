package management

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type anthropicRedirectTransport struct {
	upstream *url.URL
	base     http.RoundTripper
}

func (tr anthropicRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = tr.upstream.Scheme
	clone.URL.Host = tr.upstream.Host
	return tr.base.RoundTrip(clone)
}

func TestAnthropicRedirectExchange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bodies := make(chan map[string]string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/oauth/token":
			var body map[string]string
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode token body: %v", errDecode)
				http.Error(w, "invalid token body", http.StatusBadRequest)
				return
			}
			bodies <- body
			_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","expires_in":28800}`)
		case "/api/oauth/profile":
			_, _ = io.WriteString(w, `{"account":{"uuid":"account-redirect","email":"redirect@example.test"}}`)
		case "/api/oauth/claude_cli/roles":
			_, _ = io.WriteString(w, `{"roles":[]}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	upstreamURL, errParse := url.Parse(upstream.URL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	client := &http.Client{Transport: anthropicRedirectTransport{upstream: upstreamURL, base: upstream.Client().Transport}}
	originalFactory := newClaudeOAuthService
	newClaudeOAuthService = func(cfg *config.Config, proxyURL string, options ...claude.AuthOption) *claude.ClaudeAuth {
		return claude.NewClaudeAuthWithProxyURL(cfg, proxyURL, append(options, claude.WithHTTPClient(client))...)
	}
	defer func() { newClaudeOAuthService = originalFactory }()

	for _, entrypoint := range []string{"legacy", "v8-get", "v8-post", "v8-anthropic"} {
		t.Run(entrypoint, func(t *testing.T) {
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
			persisted := make(chan struct{}, 1)
			h.SetPostAuthPersistHook(func(_ context.Context, _ *coreauth.Auth) error {
				persisted <- struct{}{}
				return nil
			})
			router := gin.New()
			router.GET("/anthropic-auth-url", h.RequestAnthropicToken)
			router.GET("/oauth/auth-url", h.StartOAuthV8)
			router.POST("/oauth/auth-url", h.StartOAuthV8)
			router.POST("/oauth-callback", h.PostOAuthCallback)

			type attempt struct {
				state, challenge, redirect string
			}
			var attempts []attempt
			for _, redirect := range []string{"code", "", "localhost"} {
				method, path, body := http.MethodGet, "/anthropic-auth-url", ""
				if entrypoint != "legacy" {
					path = "/oauth/auth-url?provider=claude"
				}
				if entrypoint == "v8-post" || entrypoint == "v8-anthropic" {
					method, path = http.MethodPost, "/oauth/auth-url"
					provider := "claude"
					if entrypoint == "v8-anthropic" {
						provider = "anthropic"
					}
					payload := map[string]string{"provider": provider}
					if redirect != "" {
						payload["redirect"] = redirect
					}
					raw, errMarshal := json.Marshal(payload)
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					body = string(raw)
				} else if redirect != "" {
					separator := "?"
					if strings.Contains(path, "?") {
						separator = "&"
					}
					path += separator + "redirect=" + redirect
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("start %s: %d %s", redirect, rec.Code, rec.Body)
				}
				var response struct{ URL, State, Redirect string }
				if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
					t.Fatal(errDecode)
				}
				t.Cleanup(func() { CompleteOAuthSession(response.State) })
				parsed, errURL := url.Parse(response.URL)
				if errURL != nil {
					t.Fatal(errURL)
				}
				wantRedirect, wantMode := claude.RedirectURI, "localhost"
				if redirect == "code" {
					wantRedirect, wantMode = claude.CodePageRedirectURI, "code"
				}
				if parsed.Query().Get("redirect_uri") != wantRedirect || response.Redirect != wantMode {
					t.Fatalf("redirect URL/mode = %q/%q, want %q/%q", parsed.Query().Get("redirect_uri"), response.Redirect, wantRedirect, wantMode)
				}
				if response.State == "" || parsed.Query().Get("state") != response.State {
					t.Fatal("missing or inconsistent state")
				}
				attempts = append(attempts, attempt{response.State, parsed.Query().Get("code_challenge"), wantRedirect})
			}

			// Complete mixed redirect sessions in reverse order to catch shared state.
			for i := len(attempts) - 1; i >= 0; i-- {
				a := attempts[i]
				payload, errMarshal := json.Marshal(map[string]string{"provider": "anthropic", "code": "CODE#" + a.state, "state": a.state})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/oauth-callback", strings.NewReader(string(payload))))
				if rec.Code != http.StatusOK {
					t.Fatalf("callback: %d %s", rec.Code, rec.Body)
				}
				select {
				case body := <-bodies:
					if body["redirect_uri"] != a.redirect || body["state"] != a.state || body["code"] != "CODE" || body["client_id"] != claude.ClientID || body["grant_type"] != "authorization_code" {
						t.Fatalf("unexpected exchange body: %v", body)
					}
					digest := sha256.Sum256([]byte(body["code_verifier"]))
					if base64.RawURLEncoding.EncodeToString(digest[:]) != a.challenge {
						t.Fatal("exchange verifier does not match the authorization challenge")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("token exchange did not arrive")
				}
				select {
				case <-persisted:
				case <-time.After(5 * time.Second):
					t.Fatal("exchanged credential was not persisted")
				}
			}
		})
	}
}

func TestAnthropicRedirectValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	router := gin.New()
	router.GET("/anthropic-auth-url", h.RequestAnthropicToken)
	router.POST("/oauth/auth-url", h.StartOAuthV8)
	for _, tc := range []struct{ path, body, wantError string }{
		{"/anthropic-auth-url?redirect=bogus", "", "redirect must be code or localhost"},
		{"/anthropic-auth-url?redirect=code&is_webui=true", "", "redirect=code cannot be combined with is_webui"},
		{"/oauth/auth-url", `{"provider":"claude","redirect":"bogus"}`, "redirect must be code or localhost"},
		{"/oauth/auth-url", `{"provider":"anthropic","redirect":"code","is_webui":true}`, "redirect=code cannot be combined with is_webui"},
		{"/oauth/auth-url", `{"provider":"claude","redirect":true}`, "redirect must be code or localhost"},
		{"/oauth/auth-url", `{"provider":"claude","redirect":null}`, "redirect must be code or localhost"},
	} {
		method := http.MethodGet
		if tc.body != "" {
			method = http.MethodPost
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, tc.path, strings.NewReader(tc.body)))
		var body map[string]string
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil {
			t.Fatal(errDecode)
		}
		if rec.Code != http.StatusBadRequest || body["error"] != tc.wantError {
			t.Fatalf("%s %s: %d %s, want 400 %q", tc.path, tc.body, rec.Code, rec.Body, tc.wantError)
		}
	}
	// Providers other than Claude ignore the redirect key, including its type.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/oauth/auth-url", strings.NewReader(`{"provider":"codex","redirect":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("codex start: %d %s", rec.Code, rec.Body)
	}
	var response struct{ State string }
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	CompleteOAuthSession(response.State)
}
