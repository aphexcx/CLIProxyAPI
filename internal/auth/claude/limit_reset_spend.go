package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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

// limitResetReasonCodePattern is the only shape of provider reason the hub
// keeps: a code of lowercase letters and underscores. Free text, which could
// carry an email, an organization id or a credential, is never kept.
var limitResetReasonCodePattern = regexp.MustCompile(`^[a-z][a-z_]{0,47}$`)

// LimitResetReasonUnrecognized replaces a provider reason that is not a code.
const LimitResetReasonUnrecognized = "unrecognized"

// ValidLimitResetReasonCode reports whether reason is a code the hub keeps.
func ValidLimitResetReasonCode(reason string) bool {
	return limitResetReasonCodePattern.MatchString(reason)
}

// limitResetReasonCode keeps a provider reason only when it is a code.
func limitResetReasonCode(reason string) string {
	if reason == "" || ValidLimitResetReasonCode(reason) {
		return reason
	}
	return LimitResetReasonUnrecognized
}

// LimitResetUserAgent is the client name every reset read and claim carries:
// the Claude Code shape the hub's earlier dashboard used, the only one
// observed to list grants and claim one through the hub. The same read with
// any other client name (axios, or claude-code/<version>) answers eligible
// false with ineligible_reason "surface" and no grants.
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
	// The established control-plane reader's freshness rule: never a cached copy.
	req.Header.Set("Cache-Control", "no-cache")
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

// FetchLimitResetStatus reads the seat's banked reset grants fresh, never
// from a cache, for both the background poller and a spend. A response with
// no valid usage window is refused. The body is returned raw for the shared
// parser and is never authoritative for usage windows.
func (o *ClaudeAuth) FetchLimitResetStatus(ctx context.Context, accessToken, userAgent string) (json.RawMessage, error) {
	body, errFetch := o.fetchLimitResetSpendJSON(ctx, LimitResetsUsageURL, accessToken, userAgent, "reset status")
	if errFetch != nil {
		return nil, errFetch
	}
	windows, errParse := parseOAuthUsage(body)
	if errParse != nil || len(windows) == 0 {
		return nil, errors.New("reset status response has no valid usage windows")
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
			result.Reason = limitResetReasonCode(*answer.Reason)
		}
		result.ResetsLeft, result.Cleared = answer.ResetsLeft, answer.Cleared
		result.WeeklyResetsAt, result.CooldownUntil = answer.WeeklyResetsAt, answer.CooldownUntil
	}
	return result, nil
}
