package httptransport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type guardTestTransport func(*http.Request) (*http.Response, error)

func (f guardTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGuardChecksActualSendAndEveryRedirect(t *testing.T) {
	paths := []string{"/reset_rate_limits", "/reѕet_rate_limits", "/resеt_rate_limits", "/reset_\u200brate_limits", "/reset_\u00adrate_limits", "/v1/\u0001messages"}
	for _, path := range paths {
		for _, redirect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/redirect=%t", path, redirect), func(t *testing.T) {
				calls := 0
				client := &http.Client{}
				// The transport is deliberately assigned after construction.
				client.Transport = guardTestTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Path != "/redirect" {
						t.Fatal("denied request reached the underlying transport")
					}
					target := (&url.URL{Scheme: "https", Host: "api.anthropic.com", Path: path}).String()
					return &http.Response{StatusCode: 307, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				})
				target := (&url.URL{Scheme: "https", Host: "api.anthropic.com", Path: path}).String()
				if redirect {
					target = "https://fixture.invalid/redirect?key=fixture-query-secret"
				}
				req, err := http.NewRequest("POST", target, strings.NewReader(`{"program":"cedar_ember"}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer fixture-only")
				_, err = Do(client, req)
				if !errors.Is(err, ErrResetRateLimitsDenied) {
					t.Fatalf("got %v", err)
				}
				if strings.Contains(err.Error(), "fixture-query-secret") {
					t.Fatal("redirect refusal exposed the original query")
				}
				want := 0
				if redirect {
					want = 1
				}
				if calls != want {
					t.Fatalf("calls=%d", calls)
				}
			})
		}
	}
}

func TestGuardLegitimatePaths(t *testing.T) {
	for _, raw := range []string{"https://api.anthropic.com/v1/messages", "https://fixture.invalid/gemini%3AgenerateContent", "https://fixture.invalid/caf%C3%A9", "https://api.anthropic.com/v1/messages?next=reset_rate_limits"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if DeniesResetRateLimits(u) {
			t.Errorf("legitimate URL denied: %s", raw)
		}
	}
}

func TestGuardVirtualHostAndOpaquePaths(t *testing.T) {
	for _, req := range []*http.Request{
		{URL: &url.URL{Scheme: "https", Host: "fixture.invalid", Path: "/resеt_rate_limits"}, Host: "api.anthropic.com"},
		{URL: &url.URL{Scheme: "https", Host: "api.anthropic.com", Opaque: "//api.anthropic.com/reset_rate_limits"}},
	} {
		_, err := Do(&http.Client{Transport: guardTestTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("ambiguous request left the hub")
			return nil, nil
		})}, req)
		if !errors.Is(err, ErrResetRateLimitsDenied) {
			t.Fatalf("got %v", err)
		}
	}
}
