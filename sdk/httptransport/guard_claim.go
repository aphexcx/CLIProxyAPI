package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync/atomic"
)

// ErrBankedResetClaimMalformed refuses a claim whose identifiers do not match
// the provider client's own patterns. Nothing is sent.
var ErrBankedResetClaimMalformed = errors.New("banked-reset claim refused: malformed organization, grant or request id")

var (
	bankedResetOrganizationPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	bankedResetGrantPattern        = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	bankedResetRequestPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// BankedResetClaim names the one grant a deliberate operator spend claims.
type BankedResetClaim struct {
	OrganizationUUID string
	GrantID          string
	RequestID        string
}

// bankedResetClaimKey is unexported, so no other package can mark a request.
type bankedResetClaimKey struct{}

// bankedResetClaimMarker binds the exception to the exact request built here.
// It opens the guard for one transport send only.
type bankedResetClaimMarker struct {
	url  string
	sent atomic.Bool
}

// SendBankedResetClaim is the single exception to the reset deny rule
// (cp-g33). It builds the claim itself, from validated identifiers, and sends
// it once: no redirect is followed and the marker it attaches is spent by the
// first transport send. The api-call pass-through, plugins, inference and every
// other request stay refused. The only caller is the management spend route;
// TestBankedResetClaimHasOneCallerChain enforces that.
func SendBankedResetClaim(ctx context.Context, client *http.Client, claim BankedResetClaim, header http.Header) (*http.Response, error) {
	if !bankedResetOrganizationPattern.MatchString(claim.OrganizationUUID) || !bankedResetGrantPattern.MatchString(claim.GrantID) || !bankedResetRequestPattern.MatchString(claim.RequestID) {
		return nil, ErrBankedResetClaimMalformed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := "https://api.anthropic.com/api/organizations/" + claim.OrganizationUUID + "/reset_rate_limits"
	body, errMarshal := json.Marshal(struct {
		Program   string `json:"program"`
		GrantID   string `json:"grant_id"`
		RequestID string `json:"request_id"`
	}{"cedar_ember", claim.GrantID, claim.RequestID})
	if errMarshal != nil {
		return nil, errMarshal
	}
	marker := &bankedResetClaimMarker{url: endpoint}
	req, errRequest := http.NewRequestWithContext(context.WithValue(ctx, bankedResetClaimKey{}, marker), http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, errRequest
	}
	for key, values := range header {
		if http.CanonicalHeaderKey(key) == "Host" {
			continue
		}
		req.Header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	once := *client
	once.Jar = nil
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return Do(&once, req)
}

// bankedResetClaimPermitted reports whether req is the claim
// SendBankedResetClaim built and has not yet sent. consume spends the marker,
// so a redirect hop, a replay or a request reusing the context is refused.
func bankedResetClaimPermitted(req *http.Request, consume bool) bool {
	if req == nil || req.URL == nil {
		return false
	}
	marker, _ := req.Context().Value(bankedResetClaimKey{}).(*bankedResetClaimMarker)
	if marker == nil || req.Method != http.MethodPost || req.URL.String() != marker.url || (req.Host != "" && req.Host != "api.anthropic.com") {
		return false
	}
	if consume {
		return marker.sent.CompareAndSwap(false, true)
	}
	return !marker.sent.Load()
}
