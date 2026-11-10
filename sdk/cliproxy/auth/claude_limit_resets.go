package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ClaudeLimitResets is an observation, never authorization to spend a reset.
type ClaudeLimitResets struct {
	Eligible         bool               `json:"eligible"`
	IneligibleReason *string            `json:"ineligible_reason"`
	AtLimit          bool               `json:"at_limit"`
	WeeklyResetsAt   *time.Time         `json:"weekly_resets_at"`
	CooldownUntil    *time.Time         `json:"cooldown_until"`
	Grants           []ClaudeResetGrant `json:"grants"`
}

type ClaudeResetGrant struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	ResetsLeft       int        `json:"resets_left"`
	ResetsTotal      *int       `json:"resets_total,omitempty"`
	StartsAt         *time.Time `json:"starts_at,omitempty"`
	EndsAt           *time.Time `json:"ends_at"`
	Type             string     `json:"type"`
	Clears           []string   `json:"clears"`
	UsableNow        bool       `json:"usable_now"`
	Paused           bool       `json:"paused,omitempty"`
	UseRequiresLimit *bool      `json:"use_requires_limit,omitempty"`
	GivesBackWeekly  *bool      `json:"gives_back_weekly,omitempty"`
}

// ParseClaudeLimitResets accepts the captured cedar_ember shape and ignores
// unrelated usage fields. Errors contain no upstream data or credentials.
func ParseClaudeLimitResets(usage json.RawMessage) (*ClaudeLimitResets, error) {
	var envelope struct {
		CedarEmber json.RawMessage `json:"cedar_ember"`
	}
	if json.Unmarshal(usage, &envelope) != nil || len(envelope.CedarEmber) == 0 || string(envelope.CedarEmber) == "null" {
		return nil, errors.New("cedar_ember status unavailable")
	}
	var required struct {
		Eligible *bool `json:"eligible"`
		Grants   []struct {
			ID         string `json:"id"`
			ResetsLeft *int   `json:"resets_left"`
		} `json:"grants"`
	}
	var status ClaudeLimitResets
	if json.Unmarshal(envelope.CedarEmber, &required) != nil || required.Eligible == nil || json.Unmarshal(envelope.CedarEmber, &status) != nil {
		return nil, errors.New("invalid cedar_ember status")
	}
	if status.Eligible && required.Grants == nil {
		return nil, errors.New("cedar_ember grants unavailable")
	}
	seen := make(map[string]bool)
	for i, grant := range required.Grants {
		if strings.TrimSpace(grant.ID) == "" || seen[grant.ID] || grant.ResetsLeft == nil || *grant.ResetsLeft < 0 {
			return nil, errors.New("invalid cedar_ember grant")
		}
		seen[grant.ID] = true
		g := &status.Grants[i]
		if g.ResetsTotal != nil && (*g.ResetsTotal < 0 || g.ResetsLeft > *g.ResetsTotal) {
			return nil, errors.New("invalid cedar_ember grant count")
		}
		if g.Clears == nil {
			g.Clears = []string{}
		}
	}
	if status.Grants == nil {
		status.Grants = []ClaudeResetGrant{}
	}
	return &status, nil
}

type ClaudeLimitResetsSnapshot struct {
	CedarEmber *ClaudeLimitResets `json:"cedar_ember"`
	Error      string             `json:"error,omitempty"`
	LastGoodAt *time.Time         `json:"last_good_at"`
	AgeSeconds *int64             `json:"age_seconds"`
	Stale      bool               `json:"stale"`
}

// ClaudeLimitResetsCached never schedules or performs I/O. A malformed reset
// block cannot discard its last good observation or advance that observation's age.
func (m *Manager) ClaudeLimitResetsCached(id string) ClaudeLimitResetsSnapshot {
	snapshot := ClaudeLimitResetsSnapshot{Error: "quota polling unavailable", Stale: true}
	quota, ok := m.ClaudeQuotaCached(id)
	if !ok {
		return snapshot
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, seat := m.auths[id], m.quotaPollSeats[id]
	if auth == nil || seat == nil || seat.epoch != auth.RegistrationEpoch || seat.resetCredentialVersion != auth.CredentialVersion {
		snapshot.Error = "not polled yet"
		return snapshot
	}
	if seat.resetStatus != nil {
		// Reparse the isolated status to copy all slices and pointers.
		_ = json.Unmarshal(seat.resetStatus, &snapshot.CedarEmber)
		at := seat.resetPolledAt
		age := max(int64(0), int64(time.Since(at)/time.Second))
		snapshot.LastGoodAt, snapshot.AgeSeconds = &at, &age
	}
	snapshot.Error = seat.resetError
	snapshot.Stale = quota.Stale || snapshot.CedarEmber == nil || snapshot.Error != ""
	if seat.lastFailed {
		snapshot.Stale = true
		snapshot.Error = "usage poll failed"
		if seat.lastStatus != 0 {
			snapshot.Error = fmt.Sprintf("usage poll failed (HTTP %d)", seat.lastStatus)
		}
	} else if quota.Status == "polling_paused" {
		snapshot.Error = "polling paused"
	} else if snapshot.Stale && snapshot.Error == "" {
		snapshot.Error = "cached reset status is stale"
	}
	return snapshot
}

func (seat *claudeQuotaPollSeat) recordLimitResets(usage json.RawMessage, now time.Time) {
	status, err := ParseClaudeLimitResets(usage)
	if err != nil {
		seat.resetError = err.Error()
		return
	}
	seat.resetStatus, _ = json.Marshal(status)
	seat.resetPolledAt, seat.resetError = now, ""
}
