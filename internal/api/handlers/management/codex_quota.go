package management

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// codexUsagePayload renders the merged windows in the panel's wham usage shape.
// The plural rate_limits remains the cache's 429 counter; rate_limit is usage.
func codexUsagePayload(snapshot coreauth.ClaudeQuotaSnapshot) gin.H {
	payload := gin.H{}
	if len(snapshot.Usage) > 0 {
		_ = json.Unmarshal(snapshot.Usage, &payload)
	}
	if payload == nil {
		payload = gin.H{}
	}
	payload["status"], payload["stale"] = snapshot.Status, snapshot.Stale
	payload["windows"], payload["polled_at"] = snapshot.Windows, snapshot.PolledAt
	payload["age_seconds"], payload["next_poll_at"] = snapshot.AgeSeconds, snapshot.NextPollAt
	payload["refreshing"], payload["polls"] = snapshot.Refreshing, snapshot.Polls
	payload["rate_limits"], payload["last_status"] = snapshot.RateLimits, snapshot.LastStatus
	payload["last_poll_at"] = snapshot.LastPollAt
	groups := map[string]gin.H{}
	selected := map[string]coreauth.QuotaWindow{}
	windowNames := make([]string, 0, len(snapshot.Windows))
	for name := range snapshot.Windows {
		windowNames = append(windowNames, name)
	}
	// Canonical name order breaks ties between observations with equal timestamps.
	sort.Strings(windowNames)
	for _, name := range windowNames {
		window := snapshot.Windows[name]
		parts := strings.Split(name, "/")
		period := parts[len(parts)-1]
		group := strings.TrimSuffix(name, period)
		seconds, slot := 18000, "primary_window"
		switch period {
		case "seven_day":
			seconds, slot = 604800, "secondary_window"
		case "monthly":
			seconds, slot = 30*24*60*60, "secondary_window"
		case "five_hour":
		default:
			continue
		}
		key := group + slot
		if previous, exists := selected[key]; exists && !window.ObservedAt.After(previous.ObservedAt) {
			continue
		}
		selected[key] = window
		if groups[group] == nil {
			groups[group] = gin.H{}
		}
		groups[group][slot] = gin.H{"used_percent": window.Utilization, "reset_at": window.ResetsAt.Unix(), "limit_window_seconds": seconds}
	}
	payload["rate_limit"], payload["code_review_rate_limit"] = groups[""], groups["code_review/"]
	names := make([]string, 0)
	for group := range groups {
		if strings.HasPrefix(group, "additional/") {
			names = append(names, group)
		}
	}
	sort.Strings(names)
	additional := make([]gin.H, 0, len(names))
	for _, group := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(group, "additional/"), "/")
		additional = append(additional, gin.H{"limit_name": name, "rate_limit": groups[group]})
	}
	payload["additional_rate_limits"] = additional
	return payload
}

func (h *Handler) cachedCodexUsage(auth *coreauth.Auth, refresh bool) (coreauth.ClaudeQuotaSnapshot, bool) {
	if auth == nil || h.authManager == nil {
		return coreauth.ClaudeQuotaSnapshot{}, false
	}
	if refresh {
		h.authManager.RequestCodexQuotaPoll(auth.ID)
	}
	return h.authManager.CodexQuotaCached(auth.ID)
}

func (h *Handler) serveCachedCodexAPICall(c *gin.Context, auth *coreauth.Auth) bool {
	if !c.GetBool(ConfigV8ContextKey) {
		return false
	}
	snapshot, ok := h.cachedCodexUsage(auth, true)
	if !ok {
		return false
	}
	payload := codexUsagePayload(snapshot)
	status := http.StatusOK
	if snapshot.Stale {
		status = http.StatusServiceUnavailable
		payload["error"] = gin.H{"message": "Cached Codex usage is stale or not polled yet; read credentials/quota for retained usage and poll age."}
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot encode cached Codex usage"})
		return true
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, apiCallResponse{StatusCode: status, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: string(body)})
	return true
}
