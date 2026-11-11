package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	log "github.com/sirupsen/logrus"
)

// Claude cache affinity is automatic for built-in routing. It shares the manager's
// existing eligibility and model-availability checks; it never makes a seat ready.
// State is process-local and survives selector/config reloads on this manager.
type claudeSeatRouting struct {
	mu          sync.Mutex
	bindings    map[string]*claudeSeatBinding
	placements  map[string]*claudeSeatPlacement
	counts      map[claudeSeatCountKey]uint64
	nextCleanup time.Time
}

// Keep only the last seat per hashed identity for process-lifetime move accounting.
// This history never participates in selection or extends an affinity binding.
// Aliases share a record, so a return through any known alias sees the latest seat.
type claudeSeatPlacement struct {
	authID    string
	authIndex string
}

type claudeSeatBinding struct {
	authID     string
	authIndex  string
	clientKey  string
	identities map[string]bool
	expiresAt  time.Time
}

type claudeSeatCountKey struct {
	authID string
	model  string
}

// Each selection gets a wrapper; the affinity state belongs to the manager.
type claudeSeatSelector struct {
	manager  *Manager
	fallback Selector
	affinity *SessionAffinitySelector
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
	affinity, hasAffinity := fallback.(*SessionAffinitySelector)
	if hasAffinity {
		fallback = affinity.fallback
	}
	if !isBuiltInSelector(fallback) {
		return selector
	}
	return &claudeSeatSelector{manager: m, fallback: fallback, affinity: affinity, tried: tried}
}

