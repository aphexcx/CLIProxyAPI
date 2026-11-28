package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type claudeCacheMeterResponse struct {
	RuleEnabled bool `json:"rule_enabled"`
	helps.ClaudeCacheSnapshot
}

// GetClaudeCacheMeter reports the in-memory Claude cache meter: per UTC day,
// client and kind, the cache-write tokens at five minutes and at one hour and
// the cache-read tokens, the day's seat moves filed as kept or re-paid by
// relation and warm or expired, plus whether claude-header-defaults.one-hour-cache is on.
func (h *Handler) GetClaudeCacheMeter(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	ruleEnabled := coreauth.ClaudeOneHourCacheEnabled(h.cfg)
	h.mu.Unlock()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, claudeCacheMeterResponse{
		RuleEnabled:         ruleEnabled,
		ClaudeCacheSnapshot: helps.DefaultClaudeCacheMeter().Snapshot(),
	})
}
