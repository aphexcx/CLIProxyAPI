package helps

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	codexRateLimitsEventType      = "codex.rate_limits"
	codexQuotaAdditionalHeaderKey = "X-Codex-Additional-"
	maxCodexAdditionalRateLimits  = 8
)

// ParseCodexQuotaEventHeaders converts one Codex websocket quota event into the
// same bounded header representation used by HTTP usage observations. Codex
// sends additional_rate_limits as an object on the websocket path, while the
// /wham/usage probe exposes the equivalent data as an array; both forms are
// normalized into namespaced X-Codex headers here.
//
// The websocket payload identifies each additional limit only by limit name
// ("GPT-5.3-Codex-Spark"), while HTTP responses namespace the same limit by a
// short name (x-codex-bengalfox-*). The two forms therefore cannot produce
// identical header names; the X-Codex-Additional- prefix marks the websocket
// origin, and the snapshot-replacement semantics in QuotaState keep the two
// spellings from accumulating side by side.
func ParseCodexQuotaEventHeaders(payload []byte) http.Header {
	if len(payload) == 0 {
		return nil
	}

	eventKind := codexQuotaEventKind(payload)
	if eventKind == codexQuotaEventOther {
		return nil
	}
	root := util.ParseGJSONBytesNoCopy(payload)
	if eventKind == codexQuotaEventError {
		return parseCodexQuotaHeadersObject(root.Get("headers"))
	}
	return parseCodexQuotaHeaders(root)
}

// ParseCodexUsageHeaders accepts the wham usage response without an event type.
// Only the same bounded, allowlisted quota fields as stream observations survive.
func ParseCodexUsageHeaders(payload []byte) http.Header {
	if !ojson.ValidBytes(payload) {
		return nil
	}
	return parseCodexQuotaHeaders(ojson.ParseBytes(payload), true)
}

// ParseCodexResetCreditSummary retains only nonnegative, integer credit counts.
// Credit details and arbitrary upstream metadata must not enter the usage cache.
func ParseCodexResetCreditSummary(payload []byte) map[string]float64 {
	if !ojson.ValidBytes(payload) {
		return nil
	}
	credits := firstCodexQuotaResult(ojson.ParseBytes(payload), "rate_limit_reset_credits", "rateLimitResetCredits")
	if !credits.IsObject() {
		return nil
	}
	summary := make(map[string]float64)
	for _, field := range []struct{ key, alias string }{
		{"available_count", "availableCount"},
		{"applicable_available_count", "applicableAvailableCount"},
	} {
		raw := firstCodexQuotaResult(credits, field.key, field.alias)
		if raw.Type != ojson.Number && raw.Type != ojson.String {
			continue
		}
		value, errParse := strconv.ParseFloat(strings.TrimSpace(raw.String()), 64)
		if errParse == nil && !math.IsNaN(value) && value >= 0 && value <= 1<<53-1 && math.Trunc(value) == value {
			summary[field.key] = value
		}
	}
	return summary
}

