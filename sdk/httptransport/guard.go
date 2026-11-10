package httptransport

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/text/unicode/norm"
)

type resetDeniedError struct{}

func (resetDeniedError) Error() string {
	return "banked-reset deny rule (cp-bha): reset_rate_limits or ambiguous Anthropic paths are forbidden"
}
func (resetDeniedError) StatusCode() int { return http.StatusForbidden }

// ErrResetRateLimitsDenied identifies the process-wide banked-reset boundary.
var ErrResetRateLimitsDenied error = resetDeniedError{}

// DeniesResetRateLimits examines the actual request URL, independent of method,
// provider or token type. Anthropic paths additionally reject non-ASCII/control
// bytes rather than guessing how a server normalizes confusables or invisibles.
func DeniesResetRateLimits(u *url.URL) bool {
	if u == nil {
		return false
	}
	if u.Opaque != "" {
		return true
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
	if strings.Contains(path, "%") {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "anthropic.com" || strings.HasSuffix(host, ".anthropic.com") || host == "claude.ai" || strings.HasSuffix(host, ".claude.ai") {
		for _, r := range path {
			if r < 0x20 || r >= 0x7f {
				return true
			}
		}
	}
	path = strings.ToLower(norm.NFKC.String(strings.ReplaceAll(path, "\\", "/")))
	return strings.Contains(path, "reset_rate_limits")
}

type guardedTransport struct{ base http.RoundTripper }

func (t *guardedTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func deniedRequest(req *http.Request) bool {
	if DeniesResetRateLimits(req.URL) {
		return true
	}
	if req.Host != "" && req.URL != nil {
		virtual := *req.URL
		virtual.Host = req.Host
		return DeniesResetRateLimits(&virtual)
	}
	return false
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if deniedRequest(req) {
		return nil, ErrResetRateLimitsDenied
	}
	return t.base.RoundTrip(req)
}

// GuardTransport preserves the selected proxy, TLS fingerprint and connection
// pool. net/http calls it for the initial request and every redirect hop.
func GuardTransport(base http.RoundTripper) http.RoundTripper {
	if transport, ok := base.(*http.Transport); ok && transport == nil {
		base = nil
	}
	if _, guarded := base.(*guardedTransport); guarded {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &guardedTransport{base: base}
}

// GuardClient copies only the client value. Applying the guard at the send
// boundary includes injected clients and transports modified after construction.
func GuardClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.Transport = GuardTransport(client.Transport)
	return &clone
}

// Doer includes the read-only cache adapters and test doubles used by the hub.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Do is the final HTTP send boundary for all hub packages. Real HTTP clients
// receive the same guarded transport, including every redirect. Adapter calls
// are checked here too; their own outbound sends must use this boundary.
func Do(client Doer, req *http.Request) (*http.Response, error) {
	if deniedRequest(req) {
		return nil, ErrResetRateLimitsDenied
	}
	if client == nil {
		client = GuardClient(nil)
	} else if concrete, ok := client.(*http.Client); ok {
		client = GuardClient(concrete)
	}
	response, err := client.Do(req)
	if errors.Is(err, ErrResetRateLimitsDenied) {
		// net/http wraps redirect failures with the original URL, whose query
		// may contain credentials. Expose only the stable refusal and status.
		return nil, ErrResetRateLimitsDenied
	}
	return response, err
}
