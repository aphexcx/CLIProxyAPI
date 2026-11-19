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
	"io"
	"net/http"
	"regexp"
	"slices"
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

// ValidGrantID reports whether id is a grant id a claim can name.
func ValidGrantID(id string) bool { return grantPattern.MatchString(id) }

// ValidRequestID reports whether id is a request id a claim can carry.
func ValidRequestID(id string) bool { return requestPattern.MatchString(id) }

// Claim names the one grant a deliberate operator spend claims.
type Claim struct {
	OrganizationUUID string
	GrantID          string
	RequestID        string
}

// claimMarkerKey is unexported, so no other package can mark a request.
type claimMarkerKey struct{}

// claimMarker binds the exception to the exact request built here: its URL,
// its body and every header, the credential included. It opens the guard for
// one transport send only.
type claimMarker struct {
	url    string
	body   []byte
	header http.Header
	sent   atomic.Bool
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
	marker.body, marker.header = body, req.Header.Clone()
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

// markerOf returns the marker of a claim NewMarkedClaimRequest built whose
// method, URL, host and headers are still the ones it minted.
func markerOf(req *http.Request) *claimMarker {
	if req == nil || req.URL == nil {
		return nil
	}
	marker, _ := req.Context().Value(claimMarkerKey{}).(*claimMarker)
	if marker == nil || req.Method != http.MethodPost || req.URL.String() != marker.url || (req.Host != "" && req.Host != "api.anthropic.com") {
		return nil
	}
	if len(req.Header) != len(marker.header) {
		return nil
	}
	for key, values := range marker.header {
		if !slices.Equal(req.Header[key], values) {
			return nil
		}
	}
	return marker
}

// ClaimPermitted reports whether req is an unsent claim NewMarkedClaimRequest
// built, with its minted headers. It is the preflight; only PermittedClaim,
// at the transport, sends one.
func ClaimPermitted(req *http.Request) bool {
	marker := markerOf(req)
	return marker != nil && !marker.sent.Load()
}

// PermittedClaim spends the marker of an unsent claim and returns the request
// a transport sends in its place: req with the minted body and headers, so a
// body changed after minting is never sent. A redirect hop, a replay, a
// request reusing the context, or a claim whose headers changed is refused.
func PermittedClaim(req *http.Request) (*http.Request, bool) {
	marker := markerOf(req)
	if marker == nil || !marker.sent.CompareAndSwap(false, true) {
		return nil, false
	}
	claim := req.Clone(req.Context())
	claim.Header = marker.header.Clone()
	claim.Body = io.NopCloser(bytes.NewReader(marker.body))
	claim.ContentLength = int64(len(marker.body))
	claim.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(marker.body)), nil }
	return claim, true
}
