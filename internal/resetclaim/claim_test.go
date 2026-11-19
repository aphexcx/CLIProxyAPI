package resetclaim_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/resetclaim"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
)

const (
	fixtureOrganization = "00000000-0000-4000-8000-0000000000aa"
	fixtureClaimURL     = "https://api.anthropic.com/api/organizations/" + fixtureOrganization + "/reset_rate_limits"
)

type claimTestTransport func(*http.Request) (*http.Response, error)

func (f claimTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClaim() resetclaim.Claim {
	return resetclaim.Claim{OrganizationUUID: fixtureOrganization, GrantID: "fixture-grant", RequestID: "fixture-request"}
}

// send is the shape of the one production caller: a marked request, a client
// that follows no redirect, and the shared guarded send.
func send(t *testing.T, client *http.Client, claim resetclaim.Claim, header http.Header) (*http.Response, error) {
	t.Helper()
	req, err := resetclaim.NewMarkedClaimRequest(context.Background(), claim, header)
	if err != nil {
		return nil, err
	}
	return httptransport.Do(resetclaim.NoRedirectClaimClient(client), req)
}

func TestBankedResetClaimSendsExactlyTheBuiltRequestOnce(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: claimTestTransport(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		body, _ := io.ReadAll(req.Body)
		// The caller's Host header must not move the claim to another host.
		if req.Method != http.MethodPost || req.URL.String() != fixtureClaimURL || req.Host != "api.anthropic.com" || req.Header.Get("Host") != "" {
			t.Fatalf("unexpected claim %s %s host=%q", req.Method, req.URL, req.Host)
		}
		if string(body) != `{"program":"cedar_ember","grant_id":"fixture-grant","request_id":"fixture-request"}` {
			t.Fatalf("unexpected body %s", body)
		}
		if req.Header.Get("Authorization") != "Bearer fixture-only" || req.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected headers %v", req.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"result":"reset"}`)), Request: req}, nil
	})}
	header := http.Header{"Authorization": {"Bearer fixture-only"}, "Host": {"fixture.invalid"}}
	resp, err := send(t, client, fixtureClaim(), header)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	// The response's request context still carries the spent marker: a request
	// built from it is refused like any other reset claim, at both boundaries.
	replay, err := http.NewRequestWithContext(resp.Request.Context(), http.MethodPost, fixtureClaimURL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = httptransport.Do(client, replay); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
		t.Fatalf("replayed marker: got %v", err)
	}
	if _, err = httptransport.GuardTransport(client.Transport).RoundTrip(replay); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
		t.Fatalf("replayed marker at the transport: got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("replay reached the transport: calls=%d", calls.Load())
	}
}

func TestBankedResetClaimDoesNotFollowRedirects(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: claimTestTransport(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		header := http.Header{"Location": {fixtureClaimURL + "?again=1"}}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	resp, err := send(t, client, fixtureClaim(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
	}
	// Even a client that follows redirects cannot carry the marker to a second hop.
	calls.Store(0)
	req, err := resetclaim.NewMarkedClaimRequest(context.Background(), fixtureClaim(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = httptransport.Do(client, req); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) || calls.Load() != 1 {
		t.Fatalf("redirect hop: err=%v calls=%d", err, calls.Load())
	}
}

func TestBankedResetClaimRefusesMalformedInput(t *testing.T) {
	cases := map[string]resetclaim.Claim{
		"organization path": {OrganizationUUID: "../x", GrantID: "g", RequestID: "r"},
		"organization tail": {OrganizationUUID: fixtureOrganization + "/x", GrantID: "g", RequestID: "r"},
		"empty grant":       {OrganizationUUID: fixtureOrganization, GrantID: "", RequestID: "r"},
		"grant slash":       {OrganizationUUID: fixtureOrganization, GrantID: "a/b", RequestID: "r"},
		"grant upper":       {OrganizationUUID: fixtureOrganization, GrantID: "Grant", RequestID: "r"},
		"grant too long":    {OrganizationUUID: fixtureOrganization, GrantID: strings.Repeat("a", 41), RequestID: "r"},
		"empty request":     {OrganizationUUID: fixtureOrganization, GrantID: "g", RequestID: ""},
		"request quote":     {OrganizationUUID: fixtureOrganization, GrantID: "g", RequestID: `r"`},
		"request too long":  {OrganizationUUID: fixtureOrganization, GrantID: "g", RequestID: strings.Repeat("r", 65)},
	}
	for name, claim := range cases {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: claimTestTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("malformed claim left the hub")
				return nil, nil
			})}
			if _, err := send(t, client, claim, nil); !errors.Is(err, resetclaim.ErrMalformed) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// A request that names the claim URL but was not built by
// NewMarkedClaimRequest is refused at both boundaries, whatever client sends it.
func TestBankedResetClaimURLStaysDeniedForOtherRequests(t *testing.T) {
	client := &http.Client{Transport: claimTestTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("hand-built claim left the hub")
		return nil, nil
	})}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut} {
		req, err := http.NewRequest(method, fixtureClaimURL, strings.NewReader(`{"program":"cedar_ember"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = httptransport.Do(client, req); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
			t.Fatalf("%s: got %v", method, err)
		}
		if _, err = httptransport.GuardTransport(client.Transport).RoundTrip(req); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
			t.Fatalf("%s transport: got %v", method, err)
		}
	}
	// A marked request whose method or host was changed after minting is refused.
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Method = http.MethodGet },
		func(r *http.Request) { r.Host = "fixture.invalid" },
		func(r *http.Request) { r.URL.RawQuery = "x=1" },
	} {
		req, err := resetclaim.NewMarkedClaimRequest(context.Background(), fixtureClaim(), nil)
		if err != nil {
			t.Fatal(err)
		}
		mutate(req)
		if _, err = httptransport.Do(client, req); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
			t.Fatalf("mutated marked request: got %v", err)
		}
	}
}
