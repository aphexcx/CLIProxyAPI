// Package resetclaim mints the one request the hub's banked-reset deny rule
// (sdk/httptransport, cp-bha) lets out: a claim built by the management spend
// route (cp-g33). It lives under internal/ so code outside this module cannot
// mint one; TestBankedResetClaimHasOneCallerChain bounds the callers inside it.
package resetclaim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync/atomic"
)

// ErrMalformed refuses a claim whose identifiers do not match the provider
// client's own patterns. Nothing is built or sent.
var ErrMalformed = errors.New("banked-reset claim refused: malformed organization, grant or request id")

var (
	organizationPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	grantPattern        = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	requestPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Claim names the one grant a deliberate operator spend claims.
type Claim struct {
	OrganizationUUID string
	GrantID          string
	RequestID        string
}

// claimMarkerKey is unexported, so no other package can mark a request.
type claimMarkerKey struct{}

// claimMarker binds the exception to the exact request built here. It opens
// the guard for one transport send only.
type claimMarker struct {
	url  string
	sent atomic.Bool
}

// NewMarkedClaimRequest builds the claim from validated identifiers: a fixed
// URL and body, the caller's headers except Host. Send it with
// httptransport.Do through NoRedirectClaimClient.
func NewMarkedClaimRequest(ctx context.Context, claim Claim, header http.Header) (*http.Request, error) {
	if !organizationPattern.MatchString(claim.OrganizationUUID) || !grantPattern.MatchString(claim.GrantID) || !requestPattern.MatchString(claim.RequestID) {
		return nil, ErrMalformed
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
	marker := &claimMarker{url: endpoint}
	req, errRequest := http.NewRequestWithContext(context.WithValue(ctx, claimMarkerKey{}, marker), http.MethodPost, endpoint, bytes.NewReader(body))
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
	return req, nil
}

// NoRedirectClaimClient copies client so a claim follows no redirect and
// carries no cookie jar. The marker refuses a second hop in any case.
func NoRedirectClaimClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	once := *client
	once.Jar = nil
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &once
}

// ClaimPermitted reports whether req is a claim NewMarkedClaimRequest built
// and no transport has sent yet. consume spends the marker, so a redirect hop,
// a replay or a request reusing the context is refused.
func ClaimPermitted(req *http.Request, consume bool) bool {
	if req == nil || req.URL == nil {
		return false
	}
	marker, _ := req.Context().Value(claimMarkerKey{}).(*claimMarker)
	if marker == nil || req.Method != http.MethodPost || req.URL.String() != marker.url || (req.Host != "" && req.Host != "api.anthropic.com") {
		return false
	}
	if consume {
		return marker.sent.CompareAndSwap(false, true)
	}
	return !marker.sent.Load()
}
