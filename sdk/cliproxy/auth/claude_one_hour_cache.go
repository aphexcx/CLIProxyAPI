package auth

import (
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
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

// IsClaudeSubagentRequest reports whether a request comes from a Claude Code
// subagent: an agent header, a parent session in metadata.user_id, or the
// subagent flag in the first system block. The seat router applies it to the
// request it places and the Claude executor's beta code to the request it
// sends; the executor's body check, isClaudeSubagentRequestDoc, is the same
// test on its parsed body, so the late subagent hour and its hold agree about
// which conversations are subagents.
func IsClaudeSubagentRequest(headers http.Header, body []byte) bool {
	if headerValueFromHTTPHeader(headers, "X-Claude-Code-Agent-Id") != "" ||
		headerValueFromHTTPHeader(headers, "X-Claude-Code-Parent-Agent-Id") != "" {
		return true
	}
	if ojson.GetBytesNoCopy(body, "metadata.user_id.parent_session_id").Exists() {
		return true
	}
	if userID := ojson.GetBytesNoCopy(body, "metadata.user_id").String(); userID != "" {
		if strings.Contains(userID, `"parent_session_id"`) {
			return true
		}
	}
	// Only system[0].text (or a string system) carries the flag, never message content.
	system := ojson.GetBytesNoCopy(body, "system")
	if system.IsArray() && len(system.Array()) > 0 {
		return strings.Contains(system.Array()[0].Get("text").String(), "cc_is_subagent=true")
	}
	return system.Type == ojson.String && strings.Contains(system.String(), "cc_is_subagent=true")
}

// ClaudeOneHourCacheEnabled reads claude-header-defaults.one-hour-cache, which
// defaults to true.
func ClaudeOneHourCacheEnabled(cfg *internalconfig.Config) bool {
	return cfg == nil || cfg.ClaudeHeaderDefaults.OneHourCache == nil || *cfg.ClaudeHeaderDefaults.OneHourCache
}

// ClaudeLateSubagentHourEnabled reads claude-header-defaults.late-subagent-hour,
// which defaults to false.
func ClaudeLateSubagentHourEnabled(cfg *internalconfig.Config) bool {
	return cfg != nil && cfg.ClaudeHeaderDefaults.LateSubagentHour
}

// copyClaudeSeatMetadata carries the seat router's client hash, move tag and
// late subagent hour from the pick options into the execution options, so the
// executor's cache measure names the client the routing log names, files a
// moved reply under its move, and sends a late subagent at the hour it is held
// for. A move tag or hour the pick did not set is removed: it belongs to an
// earlier choice.
func copyClaudeSeatMetadata(dst, src map[string]any) {
	if clientHash, ok := src[cliproxyexecutor.ClaudeSeatClientHashMetadataKey]; ok {
		dst[cliproxyexecutor.ClaudeSeatClientHashMetadataKey] = clientHash
	}
	for _, key := range []string{cliproxyexecutor.ClaudeSeatMoveMetadataKey, cliproxyexecutor.ClaudeLateSubagentHourMetadataKey} {
		if value, ok := src[key]; ok {
			dst[key] = value
		} else {
			delete(dst, key)
		}
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

// claudeLateSubagentHourEnabled reads the late subagent key the same way, so a
// reload turns the rule on or off without a restart.
func (m *Manager) claudeLateSubagentHourEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return ClaudeLateSubagentHourEnabled(cfg)
}
