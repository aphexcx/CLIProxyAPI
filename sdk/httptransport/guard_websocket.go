package httptransport

import (
	"context"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

// DialWebSocket applies the same URL boundary before the authenticated upgrade.
// Gorilla does not follow redirects; any caller retry passes this check again.
func DialWebSocket(ctx context.Context, dialer *websocket.Dialer, rawURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if deniedRequest(&http.Request{URL: u, Host: headers.Get("Host")}) {
		return nil, nil, ErrResetRateLimitsDenied
	}
	return dialer.DialContext(ctx, rawURL, headers)
}
