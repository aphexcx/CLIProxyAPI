package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// Claude cache affinity is automatic for built-in routing. It shares the manager's
// existing eligibility and model-availability checks; it never makes a seat ready.
// State is process-local and survives selector/config reloads on this manager.
type claudeSeatRouting struct {
	mu          sync.Mutex
	bindings    map[string]claudeSeatBinding
	counts      map[claudeSeatCountKey]uint64
	nextCleanup time.Time
}

type claudeSeatBinding struct {
	authID    string
	authIndex string
	expiresAt time.Time
}

type claudeSeatCountKey struct {
	authID string
	model  string
}

// Each selection gets a wrapper; the affinity state belongs to the manager.
type claudeSeatSelector struct {
	manager  *Manager
	fallback Selector
	tried    map[string]struct{}
}

func onlyClaudeProviders(providers []string) bool {
	if len(providers) == 0 {
		return false
	}
	for _, provider := range providers {
		if canonicalSchedulingProvider(provider) != "claude" {
			return false
		}
	}
	return true
}

func (m *Manager) claudeSelector(selector Selector, providers []string, tried map[string]struct{}) Selector {
	if !onlyClaudeProviders(providers) {
		return selector
	}
	fallback := selector
	if affinity, ok := fallback.(*SessionAffinitySelector); ok {
		fallback = affinity.fallback
	}
	if !isBuiltInSelector(fallback) {
		return selector
	}
	return &claudeSeatSelector{manager: m, fallback: fallback, tried: tried}
}

