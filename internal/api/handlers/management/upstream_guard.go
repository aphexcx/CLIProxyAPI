package management

import (
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Refuse ambiguous claim paths before the underlying transport sees them. The
// same transport checks the actual request on the initial send and every redirect.
// Containment deliberately rejects extensions, parameters and trailing controls.
func deniesResetRateLimits(u *url.URL) bool {
	if u == nil {
		return false
	}
	path := u.EscapedPath()
	for round := 0; round < 3; round++ {
		decoded, err := url.PathUnescape(path)
		if err != nil {
			return true
		}
		if decoded == path {
			break
		}
		path = decoded
	}
	// More encoding than the bounded decoder accepts is ambiguous; fail closed.
	if strings.Contains(path, "%") {
		return true
	}
	path = strings.ToLower(norm.NFKC.String(strings.ReplaceAll(path, "\\", "/")))
	return strings.Contains(path, "reset_rate_limits")
}

type managementTransport struct{ base http.RoundTripper }

func (t *managementTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if deniesResetRateLimits(req.URL) {
		return nil, errResetRateLimitsDenied
	}
	return t.base.RoundTrip(req)
}

func guardManagementTransport(base http.RoundTripper) http.RoundTripper {
	if transport, ok := base.(*http.Transport); ok && transport == nil {
		base = nil
	}
	if _, guarded := base.(*managementTransport); guarded {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &managementTransport{base: base}
}

func newManagementHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: guardManagementTransport(transport)}
}
