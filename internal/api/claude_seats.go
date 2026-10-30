package api

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed claude_seats.html
var claudeSeatsHTML string

// The shell contains no seat data. All reads and refreshes require the existing
// management key and its localhost/remote policy, through the v8 API.
func (s *Server) serveClaudeSeats(c *gin.Context) {
	if !s.managementAvailable(c) {
		return
	}
	cfg := s.getConfig()
	if !cfg.Claude.Quota.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(claudeSeatsHTML))
}
