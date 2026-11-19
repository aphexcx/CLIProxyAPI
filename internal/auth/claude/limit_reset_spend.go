package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/resetclaim"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	log "github.com/sirupsen/logrus"
)

// Results of one banked-reset claim. The first six are the provider's own
// answers; the rest are the hub's reading of a status or a missing answer.
const (
	LimitResetResultReset       = "reset"
	LimitResetResultAlreadyUsed = "already_used"
	LimitResetResultNotLimited  = "not_limited"
	LimitResetResultCooldown    = "cooldown"
	LimitResetResultIneligible  = "ineligible"
	LimitResetResultUnavailable = "unavailable"
	LimitResetResultRateLimited = "rate_limited"
	LimitResetResultAuthError   = "auth_error"
	// LimitResetResultUnknown means the claim may have been spent: no terminal
	// answer came back. Only a retry with the same request id may follow.
	LimitResetResultUnknown = "unknown"
)

// LimitResetUserAgent is the client name a spend's reads and claim carry: the
// Claude Code shape the hub's earlier dashboard used, the only one observed to
// list grants and claim one through the hub.
func LimitResetUserAgent(version string) string {
	return "claude-cli/" + version + " (external, cli)"
}

// LimitResetClaimResult is the outcome of one claim. Errors never carry the
// upstream body or the credential.
type LimitResetClaimResult struct {
	HTTPStatus     int      `json:"http_status,omitempty"`
	Result         string   `json:"result"`
	Reason         string   `json:"reason,omitempty"`
	ResetsLeft     *int     `json:"resets_left,omitempty"`
	Cleared        []string `json:"cleared,omitempty"`
	WeeklyResetsAt *string  `json:"weekly_resets_at,omitempty"`
	CooldownUntil  *string  `json:"cooldown_until,omitempty"`
}

func applyLimitResetSpendHeaders(header http.Header, accessToken, userAgent string) {
	header.Set("Accept", "application/json, text/plain, */*")
	header.Set("Authorization", "Bearer "+accessToken)
	header.Set("Content-Type", "application/json")
	header.Set("Anthropic-Beta", "oauth-2025-04-20")
	header.Set("User-Agent", userAgent)
	// Each spend builds its own client: close every connection so no socket
	// outlives the spend.
	header.Set("Connection", "close")
}

func (o *ClaudeAuth) fetchLimitResetSpendJSON(ctx context.Context, endpoint, accessToken, userAgent, label string) ([]byte, error) {
	if o == nil || o.httpClient == nil {
		return nil, fmt.Errorf("fetch Claude %s: HTTP client is nil", label)
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("fetch Claude %s: access token is empty", label)
	}
	ctx = context.WithValue(ctx, claudeUsageConnectTimeoutContextKey{}, claudeUsageConnectTimeout)
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create Claude %s request: %w", label, errRequest)
	}
	applyLimitResetSpendHeaders(req.Header, accessToken, userAgent)
	req.Close = true
	resp, errDo := httptransport.Do(o.httpClient, req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch Claude %s: %w", label, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close Claude %s response body: %v", label, errClose)
		}
	}()
	body, errRead := readClaudeOAuthResponseBody(resp)
	if errRead != nil {
		return nil, &controlPlaneHTTPError{resp.StatusCode, fmt.Errorf("read Claude %s response: %w", label, errRead)}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &controlPlaneHTTPError{resp.StatusCode, fmt.Errorf("fetch Claude %s failed with status %d", label, resp.StatusCode)}
	}
	return body, nil
}

// FetchLimitResetSpendProfile reads the seat's organization for a spend.
func (o *ClaudeAuth) FetchLimitResetSpendProfile(ctx context.Context, accessToken, userAgent string) (*OAuthProfile, error) {
	body, errFetch := o.fetchLimitResetSpendJSON(ctx, ProfileURL, accessToken, userAgent, "profile")
	if errFetch != nil {
		return nil, errFetch
	}
	var profile OAuthProfile
	if errUnmarshal := json.Unmarshal(body, &profile); errUnmarshal != nil {
		return nil, fmt.Errorf("parse Claude profile response: %w", errUnmarshal)
	}
	if strings.TrimSpace(profile.Organization.UUID) == "" {
		return nil, errors.New("fetch Claude profile: response organization UUID is empty")
	}
	return &profile, nil
}

