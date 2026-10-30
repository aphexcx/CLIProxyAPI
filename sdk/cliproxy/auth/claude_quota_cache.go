package auth

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const claudeQuotaMinimumPollInterval = time.Minute

type claudeQuotaPollSeat struct {
	lastAttempt time.Time
	nextPoll    time.Time
	polledAt    time.Time
	running     bool
	queued      bool
	epoch       uint64
	polls       uint64
	rateLimits  uint64
	lastStatus  int
}

// ClaudeQuotaSnapshot contains only runtime usage, never credential material.
// Windows may include newer response headers; PolledAt is the last successful
// usage poll completion, not the time at which a management request read it.
type ClaudeQuotaSnapshot struct {
	Status     string                 `json:"status"`
	Windows    map[string]QuotaWindow `json:"windows"`
	PolledAt   *time.Time             `json:"polled_at"`
	AgeSeconds *int64                 `json:"age_seconds"`
	NextPollAt *time.Time             `json:"next_poll_at"`
	Refreshing bool                   `json:"refreshing"`
	Holds      []CooldownView         `json:"holds"`
	Polls      uint64                 `json:"polls"`
	RateLimits uint64                 `json:"rate_limits"`
	LastStatus int                    `json:"last_status"`
	LastPollAt *time.Time             `json:"last_poll_at"`
}

func (m *Manager) claudeQuotaPollSeatLocked(id string) *claudeQuotaPollSeat {
	if m.quotaPollSeats == nil {
		m.quotaPollSeats = make(map[string]*claudeQuotaPollSeat)
	}
	if m.quotaPollSeats[id] == nil {
		m.quotaPollSeats[id] = &claudeQuotaPollSeat{}
	}
	return m.quotaPollSeats[id]
}

// ClaudeQuotaCached returns a copy without performing or scheduling network I/O.
func (m *Manager) ClaudeQuotaCached(id string) (ClaudeQuotaSnapshot, bool) {
	if m == nil {
		return ClaudeQuotaSnapshot{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	auth := m.auths[id]
	if cfg == nil || !cfg.Claude.Quota.Enabled || auth == nil || !strings.EqualFold(auth.Provider, "claude") || auth.AuthKind() != AuthKindOAuth {
		return ClaudeQuotaSnapshot{}, false
	}
	now := time.Now()
	snapshot := ClaudeQuotaSnapshot{Status: "not_polled_yet", Windows: auth.Quota.Clone().Windows, Holds: CooldownSnapshotForAuth(auth, now)}
	if snapshot.Windows == nil {
		snapshot.Windows = map[string]QuotaWindow{}
	}
	next := now
	if seat := m.quotaPollSeats[id]; seat != nil {
		snapshot.Polls, snapshot.RateLimits, snapshot.LastStatus = seat.polls, seat.rateLimits, seat.lastStatus
		if !seat.lastAttempt.IsZero() {
			at := seat.lastAttempt
			snapshot.LastPollAt = &at
		}
		if seat.epoch == auth.RegistrationEpoch && !seat.polledAt.IsZero() {
			at := seat.polledAt
			age := max(int64(0), int64(now.Sub(at)/time.Second))
			snapshot.PolledAt, snapshot.AgeSeconds, snapshot.Status = &at, &age, "ok"
		}
		next = seat.nextPoll
		snapshot.Refreshing = seat.running || seat.queued
	}
	if claudeQuotaEligible(auth) {
		if next.Before(now) {
			next = now
		}
		if next.Before(m.quotaNextStart) {
			next = m.quotaNextStart
		}
		snapshot.NextPollAt = &next
	} else {
		snapshot.Status = "polling_paused"
	}
	return snapshot, true
}

// RequestClaudeQuotaPoll only queues work on the service loop. Repeated clicks,
// automatic polls and config reloads share the same per-seat minute budget.
func (m *Manager) RequestClaudeQuotaPoll(id string) bool {
	if m == nil {
		return false
	}
	m.quotaPollMu.Lock()
	defer m.quotaPollMu.Unlock()
	if m.quotaPollParent == nil || m.quotaPollParent.Err() != nil || m.quotaPollCancel == nil {
		return false
	}
	m.mu.Lock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Claude.Quota.Enabled || !claudeQuotaEligible(m.auths[id]) {
		m.mu.Unlock()
		return false
	}
	seat := m.claudeQuotaPollSeatLocked(id)
	now := time.Now()
	if seat.running || seat.queued || now.Before(seat.lastAttempt.Add(claudeQuotaMinimumPollInterval)) {
		m.mu.Unlock()
		return false
	}
	// A click joins the existing scheduled slot; it cannot accelerate the budget.
	seat.queued = true
	m.mu.Unlock()
	select {
	case m.quotaPollWake <- struct{}{}:
	default:
	}
	return true
}

func (m *Manager) reserveClaudeQuotaPoll(ctx context.Context, cfg *internalconfig.Config, auth *Auth, interval time.Duration, now time.Time) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !sameClaudeQuotaCredential(m.auths[auth.ID], auth) {
		return nil, false
	}
	seat := m.claudeQuotaPollSeatLocked(auth.ID)
	if seat.running || now.Before(seat.nextPoll) || now.Before(seat.lastAttempt.Add(claudeQuotaMinimumPollInterval)) || now.Before(m.quotaNextStart) {
		return nil, false
	}
	if seat.epoch != auth.RegistrationEpoch {
		seat.polledAt = time.Time{}
		seat.epoch = auth.RegistrationEpoch
	}
	seat.lastAttempt, seat.running, seat.queued = now, true, false
	seat.polls++
	seat.nextPoll = now.Add(max(interval, claudeQuotaMinimumPollInterval) + time.Duration(rand.Int64N(int64(5*time.Second))))
	// Spread a fleet's reads instead of bursting all seats on the minute.
	count := 0
	for _, candidate := range m.auths {
		if claudeQuotaEligible(candidate) {
			count++
		}
	}
	spacing := claudeQuotaMinimumPollInterval / time.Duration(max(count, 1))
	m.quotaNextStart = now.Add(spacing + time.Duration(rand.Int64N(int64(spacing/10+1))))
	return func() {
		m.mu.Lock()
		seat.running = false
		m.mu.Unlock()
	}, true
}

func (m *Manager) recordClaudeQuotaPoll(base *Auth, started time.Time, errPoll error) {
	status := 200
	if errPoll != nil {
		status = 0 // No HTTP status received (transport/cancellation).
		var statusError interface{ StatusCode() int }
		if errors.As(errPoll, &statusError) {
			status = statusError.StatusCode()
		}
	}
	log.WithFields(log.Fields{"auth_file": claudeQuotaName(base), "http_status": status, "latency_ms": time.Since(started).Milliseconds(), "success": errPoll == nil}).Info("claude quota: usage poll")
	m.mu.Lock()
	defer m.mu.Unlock()
	if seat := m.quotaPollSeats[base.ID]; seat != nil && seat.epoch == base.RegistrationEpoch && !seat.lastAttempt.After(started) && sameClaudeQuotaCredential(m.auths[base.ID], base) {
		seat.lastStatus = status
		if status == 429 {
			seat.rateLimits++
		}
	}
}
