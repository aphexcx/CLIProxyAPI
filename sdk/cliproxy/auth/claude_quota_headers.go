package auth

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// The upstream Claude rate-limit parser names 7d_oi as the Fable window.
// Require utilization and that window's own reset from the SAME response;
// status, Retry-After, and unified/overage resets do not identify its usage.
func claudeQuotaHeaderWindows(headers http.Header, observedAt time.Time) map[string]QuotaWindow {
	if observedAt.IsZero() {
		return nil
	}
	signals := collectQuotaSignals("claude", headers)
	windows := make(map[string]QuotaWindow)
	for _, mapping := range []struct{ header, window string }{
		{"5h", "five_hour"}, {"7d", "seven_day"}, {"7d_oi", "Fable"},
	} {
		prefix := "Anthropic-Ratelimit-Unified-" + mapping.header
		fraction, errParse := strconv.ParseFloat(signals[http.CanonicalHeaderKey(prefix+"-Utilization")], 64)
		percent := fraction * 100
		if errParse != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 {
			continue
		}
		rawReset := signals[http.CanonicalHeaderKey(prefix+"-Reset")]
		reset, errReset := time.Parse(time.RFC3339Nano, rawReset)
		if errReset != nil {
			seconds, errSeconds := strconv.ParseFloat(rawReset, 64)
			if errSeconds != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds >= float64(math.MaxInt64) {
				continue
			}
			reset = time.Unix(int64(seconds), int64((seconds-math.Trunc(seconds))*1e9))
		}
		if reset.IsZero() || reset.Year() < 1 || reset.Year() > 9999 {
			continue
		}
		windows[mapping.window] = QuotaWindow{Utilization: percent, ResetsAt: reset, ObservedAt: observedAt}
	}
	return windows
}

// Retain the newest valid observation per window, including expired windows as
// watermarks. Deleting an expired watermark would let an older response with a
// longer reset become fresh again. Only newly accepted observations may arm holds.
func mergeClaudeQuotaWindows(auth *Auth, windows map[string]QuotaWindow, now time.Time) map[string]QuotaWindow {
	fresh := make(map[string]QuotaWindow, len(windows))
	for name, window := range windows {
		if window.ResetsAt.IsZero() || math.IsNaN(window.Utilization) || math.IsInf(window.Utilization, 0) || window.Utilization < 0 {
			continue
		}
		if window.ObservedAt.IsZero() {
			window.ObservedAt = now
		}
		if previous, ok := auth.Quota.Windows[name]; ok && !window.ObservedAt.After(previous.ObservedAt) {
			continue
		}
		if auth.Quota.Windows == nil {
			auth.Quota.Windows = make(map[string]QuotaWindow)
		}
		auth.Quota.Windows[name] = window
		fresh[name] = window
	}
	return fresh
}
