package management

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const claudeResetRankingRule = "Fable at cap, then near cap (90%+); longest until weekly reset, then earliest grant expiry. Skip weekly resets within 24h, stale reads and unusable grants. Holder action only."

type claudeLimitResetSeat struct {
	Name      string `json:"name"`
	AuthIndex string `json:"auth_index"`
	coreauth.ClaudeLimitResetsSnapshot
	FableUtilization *float64 `json:"fable_utilization"`
	BestClickRank    *int     `json:"best_click_rank"`
	RankReason       string   `json:"rank_reason"`
	grantExpiry      time.Time
}

// GetClaudeLimitResets reads only the quota loop's cache, including when filtered.
// It never queues a poll, refreshes credentials, or submits an upstream request.
func (h *Handler) GetClaudeLimitResets(c *gin.Context) {
	seats := make([]claudeLimitResetSeat, 0)
	now := time.Now()
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil || !strings.EqualFold(auth.Provider, "claude") {
				continue
			}
			name := auth.FileName
			if name == "" {
				name = auth.ID
			}
			seat := claudeLimitResetSeat{Name: name, AuthIndex: auth.Index, ClaudeLimitResetsSnapshot: h.authManager.ClaudeLimitResetsCached(auth.ID)}
			quota, _ := h.authManager.ClaudeQuotaCached(auth.ID)
			for windowName, window := range quota.Windows {
				if (strings.EqualFold(windowName, "Fable") || strings.EqualFold(windowName, "seven_day_fable")) && (window.ResetsAt.IsZero() || window.ResetsAt.After(now)) {
					value := window.Utilization
					if seat.FableUtilization == nil || value > *seat.FableUtilization {
						seat.FableUtilization = &value
					}
				}
			}
			seat.RankReason = resetRankReason(&seat, now)
			if auth.Disabled {
				seat.RankReason = "seat disabled"
			}
			seats = append(seats, seat)
		}
	}
	rankClaudeResetSeats(seats)
	filter := strings.TrimSpace(c.Query("auth"))
	if filter != "" {
		filtered := make([]claudeLimitResetSeat, 0, 1)
		for _, seat := range seats {
			if seat.Name == filter {
				filtered = append(filtered, seat)
			}
		}
		seats = filtered
		if len(seats) == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "Claude auth name not found"})
			return
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"seats": seats, "ranking_rule": claudeResetRankingRule})
}

func resetRankReason(seat *claudeLimitResetSeat, now time.Time) string {
	seat.grantExpiry = time.Time{}
	status := seat.CedarEmber
	if seat.Stale || status == nil {
		return "reset status unavailable or stale"
	}
	if !status.Eligible {
		return "seat ineligible"
	}
	if status.CooldownUntil != nil && status.CooldownUntil.After(now) {
		return "reset cooldown active"
	}
	if status.WeeklyResetsAt == nil {
		return "weekly reset unknown"
	}
	if !status.WeeklyResetsAt.After(now.Add(24 * time.Hour)) {
		return "weekly reset within 24h"
	}
	if seat.FableUtilization == nil {
		return "Fable usage unknown"
	}
	if *seat.FableUtilization < 90 {
		return "below 90% Fable usage"
	}
	for _, grant := range status.Grants {
		if !slices.Contains(grant.Clears, "seven_day_overage_included") {
			continue
		}
		if grant.ResetsLeft <= 0 || !grant.UsableNow || grant.Paused || grant.EndsAt == nil || !grant.EndsAt.After(now) || (grant.StartsAt != nil && grant.StartsAt.After(now)) {
			continue
		}
		if seat.grantExpiry.IsZero() || grant.EndsAt.Before(seat.grantExpiry) {
			seat.grantExpiry = *grant.EndsAt
		}
	}
	if seat.grantExpiry.IsZero() {
		return "no usable unexpired grant"
	}
	return "holder action only"
}

func rankClaudeResetSeats(seats []claudeLimitResetSeat) {
	sort.Slice(seats, func(i, j int) bool { return seats[i].Name < seats[j].Name })
	candidates := make([]*claudeLimitResetSeat, 0, len(seats))
	for i := range seats {
		if seats[i].RankReason == "holder action only" {
			candidates = append(candidates, &seats[i])
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		aWall, bWall := *a.FableUtilization >= 100, *b.FableUtilization >= 100
		if aWall != bWall {
			return aWall
		}
		if !a.CedarEmber.WeeklyResetsAt.Equal(*b.CedarEmber.WeeklyResetsAt) {
			return a.CedarEmber.WeeklyResetsAt.After(*b.CedarEmber.WeeklyResetsAt)
		}
		return a.grantExpiry.Before(b.grantExpiry)
	})
	for i, seat := range candidates {
		rank := i + 1
		seat.BestClickRank = &rank
	}
}
