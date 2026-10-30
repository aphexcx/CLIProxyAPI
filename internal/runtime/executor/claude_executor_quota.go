package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// PollQuota reads OAuth usage without rotating the refresh token or submitting
// inference traffic. Per-credential proxy settings follow the auth control plane.
func (e *ClaudeExecutor) PollQuota(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, error) {
	windows, _, errPoll := e.PollQuotaUsage(ctx, auth)
	return windows, errPoll
}

// PollQuotaUsage shares the same read with cache consumers; no second request is made.
func (e *ClaudeExecutor) PollQuotaUsage(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, json.RawMessage, error) {
	if auth == nil {
		return nil, nil, fmt.Errorf("Claude quota: auth is nil")
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		accessToken, _ = auth.Metadata["accessToken"].(string)
	}
	windows, usage, errFetch := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL).FetchOAuthUsageSnapshot(ctx, accessToken)
	if errFetch != nil {
		return nil, nil, errFetch
	}
	result := make(map[string]cliproxyauth.QuotaWindow, len(windows))
	for name, window := range windows {
		result[name] = cliproxyauth.QuotaWindow{Utilization: window.Utilization, ResetsAt: window.ResetsAt, ObservedAt: window.ObservedAt}
	}
	return result, usage, nil
}
