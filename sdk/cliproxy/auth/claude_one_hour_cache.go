package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// IsClaudeSubscriptionCredential reports whether a Claude credential is a
// subscription (OAuth) access token rather than an API key. It is the one
// "subscription seat" test: the Claude executor applies it to the credential it
// sends, and the seat router to the same credential through
// IsClaudeSubscriptionSeat, so the one-hour cache rule and the seat hold cannot
// disagree about which seats the rule covers.
func IsClaudeSubscriptionCredential(credential string) bool {
	return strings.Contains(credential, "sk-ant-oat")
}

// IsClaudeSubscriptionSeat applies IsClaudeSubscriptionCredential to the
// credential a Claude seat sends upstream, resolved in the executor's order:
// the api_key attribute, otherwise the OAuth access token.
func IsClaudeSubscriptionSeat(a *Auth) bool {
	if a == nil {
		return false
	}
	credential := ""
	if a.Attributes != nil {
		credential = a.Attributes["api_key"]
	}
	if credential == "" {
		credential = authMetadataString(a, "access_token")
	}
	return IsClaudeSubscriptionCredential(credential)
}

// ClaudeOneHourCacheEnabled reads claude-header-defaults.one-hour-cache, which
// defaults to true.
func ClaudeOneHourCacheEnabled(cfg *internalconfig.Config) bool {
	return cfg == nil || cfg.ClaudeHeaderDefaults.OneHourCache == nil || *cfg.ClaudeHeaderDefaults.OneHourCache
}

// copyClaudeSeatClientHash carries the seat router's client hash from the pick
// options into the execution options, so the executor's cache measure names the
// client the routing log names.
func copyClaudeSeatClientHash(dst, src map[string]any) {
	if clientHash, ok := src[cliproxyexecutor.ClaudeSeatClientHashMetadataKey]; ok {
		dst[cliproxyexecutor.ClaudeSeatClientHashMetadataKey] = clientHash
	}
}

// claudeOneHourCacheEnabled reads the key from the manager's runtime config,
// which a config reload replaces, so the seat hold follows the key without a
// restart.
func (m *Manager) claudeOneHourCacheEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return ClaudeOneHourCacheEnabled(cfg)
}