func claudeClientKey(identity string, opts cliproxyexecutor.Options) string {
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
	routing := &s.manager.claudeSeats
	routing.mu.Lock()
	defer routing.mu.Unlock()
	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = "claude"
	rootIdentity := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	rootKey := claudeClientKey(rootIdentity, opts)
	identity, parentID, lcp := s.conversationIdentity(model, opts)
	key := claudeClientKey(identity, opts)
	if routing.bindings == nil {
		routing.bindings = make(map[string]*claudeSeatBinding)
		routing.placements = make(map[string]*claudeSeatPlacement)
		routing.counts = make(map[claudeSeatCountKey]uint64)
	}
	binding := routing.bindings[key]
	if binding == nil {
		binding = routing.bindings[rootKey]
	}
	var previous claudeSeatBinding
	hadBinding := binding != nil
	if hadBinding {
		previous = *binding
		key = binding.clientKey
	}
	if !now.Before(routing.nextCleanup) {
		for client, cached := range routing.bindings {
			if cached != binding && !now.Before(cached.expiresAt) {
				if client == cached.clientKey {
					log.WithFields(log.Fields{"provider": "claude", "client_hash": client,
						"auth_index": cached.authIndex, "reason": "cache_expired"}).Info("claude routing: binding expired")
				}
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
	if !hadBinding && parentID != "" {
		if parent, ok := routing.bindings[claudeClientKey(parentID, opts)]; ok && now.Before(parent.expiresAt) {
			for _, auth := range auths {
				if auth.ID == parent.authID {
					selected = auth
					reason = "parent_affinity"
					break
				}
			}
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
	} else if weighted, ok := s.fallback.(*WeightedRoundRobinSelector); ok {
		// A warm selection is forced, but still owes traffic to eligible peers.
		var reservation *weightedReservation
		var errPick error
		selected, reservation, errPick = weighted.reserveForRequest(ctx, "claude", model, opts, auths, selected)
		reservation.finish(errPick == nil && selected != nil)
		if errPick != nil || selected == nil {
			return selected, errPick
		}
	}
	if lcp {
		// Reuse the configured matcher's identity/tree bookkeeping after the seat
		// decision. A single forced candidate prevents it from changing placement.
		identitySelector := *s.affinity
		identitySelector.fallback = &FillFirstSelector{}
		identitySelector.rebindOnNever = false
		identitySelector.rebindOnQuotaOnly = false
		var reservation *weightedReservation
		_, _, _ = identitySelector.pickLCP(ctx, "claude", model, opts, []*Auth{selected}, selectorLogEntry(ctx), &reservation)
		reservation.finish(false)
		if matched := sessionMetadataString(opts.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey); matched != "" {
			identity = cliproxysession.BoundSessionIdentity(matched)
		}
	}
	opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = identity
	index := selected.EnsureIndex()
	placement := "fresh"
	if reason == "cache_warm" {
		placement = "warm"
	}
	if key != "" {
		expiresAt := now.Add(claudePromptCacheTTL(opts.OriginalRequest))
		if previous.authID == selected.ID && previous.expiresAt.After(expiresAt) {
			expiresAt = previous.expiresAt
		}
		if binding == nil {
			binding = &claudeSeatBinding{clientKey: key, identities: make(map[string]bool)}
		}
		binding.authID, binding.authIndex, binding.expiresAt = selected.ID, index, expiresAt
		binding.identities[identity] = true
		binding.identities[rootIdentity] = true
		routing.bindings[key] = binding
		if rootKey != "" {
			routing.bindings[rootKey] = binding
		}
		if identityKey := claudeClientKey(identity, opts); identityKey != "" {
			routing.bindings[identityKey] = binding
		}
		last := routing.placements[key]
		if last == nil {
			last = routing.placements[rootKey]
		}
		if last != nil && !hadBinding {
			reason = "cache_expired"
		}
		if last != nil && last.authID != selected.ID {
			placement = "move"
			log.WithFields(log.Fields{"provider": "claude", "model": model, "client_hash": key,
				"previous_auth_index": last.authIndex, "auth_index": index, "reason": reason}).Info("claude routing: client moved")
		}
		if last == nil {
			last = &claudeSeatPlacement{}
		}
		last.authID, last.authIndex = selected.ID, index
		routing.placements[key] = last
		if rootKey != "" {
			routing.placements[rootKey] = last
		}
		if identityKey := claudeClientKey(identity, opts); identityKey != "" {
			routing.placements[identityKey] = last
		}
	}
	countKey := claudeSeatCountKey{authID: selected.ID, model: canonicalModelKey(model)}
	routing.counts[countKey]++
	_, limited := s.scarceModelAvailability(ctx, selected, model, opts, now)
	log.WithFields(log.Fields{"provider": "claude", "model": countKey.model, "client_hash": key,
		"auth_index": index, "request_count": routing.counts[countKey], "reason": reason, "placement": placement,
		"scarce_model_limited": limited}).Info("claude routing: request")
	return selected, nil
}

// Keep configured parent and prefix identities while letting cache lifetime and
// current availability, rather than legacy rebind modes, govern seat placement.
func (s *claudeSeatSelector) conversationIdentity(model string, opts cliproxyexecutor.Options) (identity, parent string, lcp bool) {
	identity = CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if s.affinity == nil {
		return identity, "", false
	}
	if explicit, fallback := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata); explicit != "" {
		isFork, _ := opts.Metadata[cliproxyexecutor.IsForkMetadataKey].(bool)
		if s.affinity.subagentAffinity && !isFork {
			parent = cliproxysession.BoundSessionIdentity(fallback)
		}
		return identity, parent, false
	}
	if s.affinity.matcher == nil {
		return identity, "", false
	}
	turns := cliproxysession.ExtractCanonicalTurns(opts.SourceFormat, opts.OriginalRequest)
	if len(turns) == 0 {
		return identity, "", false
	}
	if match, ok := s.affinity.matcher.Match(lcpAffinityNamespace("claude", model, opts.Metadata), turns); ok {
		identity = cliproxysession.BoundSessionIdentity(match.SessionID)
		parent = cliproxysession.BoundSessionIdentity(match.ParentSessionID)
	}
	return identity, parent, true
}

func (r *claudeSeatRouting) lookup(sessionID string) (string, string) {
	identity := cliproxysession.BoundSessionIdentity(strings.TrimSpace(sessionID))
	if identity == "" {
		return "", "unbound"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var found string
	now := time.Now()
	candidates := []string{identity}
	hasPrefix := false
	for _, prefix := range cliproxysession.CandidateSessionPrefixes {
		hasPrefix = hasPrefix || strings.HasPrefix(identity, prefix)
	}
	if !hasPrefix {
		for _, prefix := range cliproxysession.CandidateSessionPrefixes {
			candidates = append(candidates, cliproxysession.BoundSessionIdentity(prefix+identity))
		}
	}
	for _, binding := range r.bindings {
		matches := false
		for _, candidate := range candidates {
			matches = matches || binding.identities[candidate]
		}
		if !matches || !now.Before(binding.expiresAt) {
			continue
		}
		if found != "" && found != binding.authID {
			return "", "ambiguous"
		}
		found = binding.authID
	}
	if found != "" {
		return found, "bound"
	}
	return "", "unbound"
}

// A scarce model is defined by the existing configured model-window mapping.
// Prefer a seat blocked on such a model only when that window excludes this
// request's model. The ready pool is never emptied by this preference.
func (s *claudeSeatSelector) preferScarceModelUnavailable(ctx context.Context, auths []*Auth, model string, opts cliproxyexecutor.Options, now time.Time) []*Auth {
	var preferred []*Auth
	for _, auth := range auths {
		if unavailable, _ := s.scarceModelAvailability(ctx, auth, model, opts, now); unavailable {
			preferred = append(preferred, auth)
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return auths
}

// Selection prefers any unavailable scarce model; accounting distinguishes quota
// limits from unrelated failures such as an upstream 503 or a disabled model.
func (s *claudeSeatSelector) scarceModelAvailability(ctx context.Context, auth *Auth, model string, opts cliproxyexecutor.Options, now time.Time) (unavailable, limited bool) {
	cfg, _ := s.manager.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return false, false
	}
	quota := cfg.Claude.Quota.WithDefaults()
	requested := canonicalModelKey(s.manager.selectionModelForAuth(auth, model))
	supported := make(map[string]bool)
	for _, advertised := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if advertised != nil {
			supported[canonicalModelKey(s.manager.selectionModelForAuth(auth, advertised.ID))] = true
		}
	}
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
			if !supported[canonicalModelKey(s.manager.selectionModelForAuth(auth, scarce))] {
				continue
			}
			blocked, reason, _ := isAuthBlockedForModelForRequest(ctx, auth, s.manager.selectionModelForAuth(auth, scarce), now, opts.Headers)
			if blocked {
				unavailable = true
				if reason == blockReasonCooldown {
					return true, true
				}
			}
		}
	}
	return unavailable, false
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