func claudeClientKey(opts cliproxyexecutor.Options) string {
	identity := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if identity == "" {
		return ""
	}
	scope := sessionMetadataString(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
	digest := sha256.Sum256([]byte("claude-seat-v1\x00" + scope + "\x00" + identity))
	return hex.EncodeToString(digest[:])
}

// Only protocol cache controls affect affinity, never arbitrary tool input or text.
// The longest declared cache lifetime protects all the request's cacheable blocks.
func claudePromptCacheTTL(payload []byte) time.Duration {
	ttl := 5 * time.Minute
	root := ojson.ParseBytes(payload)
	check := func(block ojson.Result) {
		control := block.Get("cache_control")
		if control.Get("type").String() == "ephemeral" && control.Get("ttl").String() == "1h" {
			ttl = time.Hour
		}
	}
	check(root)
	for _, field := range []string{"system", "tools", "messages"} {
		root.Get(field).ForEach(func(_, item ojson.Result) bool {
			check(item)
			if field == "messages" {
				item.Get("content").ForEach(func(_, block ojson.Result) bool {
					check(block)
					return true
				})
			}
			return true
		})
	}
	return ttl
}

func (s *claudeSeatSelector) Pick(ctx context.Context, _ string, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	if _, weighted := s.fallback.(*WeightedRoundRobinSelector); weighted {
		auths = positiveWeightAuths(auths)
	}
	now := time.Now()
	key := claudeClientKey(opts)
	routing := &s.manager.claudeSeats
	routing.mu.Lock()
	defer routing.mu.Unlock()
	if routing.bindings == nil {
		routing.bindings = make(map[string]claudeSeatBinding)
		routing.counts = make(map[claudeSeatCountKey]uint64)
	}
	previous, hadBinding := routing.bindings[key]
	if !now.Before(routing.nextCleanup) {
		for client, binding := range routing.bindings {
			if client != key && !now.Before(binding.expiresAt) {
				log.WithFields(log.Fields{"provider": "claude", "client_hash": client,
					"auth_index": binding.authIndex, "reason": "cache_expired"}).Info("claude routing: binding expired")
				delete(routing.bindings, client)
			}
		}
		routing.nextCleanup = now.Add(time.Minute)
	}
	var selected *Auth
	reason := "new_client"
	if hadBinding {
		if now.Before(previous.expiresAt) {
			for _, auth := range auths {
				if auth.ID == previous.authID {
					selected = auth
					break
				}
			}
			reason = "cache_warm"
			if selected == nil {
				reason = s.moveReason(ctx, previous.authID, model, opts, now)
			}
		} else {
			reason = "cache_expired"
		}
	}
	if selected == nil {
		candidates := s.preferScarceModelUnavailable(ctx, auths, model, opts, now)
		var errPick error
		// The manager already validated each credential against its resolved upstream
		// model. Preserve that decision when the fallback narrows by priority/weight.
		fallbackCtx := context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
		selected, errPick = s.fallback.Pick(fallbackCtx, "claude", model, opts, candidates)
		if errPick != nil || selected == nil {
			return selected, errPick
		}
	}
	index := selected.EnsureIndex()
	if key != "" {
		expiresAt := now.Add(claudePromptCacheTTL(opts.OriginalRequest))
		if previous.authID == selected.ID && previous.expiresAt.After(expiresAt) {
			expiresAt = previous.expiresAt
		}
		routing.bindings[key] = claudeSeatBinding{authID: selected.ID, authIndex: index, expiresAt: expiresAt}
		if hadBinding && previous.authID != selected.ID {
			log.WithFields(log.Fields{"provider": "claude", "model": model, "client_hash": key,
				"previous_auth_index": previous.authIndex, "auth_index": index, "reason": reason}).Info("claude routing: client moved")
		}
	}
	countKey := claudeSeatCountKey{authID: selected.ID, model: canonicalModelKey(model)}
	routing.counts[countKey]++
	log.WithFields(log.Fields{"provider": "claude", "model": countKey.model, "client_hash": key,
		"auth_index": index, "request_count": routing.counts[countKey], "reason": reason}).Info("claude routing: request")
	return selected, nil
}

// A scarce model is defined by the existing configured model-window mapping.
// Prefer a seat blocked on such a model only when that window excludes this
// request's model. The ready pool is never emptied by this preference.
func (s *claudeSeatSelector) preferScarceModelUnavailable(ctx context.Context, auths []*Auth, model string, opts cliproxyexecutor.Options, now time.Time) []*Auth {
	cfg, _ := s.manager.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return auths
	}
	quota := cfg.Claude.Quota.WithDefaults()
	var preferred []*Auth
	for _, auth := range auths {
		requested := canonicalModelKey(s.manager.selectionModelForAuth(auth, model))
		prefer := false
		for _, models := range quota.ModelWindows {
			includesRequested := false
			for _, scarce := range models {
				if canonicalModelKey(s.manager.selectionModelForAuth(auth, scarce)) == requested {
					includesRequested = true
				}
			}
			if includesRequested {
				continue
			}
			for _, scarce := range models {
				if !s.manager.authSupportsRouteModel(registry.GetGlobalRegistry(), auth, scarce) {
					continue
				}
				blocked, _, _ := isAuthBlockedForModelForRequest(ctx, auth, s.manager.selectionModelForAuth(auth, scarce), now, opts.Headers)
				prefer = prefer || blocked
			}
		}
		if prefer {
			preferred = append(preferred, auth)
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return auths
}

func (s *claudeSeatSelector) moveReason(ctx context.Context, id, model string, opts cliproxyexecutor.Options, now time.Time) string {
	if _, tried := s.tried[id]; tried {
		return "upstream_error"
	}
	if pinned := pinnedAuthIDFromMetadata(opts.Metadata); pinned != "" && pinned != id {
		return "pinned_auth"
	}
	auth, ok := s.manager.GetByID(id)
	if !ok {
		return "removed"
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return "disabled"
	}
	if blocked, reason, _ := isAuthBlockedForModelForRequest(ctx, auth, s.manager.selectionModelForAuth(auth, model), now, opts.Headers); blocked {
		if reason == blockReasonCooldown {
			if auth.Quota.Reason == "credential_quota" {
				return "credential_cooldown"
			}
			return "model_cooldown"
		}
		return "credential_unavailable"
	}
	return "model_or_policy_unavailable"
}
