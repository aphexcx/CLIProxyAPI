package pluginhost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBankedResetPluginBridge(t *testing.T) {
	var claims atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/api/organizations/fixture/reset_rate_limits", 307)
			return
		}
		claims.Add(1)
	}))
	defer server.Close()
	client := New().newHTTPClient(nil)
	for _, path := range []string{"/api/organizations/fixture/reset_rate_limits", "/redirect"} {
		for _, profile := range []*pluginapi.HTTPWireProfile{nil, {HTTP1Only: true}} {
			req := pluginapi.HTTPRequest{Method: "POST", URL: server.URL + path, Headers: http.Header{"Authorization": {"Bearer fixture-only"}}, Body: []byte(`{"program":"cedar_ember"}`), WireProfile: profile}
			if _, err := client.Do(context.Background(), req); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
				t.Fatalf("buffered: %v", err)
			}
			if _, err := client.DoStream(context.Background(), req); !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
				t.Fatalf("stream: %v", err)
			}
		}
	}
	if claims.Load() != 0 {
		t.Fatalf("claim reached plugin upstream %d times", claims.Load())
	}
}
