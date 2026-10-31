package auth

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const maxCodexQuotaWindows = 20 // Main, code review and eight additional pairs.

// CodexQuotaWindows normalizes poll and response headers at receipt time.
// Percentages and resets must come from the same observation; no cooldown state
// is inferred from these informational windows.
func CodexQuotaWindows(headers http.Header, observedAt time.Time) map[string]QuotaWindow {
	if observedAt.IsZero() {
		return nil
	}
	var prefixes []string
	for name := range headers {
		name = http.CanonicalHeaderKey(name)
		if strings.HasPrefix(name, "X-Codex-") && strings.HasSuffix(name, "-Used-Percent") {
			prefixes = append(prefixes, strings.TrimSuffix(name, "Used-Percent"))
		}
	}
	sort.Strings(prefixes)
	windows := make(map[string]QuotaWindow)
	additional := make(map[string]bool)
	for _, prefix := range prefixes {
		used, errUsed := strconv.ParseFloat(headers.Get(prefix+"Used-Percent"), 64)
		minutes, errMinutes := strconv.ParseInt(headers.Get(prefix+"Window-Minutes"), 10, 64)
		if errUsed != nil || errMinutes != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
			continue
		}
		var name string
		switch minutes {
		case 300:
			name = "five_hour"
		case 10080:
			name = "seven_day"
		default:
			if minutes < 28*24*60 || minutes > 31*24*60 {
				continue
			}
			name = "monthly"
		}
		resetSeconds, errReset := strconv.ParseInt(headers.Get(prefix+"Reset-At"), 10, 64)
		var reset time.Time
		if errReset == nil && resetSeconds > 0 && resetSeconds <= 253402300799 {
			reset = time.Unix(resetSeconds, 0)
		} else if after, errAfter := strconv.ParseInt(headers.Get(prefix+"Reset-After-Seconds"), 10, 64); errAfter == nil && after >= 0 && after <= 366*24*60*60 {
			reset = observedAt.Add(time.Duration(after) * time.Second)
		} else {
			continue
		}
		namespace := strings.TrimPrefix(prefix, "X-Codex-")
		namespace = strings.TrimSuffix(strings.TrimSuffix(namespace, "Primary-"), "Secondary-")
		if namespace == "Code-Review-" {
			name = "code_review/" + name
		} else if namespace != "" {
			// HTTP uses a short namespace (bengalfox); polls/events use a
			// display-name namespace. Limit-Name is their shared identity.
			namespace = codexQuotaLimitIdentity(headers.Get("X-Codex-" + namespace + "Limit-Name"))
			if namespace == "" || (!additional[namespace] && len(additional) >= 8) {
				continue
			}
			additional[namespace] = true
			name = "additional/" + namespace + "/" + name
		}
		windows[name] = QuotaWindow{Utilization: used, ResetsAt: reset, ObservedAt: observedAt}
	}
	return windows
}

func codexQuotaLimitIdentity(name string) string {
	if len(name) > 128 || !validQuotaSignalValue(name) {
		return ""
	}
	name = strings.Map(func(char rune) rune {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			return char
		}
		return '-'
	}, strings.TrimSpace(name))
	return strings.ToLower(strings.Trim(name, "-"))
}

// Retain newer response windows even when an earlier poll body arrives later.
func mergeCodexQuotaWindows(auth *Auth, windows map[string]QuotaWindow) {
	mergeClaudeQuotaWindows(auth, windows)
	// Bound groups across observations too; rotating additional names must not
	// evict the main or code-review windows, even when those are older.
	names := make([]string, 0, len(auth.Quota.Windows))
	for name := range auth.Quota.Windows {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		leftAdditional := strings.HasPrefix(names[i], "additional/")
		rightAdditional := strings.HasPrefix(names[j], "additional/")
		if leftAdditional != rightAdditional {
			return !leftAdditional
		}
		left, right := auth.Quota.Windows[names[i]], auth.Quota.Windows[names[j]]
		if left.ObservedAt.Equal(right.ObservedAt) {
			return names[i] < names[j]
		}
		return left.ObservedAt.After(right.ObservedAt)
	})
	additional := make(map[string]bool)
	retained := 0
	for _, name := range names {
		group := name[:max(0, strings.LastIndex(name, "/"))]
		isAdditional := strings.HasPrefix(name, "additional/")
		if retained >= maxCodexQuotaWindows || (isAdditional && !additional[group] && len(additional) >= 8) {
			delete(auth.Quota.Windows, name)
			continue
		}
		if isAdditional {
			additional[group] = true
		}
		retained++
	}
}

func (m *Manager) applyCodexQuotaUsageSnapshot(ctx context.Context, cfg *internalconfig.Config, base *Auth, windows map[string]QuotaWindow, usage json.RawMessage, now time.Time) {
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.runtimeConfig.Load() != cfg || !cfg.Claude.Quota.Enabled || !sameClaudeQuotaCredential(m.auths[base.ID], base) {
		return
	}
	auth := m.auths[base.ID]
	mergeCodexQuotaWindows(auth, windows)
	seat := m.claudeQuotaPollSeatLocked(auth.ID)
	seat.polledAt, seat.epoch = now, auth.RegistrationEpoch
	seat.usage = append(json.RawMessage(nil), usage...)
}