func parseCodexQuotaHeaders(root ojson.Result, fullUsage ...bool) http.Header {
	headers := make(http.Header)
	hasQuotaData := false
	complete := len(fullUsage) > 0 && fullUsage[0]

	baseRateLimits := firstCodexQuotaResult(root, "rate_limits", "rate_limit", "rateLimit")
	changed, valid := addCodexQuotaRateLimitHeaders(headers, "X-Codex-", baseRateLimits, complete)
	if !valid {
		return nil
	}
	if changed {
		// The main rate-limit object has no user-facing name in the websocket
		// payload, so its fields retain the HTTP response header names.
		hasQuotaData = true
	}

	additional := firstCodexQuotaResult(root, "additional_rate_limits", "additionalRateLimits")
	additionalCount := 0
	var identifiers map[string]bool
	if complete {
		identifiers = make(map[string]bool)
		if additional.Exists() && !additional.IsObject() && !additional.IsArray() {
			return nil
		}
	}
	if additional.Exists() && (additional.IsObject() || additional.IsArray()) {
		additional.ForEach(func(key, value ojson.Result) bool {
			if additionalCount >= maxCodexAdditionalRateLimits {
				valid = !complete // A truncated read cannot prove omissions.
				return false
			}
			limitName := strings.TrimSpace(key.String())
			if additional.IsArray() {
				limitName = firstCodexQuotaResultString(value, "limit_name", "limitName", "name")
			}
			if limitName == "" {
				if complete {
					valid = false
					return false
				}
				return true
			}
			identifier := normalizeCodexQuotaHeaderIdentifier(limitName)
			if complete {
				key := strings.ToLower(identifier)
				if key == "" || identifiers[key] || !validCodexQuotaEventText(limitName) || strings.IndexFunc(limitName, func(char rune) bool { return char < 32 || char == 127 }) >= 0 {
					valid = false
					return false
				}
				identifiers[key] = true
			}
			if identifier == "" {
				return true
			}
			rateInfo := firstCodexQuotaResult(value, "rate_limit", "rateLimit")
			if !rateInfo.Exists() {
				rateInfo = value
			}
			prefix := codexQuotaAdditionalHeaderKey + identifier + "-"
			var added bool
			added, valid = addCodexQuotaRateLimitHeaders(headers, prefix, rateInfo, complete)
			if !valid {
				return false
			}
			if added {
				// The limit name is upstream-controlled and lands in the
				// plain-text request log, so reject control characters here the
				// same way the plan type is validated below.
				if validCodexQuotaEventText(limitName) {
					headers.Set(prefix+"Limit-Name", strings.Clone(limitName))
				}
				hasQuotaData = true
				additionalCount++
			}
			return true
		})
	}
	if !valid {
		return nil
	}

	codeReview := firstCodexQuotaResult(root, "code_review_rate_limits", "code_review_rate_limit", "codeReviewRateLimits", "codeReviewRateLimit")
	changed, valid = addCodexQuotaRateLimitHeaders(headers, "X-Codex-Code-Review-", codeReview, complete)
	if !valid {
		return nil
	}
	if changed {
		hasQuotaData = true
	}

	credits := firstCodexQuotaResult(root, "credits")
	if credits.Exists() && credits.IsObject() {
		hasQuotaData = setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Has-Credits", credits, "has_credits", "hasCredits") || hasQuotaData
		hasQuotaData = setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Unlimited", credits, "unlimited") || hasQuotaData
		hasQuotaData = setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Balance", credits, "balance") || hasQuotaData
	}

	if !hasQuotaData {
		return nil
	}
	// A malformed active-limit name only invalidates that one header; the
	// window percentages collected above stay valid and must not be discarded.
	activeLimit := firstCodexQuotaResultString(root, "metered_limit_name", "meteredLimitName", "limit_name", "limitName")
	if validCodexQuotaEventIdentifier(activeLimit) {
		headers.Set("X-Codex-Active-Limit", activeLimit)
	}
	if planType := firstCodexQuotaResultString(root, "plan_type", "planType"); validCodexQuotaEventText(planType) {
		headers.Set("X-Codex-Plan-Type", planType)
	}
	return headers
}

const (
	codexQuotaEventOther = iota
	codexQuotaEventError
	codexQuotaEventRateLimits
)

