package helps

import (
	"context"
	"net/http"
	"regexp"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

type claudeOneHourCacheRuleOffContextKey struct{}

// WithClaudeOneHourCacheRuleOff marks a request the executor re-sends after the
// upstream refused the one-hour rule's change: the rule stays off, so the body
// goes upstream as the client wrote it and no second fallback can follow.
func WithClaudeOneHourCacheRuleOff(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, claudeOneHourCacheRuleOffContextKey{}, true)
}

// ClaudeOneHourCacheRuleOff reports whether ctx carries WithClaudeOneHourCacheRuleOff.
func ClaudeOneHourCacheRuleOff(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	off, _ := ctx.Value(claudeOneHourCacheRuleOffContextKey{}).(bool)
	return off
}

// claudeCacheLifetimeWords matches cache_control, cache-ttl, or the word ttl
// bounded by non-letters, so "throttle" does not match.
var claudeCacheLifetimeWords = regexp.MustCompile(`(?i)cache_control|cache-ttl|(?:^|[^a-z])ttl(?:$|[^a-z])`)

// IsClaudeCacheLifetimeRefusal reports whether an upstream answer is an HTTP 400
// whose error message names the cache lifetime.
func IsClaudeCacheLifetimeRefusal(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	message := string(body)
	if ojson.ValidBytes(body) {
		if errorMessage := ojson.GetBytesNoCopy(body, "error.message").String(); errorMessage != "" {
			message = errorMessage
		}
	}
	return claudeCacheLifetimeWords.MatchString(message)
}

// NoteClaudeOneHourCacheRefusal logs and counts one refusal of a request the
// one-hour rule changed. It logs the seat as the reporter labels it, the
// client and the upstream status, never a body.
func NoteClaudeOneHourCacheRefusal(ctx context.Context, reporter *UsageReporter, client string, status int) {
	LogWithRequestID(ctx).WithFields(log.Fields{
		"auth_index":  reporter.authIndexForLog(),
		"client_hash": client,
		"status":      status,
	}).Warn("claude one-hour cache: upstream refused the one-hour lifetime; re-sending as the client wrote it")
	DefaultClaudeCacheMeter().RecordRuleRefused()
}
