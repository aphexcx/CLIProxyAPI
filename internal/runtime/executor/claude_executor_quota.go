package executor

import (
	"context"
	"fmt"
	"strings"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// PollQuota reads OAuth usage without rotating the refresh token or submitting
// inference traffic. Per-credential proxy settings follow the auth control plane.
func (e *ClaudeExecutor) PollQuota(ctx context.Context, auth *cliproxyauth.Auth) (map[string]cliproxyauth.QuotaWindow, error) {
	if auth == nil {
		return nil, fmt.Errorf("Claude quota: auth is nil")
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		accessToken, _ = auth.Metadata["accessToken"].(string)
	}
	windows, errFetch := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL).FetchOAuthUsage(ctx, accessToken)
	if errFetch != nil {
		return nil, errFetch
	}
	result := make(map[string]cliproxyauth.QuotaWindow, len(windows))
	for name, window := range windows {
		result[name] = cliproxyauth.QuotaWindow{Utilization: window.Utilization, ResetsAt: window.ResetsAt, ObservedAt: window.ObservedAt}
	}
	return result, nil
}