// FetchLimitResetSpendStatus reads the seat's banked reset grants fresh,
// never from a cache. The body is returned raw for the shared parser.
func (o *ClaudeAuth) FetchLimitResetSpendStatus(ctx context.Context, accessToken, userAgent string) (json.RawMessage, error) {
	body, errFetch := o.fetchLimitResetSpendJSON(ctx, LimitResetsUsageURL, accessToken, userAgent, "reset status")
	if errFetch != nil {
		return nil, errFetch
	}
	return json.RawMessage(body), nil
}

// ClaimLimitReset spends one reset of the named grant. It returns an error
// only when the claim was certainly not sent; any send without a terminal
// answer is LimitResetResultUnknown. It never retries.
func (o *ClaudeAuth) ClaimLimitReset(ctx context.Context, accessToken, userAgent string, claim resetclaim.Claim) (LimitResetClaimResult, error) {
	if o == nil || o.httpClient == nil {
		return LimitResetClaimResult{}, errors.New("claim Claude reset: HTTP client is nil")
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return LimitResetClaimResult{}, errors.New("claim Claude reset: access token is empty")
	}
	header := http.Header{}
	applyLimitResetSpendHeaders(header, accessToken, userAgent)
	ctx = context.WithValue(ctx, claudeUsageConnectTimeoutContextKey{}, claudeUsageConnectTimeout)
	req, errRequest := resetclaim.NewMarkedClaimRequest(ctx, claim, header)
	if errRequest != nil {
		return LimitResetClaimResult{}, errRequest
	}
	req.Close = true
	resp, errSend := httptransport.Do(resetclaim.NoRedirectClaimClient(o.httpClient), req)
	if errSend != nil {
		if errors.Is(errSend, httptransport.ErrResetRateLimitsDenied) {
			return LimitResetClaimResult{}, errSend
		}
		// The transport error names the claim URL, which carries the
		// organization id; log only whether the caller went away.
		log.WithField("canceled", ctx.Err() != nil).Warn("Claude reset claim got no answer")
		return LimitResetClaimResult{Result: LimitResetResultUnknown}, nil
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close Claude reset claim response body: %v", errClose)
		}
	}()
	result := LimitResetClaimResult{HTTPStatus: resp.StatusCode, Result: LimitResetResultUnknown}
	body, errRead := readClaudeOAuthResponseBody(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Result = LimitResetResultRateLimited
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		result.Result = LimitResetResultAuthError
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && errRead == nil:
		var answer struct {
			Result         string   `json:"result"`
			Reason         *string  `json:"reason"`
			ResetsLeft     *int     `json:"resets_left"`
			Cleared        []string `json:"cleared"`
			WeeklyResetsAt *string  `json:"weekly_resets_at"`
			CooldownUntil  *string  `json:"cooldown_until"`
		}
		if json.Unmarshal(body, &answer) != nil {
			return result, nil
		}
		switch answer.Result {
		case LimitResetResultReset, LimitResetResultAlreadyUsed, LimitResetResultNotLimited, LimitResetResultCooldown, LimitResetResultIneligible, LimitResetResultUnavailable:
			result.Result = answer.Result
		default:
			// An answer the hub cannot read may still have spent the reset.
			return result, nil
		}
		if answer.Reason != nil {
			result.Reason = *answer.Reason
		}
		result.ResetsLeft, result.Cleared = answer.ResetsLeft, answer.Cleared
		result.WeeklyResetsAt, result.CooldownUntil = answer.WeeklyResetsAt, answer.CooldownUntil
	}
	return result, nil
}
