package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ClaudeQuotaMinimumPollInterval is shared by validation and the poll scheduler.
const ClaudeQuotaMinimumPollInterval = time.Minute

// ClaudeQuotaConfig enables and schedules Claude and Codex OAuth usage reads.
// Caps, model windows and reset priority apply only to Claude. Codex usage and
// account-wide Claude windows remain observational.
type ClaudeQuotaConfig struct {
	Enabled         bool                `yaml:"enabled" json:"enabled"`
	PollInterval    string              `yaml:"poll-interval" json:"poll-interval"` // Default and minimum 1m.
	CapPercent      *float64            `yaml:"cap-percent" json:"cap-percent"`
	WindowCaps      map[string]float64  `yaml:"window-caps" json:"window-caps"`
	ModelWindows    map[string][]string `yaml:"model-windows" json:"model-windows"`
	PriorityAtReset bool                `yaml:"priority-at-reset" json:"priority-at-reset"`
}

// WithDefaults preserves explicit empty model mappings and explicit cap values.
func (q ClaudeQuotaConfig) WithDefaults() ClaudeQuotaConfig {
	if strings.TrimSpace(q.PollInterval) == "" {
		q.PollInterval = "1m"
	}
	if q.CapPercent == nil {
		capPercent := float64(100)
		q.CapPercent = &capPercent
	}
	if q.WindowCaps == nil {
		q.WindowCaps = map[string]float64{}
	}
	if q.ModelWindows == nil {
		q.ModelWindows = map[string][]string{"Fable": {"claude-fable-5-1"}}
	}
	return q
}

// CapFor returns a window override or the default per-model cap.
func (q ClaudeQuotaConfig) CapFor(window string) float64 {
	if capPercent, ok := q.WindowCaps[window]; ok {
		return capPercent
	}
	if q.CapPercent != nil {
		return *q.CapPercent
	}
	return 100
}

// Validate rejects unsafe or ambiguous rules, including account-wide pre-holds.
func (q ClaudeQuotaConfig) Validate() error {
	q = q.WithDefaults()
	interval, errParse := time.ParseDuration(q.PollInterval)
	if errParse != nil || interval <= 0 {
		return fmt.Errorf("upstream.claude.quota.poll-interval must be a positive duration")
	}
	if interval < ClaudeQuotaMinimumPollInterval {
		return fmt.Errorf("upstream.claude.quota.poll-interval must be at least 1m")
	}
	validCap := func(capPercent float64) bool {
		return !math.IsNaN(capPercent) && !math.IsInf(capPercent, 0) && capPercent > 0 && capPercent <= 100
	}
	if !validCap(*q.CapPercent) {
		return fmt.Errorf("upstream.claude.quota.cap-percent must be greater than 0 and at most 100")
	}
	for window, capPercent := range q.WindowCaps {
		if strings.TrimSpace(window) == "" || window != strings.TrimSpace(window) || !validCap(capPercent) {
			return fmt.Errorf("upstream.claude.quota.window-caps contains an invalid window or cap")
		}
		if (window == "five_hour" || window == "seven_day") && capPercent < 100 {
			return fmt.Errorf("upstream.claude.quota.window-caps.%s cannot be below 100", window)
		}
	}
	for window, models := range q.ModelWindows {
		if window == "five_hour" || window == "seven_day" {
			return fmt.Errorf("upstream.claude.quota.model-windows cannot pre-hold %s", window)
		}
		if strings.TrimSpace(window) == "" || window != strings.TrimSpace(window) {
			return fmt.Errorf("upstream.claude.quota.model-windows contains an invalid window")
		}
		for _, model := range models {
			if strings.TrimSpace(model) == "" {
				return fmt.Errorf("upstream.claude.quota.model-windows contains an empty model")
			}
		}
	}
	return nil
}
