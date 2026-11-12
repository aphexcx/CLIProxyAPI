package management

import (
	"net/http"
	"net/url"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
)

func deniesResetRateLimits(u *url.URL) bool {
	return httptransport.DeniesResetRateLimits(u)
}

func guardManagementTransport(base http.RoundTripper) http.RoundTripper {
	return httptransport.GuardTransport(base)
}

func newManagementHTTPClient(transport http.RoundTripper) *http.Client {
	return httptransport.GuardClient(&http.Client{Transport: transport})
}