// codexQuotaEventKind performs an allocation-free discriminator check for the
// common non-quota websocket event path. Codex frames carry the top-level
// type field within their opening bytes (after sequence_number at most), so
// only a bounded prefix is scanned instead of the whole frame; full JSON
// parsing is reserved for quota and error events.
func codexQuotaEventKind(payload []byte) int {
	const scanLimit = 256
	end := len(payload)
	if end > scanLimit {
		end = scanLimit
	}
	for i := 0; i+6 < end; i++ {
		if payload[i] != '"' || payload[i+1] != 't' || payload[i+2] != 'y' || payload[i+3] != 'p' || payload[i+4] != 'e' || payload[i+5] != '"' {
			continue
		}
		j := i + 6
		for j < end && (payload[j] == ' ' || payload[j] == '\t' || payload[j] == '\r' || payload[j] == '\n') {
			j++
		}
		if j >= end || payload[j] != ':' {
			continue
		}
		j++
		for j < end && (payload[j] == ' ' || payload[j] == '\t' || payload[j] == '\r' || payload[j] == '\n') {
			j++
		}
		if j+7 <= end && payload[j] == '"' && payload[j+1] == 'e' && payload[j+2] == 'r' && payload[j+3] == 'r' && payload[j+4] == 'o' && payload[j+5] == 'r' && payload[j+6] == '"' {
			return codexQuotaEventError
		}
		const rateLimits = `"codex.rate_limits"`
		if j+len(rateLimits) <= end {
			matched := true
			for offset := range rateLimits {
				if payload[j+offset] != rateLimits[offset] {
					matched = false
					break
				}
			}
			if matched {
				return codexQuotaEventRateLimits
			}
		}
	}
	return codexQuotaEventOther
}

func addCodexQuotaRateLimitHeaders(headers http.Header, prefix string, rateInfo ojson.Result, fullUsage bool) (bool, bool) {
	if !rateInfo.Exists() || !rateInfo.IsObject() {
		return false, !fullUsage || !rateInfo.Exists()
	}

	changed := false
	windowsReported := 0
	if setCodexQuotaScalarHeaderFromResult(headers, prefix+"Allowed", rateInfo, "allowed") {
		changed = true
	}
	if setCodexQuotaScalarHeaderFromResult(headers, prefix+"Limit-Reached", rateInfo, "limit_reached", "limitReached") {
		changed = true
	}
	for _, windowName := range []string{"primary", "secondary"} {
		window := firstCodexQuotaResult(rateInfo, windowName, windowName+"_window", windowName+"Window")
		if !window.Exists() || !window.IsObject() {
			if fullUsage && window.Exists() {
				return false, false
			}
			continue
		}
		used := firstCodexQuotaResult(window, "used_percent", "usedPercent")
		minutes := firstCodexQuotaResult(window, "window_minutes", "windowMinutes")
		windowMinutes := minutes.Int()
		if !minutes.Exists() {
			windowMinutes = firstCodexQuotaResult(window, "limit_window_seconds", "limitWindowSeconds").Int() / 60
		}
		resetAfter := firstCodexQuotaResult(window, "reset_after_seconds", "resetAfterSeconds")
		resetAt := firstCodexQuotaResult(window, "reset_at", "resetAt")
		hasResetAfter := resetAfter.Exists() && resetAfter.Int() >= 0
		hasResetAt := resetAt.Exists() && resetAt.Int() > 0
		if fullUsage {
			// A rejected window cannot masquerade as an absent limit in a full
			// read. A genuinely absent reset is valid and retains utilization.
			percent, errPercent := strconv.ParseFloat(codexQuotaScalarValue(used), 64)
			duration, unit := minutes, int64(1)
			if !duration.Exists() {
				duration, unit = firstCodexQuotaResult(window, "limit_window_seconds", "limitWindowSeconds"), 60
			}
			length, errLength := strconv.ParseInt(codexQuotaScalarValue(duration), 10, 64)
			if errPercent != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 || errLength != nil || length <= 0 || length%unit != 0 {
				return false, false
			}
			windowMinutes = length / unit
			if windowMinutes != 300 && windowMinutes != 10080 && (windowMinutes < 28*24*60 || windowMinutes > 31*24*60) {
				return false, false
			}
			for _, reset := range []struct {
				value    ojson.Result
				min, max int64
			}{{resetAfter, 0, 366 * 24 * 60 * 60}, {resetAt, 1, 253402300799}} {
				if reset.value.Exists() {
					seconds, errReset := strconv.ParseInt(codexQuotaScalarValue(reset.value), 10, 64)
					if errReset != nil || seconds < reset.min || seconds > reset.max {
						return false, false
					}
				}
			}
		}
		if !used.Exists() ||
			used.Float() < 0 || used.Float() > 100 || windowMinutes <= 0 ||
			(!fullUsage && !hasResetAfter && !hasResetAt) {
			continue
		}
		windowPrefix := prefix + strings.ToUpper(windowName[:1]) + windowName[1:] + "-"
		setCodexQuotaScalarHeaderFromResult(headers, windowPrefix+"Used-Percent", window, "used_percent", "usedPercent")
		headers.Set(windowPrefix+"Window-Minutes", strconv.FormatInt(windowMinutes, 10))
		if hasResetAfter {
			setCodexQuotaScalarHeaderFromResult(headers, windowPrefix+"Reset-After-Seconds", window, "reset_after_seconds", "resetAfterSeconds")
		}
		if hasResetAt {
			setCodexQuotaScalarHeaderFromResult(headers, windowPrefix+"Reset-At", window, "reset_at", "resetAt")
		}
		changed = true
		windowsReported++
	}
	if fullUsage && windowsReported == 0 {
		allowed := firstCodexQuotaResult(rateInfo, "allowed")
		reached := firstCodexQuotaResult(rateInfo, "limit_reached", "limitReached")
		if (allowed.Exists() && !allowed.Bool()) || reached.Bool() {
			return false, false // Exhaustion with no usable window is not room.
		}
	}
	return changed, true
}

