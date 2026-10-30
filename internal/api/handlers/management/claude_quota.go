package management

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// claudeUsagePayload retains the OAuth usage shape understood by the panel.
// It adds freshness and diagnostics, without inventing absent plan/billing data.
func claudeUsagePayload(snapshot coreauth.ClaudeQuotaSnapshot) gin.H {
	payload := gin.H{
		"status": snapshot.Status, "polled_at": snapshot.PolledAt, "age_seconds": snapshot.AgeSeconds,
		"next_poll_at": snapshot.NextPollAt, "refreshing": snapshot.Refreshing, "holds": snapshot.Holds,
		"windows": snapshot.Windows, "polls": snapshot.Polls, "rate_limits": snapshot.RateLimits,
		"last_status": snapshot.LastStatus, "last_poll_at": snapshot.LastPollAt,
	}
	limits := make([]gin.H, 0)
	names := make([]string, 0, len(snapshot.Windows))
	for name := range snapshot.Windows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		window := snapshot.Windows[name]
		if name == "five_hour" || name == "seven_day" {
			payload[name] = window
		} else {
			limits = append(limits, gin.H{"kind": "weekly_scoped", "scope": gin.H{"model": gin.H{"display_name": name}}, "percent": window.Utilization, "resets_at": window.ResetsAt, "is_active": true})
		}
	}
	payload["limits"] = limits
	return payload
}

func (h *Handler) cachedClaudeUsage(auth *coreauth.Auth, refresh bool) (coreauth.ClaudeQuotaSnapshot, bool) {
	if auth == nil || h.authManager == nil {
		return coreauth.ClaudeQuotaSnapshot{}, false
	}
	if refresh {
		h.authManager.RequestClaudeQuotaPoll(auth.ID)
	}
	return h.authManager.ClaudeQuotaCached(auth.ID)
}

// GetClaudeCredentialQuota is a read of the native loop's cache, including for
// never-polled seats. POST queues the same loop; neither path calls upstream.
func (h *Handler) GetClaudeCredentialQuota(c *gin.Context) {
	index := strings.TrimSpace(c.Query("auth_index"))
	refresh := c.Query("refresh") == "true"
	if c.Request.Method == http.MethodPost {
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if errBind := c.ShouldBindJSON(&body); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		index, refresh = strings.TrimSpace(body.AuthIndex), true
	}
	if index == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	snapshot, ok := h.cachedClaudeUsage(h.authByIndex(index), refresh)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "native Claude quota unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, claudeUsagePayload(snapshot))
}

func (h *Handler) serveCachedClaudeAPICall(c *gin.Context, auth *coreauth.Auth) bool {
	if !c.GetBool(ConfigV8ContextKey) {
		return false
	}
	snapshot, ok := h.cachedClaudeUsage(auth, true)
	if !ok {
		return false
	}
	body, errMarshal := json.Marshal(claudeUsagePayload(snapshot))
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot encode cached Claude usage"})
		return true
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, apiCallResponse{StatusCode: http.StatusOK, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: string(body)})
	return true
}

func (h *Handler) serveCachedClaudePluginQuota(c *gin.Context, auth *coreauth.Auth) bool {
	if !c.GetBool(ConfigV8ContextKey) {
		return false
	}
	snapshot, ok := h.cachedClaudeUsage(auth, c.Request.Method == http.MethodPost || c.Query("refresh") == "true")
	if !ok {
		return false
	}
	payload := claudeUsagePayload(snapshot)
	groups := make([]pluginapi.QuotaGroup, 0, len(snapshot.Windows))
	names := make([]string, 0, len(snapshot.Windows))
	for name := range snapshot.Windows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		window := snapshot.Windows[name]
		groups = append(groups, pluginapi.QuotaGroup{DisplayName: name, Buckets: []pluginapi.QuotaBucket{{Window: name, RemainingFraction: math.Max(0, 1-window.Utilization/100), ResetTime: window.ResetsAt.Format(time.RFC3339Nano)}}})
	}
	payload["groups"] = groups
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, payload)
	return true
}
