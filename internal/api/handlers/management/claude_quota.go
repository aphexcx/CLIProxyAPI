package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const staleClaudeUsageMessage = "Cached Claude usage is stale or not polled yet; open /claude-seats.html to view retained usage and its poll age."

// claudeUsagePayload retains the OAuth usage shape understood by the panel.
// It adds freshness and diagnostics, without inventing absent plan/billing data.
func claudeUsagePayload(snapshot coreauth.ClaudeQuotaSnapshot) gin.H {
	payload := gin.H{
		"status": snapshot.Status, "stale": snapshot.Stale, "polled_at": snapshot.PolledAt, "age_seconds": snapshot.AgeSeconds,
		"next_poll_at": snapshot.NextPollAt, "refreshing": snapshot.Refreshing, "holds": snapshot.Holds,
		"windows": snapshot.Windows, "polls": snapshot.Polls, "rate_limits": snapshot.RateLimits,
		"last_status": snapshot.LastStatus, "last_poll_at": snapshot.LastPollAt,
	}
	if len(snapshot.Usage) > 0 {
		var full gin.H
		if errDecode := json.Unmarshal(snapshot.Usage, &full); errDecode == nil && full != nil {
			for key, value := range payload {
				full[key] = value
			}
			return full
		}
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

// GetClaudeCredentialQuota reads the native Claude or Codex cache, including
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
	auth := h.authByIndex(index)
	if auth != nil && strings.EqualFold(auth.Provider, "codex") {
		if snapshot, ok := h.cachedCodexUsage(auth, refresh); ok {
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, codexUsagePayload(snapshot))
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "native Codex quota unavailable"})
		return
	}
	snapshot, ok := h.cachedClaudeUsage(auth, refresh)
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
	payload := claudeUsagePayload(snapshot)
	status := http.StatusOK
	if snapshot.Stale {
		// The existing panel stamps successful API calls as freshly observed,
		// ignoring cache timestamps. Do not report stale usage as a new success.
		status = http.StatusServiceUnavailable
		payload["error"] = gin.H{"message": staleClaudeUsageMessage}
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot encode cached Claude usage"})
		return true
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, apiCallResponse{StatusCode: status, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: string(body)})
	return true
}

func (h *Handler) serveCachedClaudePluginQuota(c *gin.Context, auth *coreauth.Auth, host *pluginhost.Host, pluginID string) bool {
	if !c.GetBool(ConfigV8ContextKey) {
		return false
	}
	snapshot, ok := h.cachedClaudeUsage(auth, c.Request.Method == http.MethodPost || c.Query("refresh") == "true")
	if !ok {
		return false
	}
	c.Header("Cache-Control", "no-store")
	if snapshot.Stale {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": staleClaudeUsageMessage}})
		return true
	}
	payload := claudeUsagePayload(snapshot)
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot encode cached Claude usage"})
		return true
	}
	// Let the selected plugin keep its subscription, summary and bucket semantics.
	// Its HTTP client can only read this snapshot; no request can reach upstream.
	normalized, handled, errNormalize := host.FetchQuotaByPlugin(c.Request.Context(), pluginID, pluginapi.QuotaFetchRequest{
		AuthIndex: auth.Index, AuthID: auth.ID, Provider: auth.Provider, Metadata: auth.Metadata, Attributes: auth.Attributes,
		HTTPClient: cachedClaudeQuotaClient{body: body},
	})
	if !handled || errNormalize != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cannot normalize cached Claude usage"})
		return true
	}
	payload["groups"], payload["subscription"], payload["summary"] = normalized.Groups, normalized.Subscription, normalized.Summary
	payload["serverTimeOffsetMs"] = normalized.ServerTimeOffsetMs
	c.JSON(http.StatusOK, payload)
	return true
}

type cachedClaudeQuotaClient struct{ body []byte }

func (client cachedClaudeQuotaClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if errContext := ctx.Err(); errContext != nil {
		return pluginapi.HTTPResponse{}, errContext
	}
	if req.Method != http.MethodGet || req.URL != claude.UsageURL {
		return pluginapi.HTTPResponse{}, errors.New("only the cached Claude usage request is available")
	}
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}}, Body: append([]byte(nil), client.body...)}, nil
}

func (cachedClaudeQuotaClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("cached Claude usage does not support streaming")
}