func parseCodexQuotaHeadersObject(headersNode ojson.Result) http.Header {
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	headers := make(http.Header)
	headersNode.ForEach(func(key, value ojson.Result) bool {
		name := http.CanonicalHeaderKey(strings.TrimSpace(key.String()))
		if !isCodexQuotaHeaderName(name) {
			return true
		}
		if raw := codexQuotaScalarValue(value); raw != "" {
			headers.Set(name, raw)
		}
		return true
	})
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func isCodexQuotaHeaderName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
		return true
	}
	if lower == "x-codex-active-limit" || lower == "x-codex-plan-type" ||
		strings.HasPrefix(lower, "x-codex-credits-") {
		return true
	}
	if !strings.HasPrefix(lower, "x-codex-") {
		return false
	}
	for _, marker := range []string{
		"-allowed",
		"-limit-reached",
		"-limit-name",
		"-used-percent",
		"-window-minutes",
		"-reset-after-seconds",
		"-reset-at",
		"-over-secondary-limit-percent",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func setCodexQuotaScalarHeader(headers http.Header, name string, object ojson.Result, paths ...string) bool {
	return setCodexQuotaScalarHeaderFromResult(headers, name, object, paths...)
}

func setCodexQuotaScalarHeaderFromResult(headers http.Header, name string, object ojson.Result, paths ...string) bool {
	value := firstCodexQuotaResult(object, paths...)
	if !value.Exists() {
		return false
	}
	raw := codexQuotaScalarValue(value)
	if raw == "" {
		return false
	}
	headers.Set(name, raw)
	return true
}

func codexQuotaScalarValue(value ojson.Result) string {
	switch value.Type {
	case ojson.String:
		return strings.Clone(strings.TrimSpace(value.String()))
	case ojson.Number, ojson.True, ojson.False:
		return strings.Clone(strings.TrimSpace(value.Raw))
	default:
		return ""
	}
}

func firstCodexQuotaResult(object ojson.Result, paths ...string) ojson.Result {
	for _, path := range paths {
		value := object.Get(path)
		if value.Exists() && value.Type != ojson.Null {
			return value
		}
	}
	return ojson.Result{}
}

func firstCodexQuotaResultString(object ojson.Result, paths ...string) string {
	for _, path := range paths {
		value := firstCodexQuotaResult(object, path)
		if raw := codexQuotaScalarValue(value); raw != "" {
			return raw
		}
	}
	return ""
}

func normalizeCodexQuotaHeaderIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(value))
	lastDash := false
	for _, char := range value {
		switch {
		case (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_':
			builder.WriteRune(char)
			lastDash = false
		case char == '-':
			builder.WriteRune(char)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	identifier := strings.Trim(builder.String(), "-_.")
	if identifier == "" || !validCodexQuotaEventIdentifier(identifier) {
		return ""
	}
	return identifier
}

func validCodexQuotaEventIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func validCodexQuotaEventText(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n")
}
