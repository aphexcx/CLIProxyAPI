package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

const seatOpus = "claude-opus-4-6"

type seatRoutingFixture struct {
	m        *Manager
	ids      []string
	hook     *logtest.Hook
	failSeat string
	attempts []string
}

func newSeatRoutingFixture(t *testing.T, selector Selector) *seatRoutingFixture {
	t.Helper()
	withQuotaCooldownEnabled(t)
	f := &seatRoutingFixture{m: NewManager(nil, selector, nil)}
	f.m.SetRetryConfig(0, 0, 0)
	cfg := &config.Config{}
	cfg.Claude.Quota.Enabled = true
	cfg.Claude.Quota.PriorityAtReset = true
	f.m.SetConfig(cfg)
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	f.hook = logtest.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previousHooks) })
	for _, suffix := range []string{"a", "b"} {
		id := t.Name() + "-" + suffix
		f.ids = append(f.ids, id)
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: quotaFable}, {ID: seatOpus}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := f.m.Register(context.Background(), &Auth{
			ID: id, Provider: "claude", Status: StatusActive,
			Attributes: map[string]string{"priority": "0"},
			Metadata:   map[string]any{"access_token": "test-only-token"},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	execute := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		f.attempts = append(f.attempts, auth.ID)
		if f.failSeat == auth.ID {
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "fake upstream unavailable"}
		}
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	f.m.RegisterExecutor(&customStreamMockExecutor{
		identifier:              "claude",
		mockCustomErrorExecutor: mockCustomErrorExecutor{identifier: "claude", executeFn: execute, countFn: execute},
		streamFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			response, errExecute := execute(ctx, auth, req, opts)
			if errExecute != nil {
				return nil, errExecute
			}
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	})
	return f
}

func (f *seatRoutingFixture) request(t *testing.T, client, model, ttl, path string) string {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"model":%q,"system":[{"type":"text","text":"private prompt","cache_control":{"type":"ephemeral","ttl":%q}}],"messages":[{"role":"user","content":"hello"}]}`, model, ttl))
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
		Headers:  http.Header{"X-Session-Id": []string{client}},
		Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "shared-access-key-scope"}}
	req := cliproxyexecutor.Request{Model: model, Payload: payload}
	if path == "stream" {
		stream, errStream := f.m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
		if errStream != nil {
			t.Fatal(errStream)
		}
		var response strings.Builder
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			response.Write(chunk.Payload)
		}
		return response.String()
	}
	var response cliproxyexecutor.Response
	var errExecute error
	if path == "count" {
		response, errExecute = f.m.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
	} else {
		response, errExecute = f.m.Execute(context.Background(), []string{"claude"}, req, opts)
	}
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	return string(response.Payload)
}

func (f *seatRoutingFixture) quota(t *testing.T, id string, used float64) {
	t.Helper()
	now := time.Now()
	auth, _ := f.m.GetByID(id)
	f.m.applyTestClaudeQuotaUsage(context.Background(), f.m.runtimeConfig.Load().(*config.Config), auth,
		map[string]QuotaWindow{
			"Fable":     {Utilization: used, ResetsAt: now.Add(24 * time.Hour)},
			"five_hour": {Utilization: 10, ResetsAt: now.Add(time.Hour)},
			"seven_day": {Utilization: 10, ResetsAt: now.Add(24 * time.Hour)},
		}, now)
}

func (f *seatRoutingFixture) events(message string) []log.Entry {
	var events []log.Entry
	for _, entry := range f.hook.AllEntries() {
		if entry.Message == message {
			events = append(events, *entry)
		}
	}
	return events
}

func TestClaudeSeatRoutingPerModel(t *testing.T) {
	for _, path := range []string{"execute", "stream", "count"} {
		t.Run(path, func(t *testing.T) {
			f := newSeatRoutingFixture(t, &RoundRobinSelector{})
			f.quota(t, f.ids[0], 10)
			f.quota(t, f.ids[1], 100)
			if got := f.request(t, "opus-client", seatOpus, "", path); got != f.ids[1] {
				t.Errorf("Opus seat = %s, want Fable-limited seat %s", got, f.ids[1])
			}
			if got := f.request(t, "fable-client", quotaFable, "", path); got != f.ids[0] {
				t.Errorf("Fable seat = %s, want %s", got, f.ids[0])
			}
		})
	}
}

func TestClaudeSeatRoutingFallbackAndCounts(t *testing.T) {
	f := newSeatRoutingFixture(t, &RoundRobinSelector{})
	f.quota(t, f.ids[0], 10)
	auth, _ := f.m.GetByID(f.ids[1])
	auth.Disabled = true
	if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for i := 0; i < 3; i++ {
		if got := f.request(t, "private-client", seatOpus, "", "execute"); got != f.ids[0] {
			t.Fatalf("fallback = %s, want %s", got, f.ids[0])
		}
	}
	events := f.events("claude routing: request")
	if len(events) != 3 || events[2].Data["request_count"] != uint64(3) {
		t.Fatalf("missing cumulative seat/model request counts: %+v", events)
	}
	formatted, errFormat := (&logging.LogFormatter{}).Format(&events[2])
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	for _, value := range []string{"request_count=3", "client_hash=", "auth_index=", "model=" + seatOpus} {
		if !strings.Contains(string(formatted), value) {
			t.Errorf("log omits %s: %s", value, formatted)
		}
	}
	for _, secret := range []string{"private-client", "private prompt", "shared-access-key-scope", "test-only-token"} {
		if strings.Contains(string(formatted), secret) {
			t.Errorf("log exposes %q", secret)
		}
	}
}

func TestClaudeSeatRoutingWarmClient(t *testing.T) {
	for name, selector := range map[string]Selector{
		"round-robin": &RoundRobinSelector{}, "weighted": &WeightedRoundRobinSelector{}, "fill-first": &FillFirstSelector{},
		"configured-affinity": NewSessionAffinitySelector(&RoundRobinSelector{}),
	} {
		t.Run(name, func(t *testing.T) {
			if stoppable, ok := selector.(interface{ Stop() }); ok {
				t.Cleanup(stoppable.Stop)
			}
			f := newSeatRoutingFixture(t, selector)
			first := f.request(t, "warm", seatOpus, "", "execute")
			for i := 0; i < 12; i++ {
				if got := f.request(t, "warm", seatOpus, "", "execute"); got != first {
					t.Fatalf("request %d moved from %s to %s without a reason", i, first, got)
				}
			}
			other := f.ids[0]
			if other == first {
				other = f.ids[1]
			}
			f.quota(t, other, 100)
			auth, _ := f.m.GetByID(other)
			auth.Attributes["priority"] = "20"
			if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if got := f.request(t, "warm", seatOpus, "", "execute"); got != first {
				t.Fatalf("scarcity/priority preference moved warm client to %s", got)
			}
			if got := f.request(t, "warm", quotaFable, "", "execute"); got != first {
				t.Fatalf("model switch moved warm client to %s", got)
			}
		})
	}
}

func TestClaudeSeatRoutingFailoverOnce(t *testing.T) {
	for _, scenario := range []string{"disabled", "model_cooldown", "upstream_error", "lcp-disabled", "lcp-model_cooldown", "lcp-upstream_error"} {
		t.Run(scenario, func(t *testing.T) {
			var selector Selector = &RoundRobinSelector{}
			client := "failover"
			cause := strings.TrimPrefix(scenario, "lcp-")
			if strings.HasPrefix(scenario, "lcp-") {
				affinity := NewSessionAffinitySelector(selector)
				t.Cleanup(affinity.Stop)
				selector, client = affinity, ""
			}
			f := newSeatRoutingFixture(t, selector)
			first := f.request(t, client, seatOpus, "", "execute")
			switch cause {
			case "disabled":
				auth, _ := f.m.GetByID(first)
				auth.Disabled = true
				if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			case "model_cooldown":
				retry := time.Hour
				f.m.MarkResult(context.Background(), Result{AuthID: first, Provider: "claude", Model: seatOpus,
					Error: &Error{HTTPStatus: 429, Message: "model limit"}, RetryAfter: &retry})
			case "upstream_error":
				f.failSeat = first
			}
			second := f.request(t, client, seatOpus, "", "execute")
			if second == first {
				t.Fatal("client did not fail over")
			}
			for i := 0; i < 5; i++ {
				if got := f.request(t, client, seatOpus, "", "execute"); got != second {
					t.Fatalf("client moved again: %s", got)
				}
			}
			moves := f.events("claude routing: client moved")
			if len(moves) != 1 || moves[0].Data["reason"] != cause {
				t.Fatalf("want one recorded %s move, got %+v", cause, moves)
			}
		})
	}
}

func TestClaudeSeatRoutingCacheLifetime(t *testing.T) {
	for _, ttl := range []string{"", "5m", "1h"} {
		t.Run("ttl="+ttl, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newSeatRoutingFixture(t, &RoundRobinSelector{})
				lifetime := 5 * time.Minute
				if ttl == "1h" {
					lifetime = time.Hour
				}
				first := f.request(t, "cache", seatOpus, ttl, "execute")
				// Sleep advances the synctest clock, never wall time.
				time.Sleep(lifetime - time.Second)
				if got := f.request(t, "cache", seatOpus, ttl, "execute"); got != first {
					t.Fatalf("warm binding expired early: %s", got)
				}
				time.Sleep(lifetime + time.Second)
				if got := f.request(t, "cache", seatOpus, ttl, "execute"); got == first {
					t.Fatal("expired binding did not return to round-robin placement")
				}
				moves := f.events("claude routing: client moved")
				if len(moves) != 1 || moves[0].Data["reason"] != "cache_expired" {
					t.Fatalf("expiry move not recorded: %+v", moves)
				}
			})
		})
	}
}

func TestClaudeSeatRoutingEverySeatChangeLogged(t *testing.T) {
	for _, path := range []string{"execute", "stream", "count"} {
		for _, swept := range []bool{false, true} {
			for _, changeSeat := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/swept=%t/change=%t", path, swept, changeSeat), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						f := newSeatRoutingFixture(t, &RoundRobinSelector{})
						f.quota(t, f.ids[0], 100)
						f.quota(t, f.ids[1], 10)
						previous := f.request(t, "private-returning-client", seatOpus, "", path)
						clientHash := f.events("claude routing: request")[0].Data["client_hash"]
						if len(f.events("claude routing: client moved")) != 0 {
							t.Fatal("first placement emitted a move")
						}
						for turn := 0; turn < 3; turn++ {
							// Advance only the synctest clock; no wall-clock wait.
							time.Sleep(6 * time.Minute)
							want := previous
							if changeSeat {
								want = f.ids[0]
								if want == previous {
									want = f.ids[1]
								}
							}
							for _, id := range f.ids {
								used := float64(10)
								if id == want {
									used = 100
								}
								f.quota(t, id, used)
							}
							if swept {
								f.request(t, fmt.Sprintf("sweeper-%d", turn), seatOpus, "", path)
							}
							before := len(f.events("claude routing: client moved"))
							if got := f.request(t, "private-returning-client", seatOpus, "", path); got != want {
								t.Fatalf("placement changed: got %s, want %s", got, want)
							}
							moves := f.events("claude routing: client moved")[before:]
							if changeSeat {
								oldAuth, _ := f.m.GetByID(previous)
								newAuth, _ := f.m.GetByID(want)
								if len(moves) != 1 {
									t.Fatalf("seat change %d emitted %d moves, want exactly one", turn, len(moves))
								}
								data := moves[0].Data
								if data["client_hash"] != clientHash || data["reason"] != "cache_expired" ||
									data["previous_auth_index"] != oldAuth.EnsureIndex() || data["auth_index"] != newAuth.EnsureIndex() {
									t.Fatalf("incorrect move: %+v", data)
								}
							} else if len(moves) != 0 {
								t.Fatalf("same-seat placement emitted %d moves", len(moves))
							}
							before = len(f.events("claude routing: client moved"))
							f.request(t, "private-returning-client", seatOpus, "", path)
							if len(f.events("claude routing: client moved")) != before {
								t.Fatal("warm stay emitted another move")
							}
							previous = want
						}
					})
				})
			}
		}
	}
}

func TestClaudeSeatRoutingRequestLogFacts(t *testing.T) {
	for _, path := range []string{"execute", "stream", "count"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newSeatRoutingFixture(t, &RoundRobinSelector{})
				check := func(placement string, limited bool) {
					t.Helper()
					events := f.events("claude routing: request")
					entry := events[len(events)-1]
					formatted, errFormat := (&logging.LogFormatter{}).Format(&entry)
					if errFormat != nil {
						t.Fatal(errFormat)
					}
					for _, field := range []string{"placement=" + placement, fmt.Sprintf("scarce_model_limited=%t", limited)} {
						if !strings.Contains(string(formatted), field) {
							t.Errorf("missing %s: %s", field, formatted)
						}
					}
					for _, secret := range []string{"private-log-client", "private prompt", "shared-access-key-scope", "test-only-token"} {
						if strings.Contains(string(formatted), secret) {
							t.Errorf("log exposes %q", secret)
						}
					}
					t.Log(strings.TrimSpace(string(formatted)))
				}
				f.quota(t, f.ids[0], 10)
				f.quota(t, f.ids[1], 100)
				f.request(t, "private-log-client", seatOpus, "", path)
				check("fresh", true)
				f.request(t, "private-log-client", seatOpus, "", path)
				check("warm", true)
				// Equal-time quota observations conservatively retain the higher usage.
				time.Sleep(time.Second)
				f.quota(t, f.ids[1], 10)
				f.request(t, "private-log-client", seatOpus, "", path)
				check("warm", false)
				f.quota(t, f.ids[0], 100)
				time.Sleep(6 * time.Minute)
				f.request(t, "unrelated-sweeper", seatOpus, "", path)
				f.request(t, "private-log-client", seatOpus, "", path)
				check("move", true)
				moves := f.events("claude routing: client moved")
				if len(moves) != 1 {
					t.Fatalf("idle return emitted %d moves", len(moves))
				}
				formatted, errFormat := (&logging.LogFormatter{}).Format(&moves[0])
				if errFormat != nil {
					t.Fatal(errFormat)
				}
				t.Log(strings.TrimSpace(string(formatted)))
				time.Sleep(6 * time.Minute)
				f.request(t, "private-log-client", seatOpus, "", path)
				check("fresh", true)
				f.request(t, "private-fable-client", quotaFable, "", path)
				check("fresh", false)
			})
		})
	}
}

func TestClaudeSeatRoutingIndependentClients(t *testing.T) {
	f := newSeatRoutingFixture(t, &RoundRobinSelector{})
	a := f.request(t, "client-a", seatOpus, "", "execute")
	b := f.request(t, "client-b", seatOpus, "", "execute")
	if a == b {
		t.Fatal("two new clients did not receive independent round-robin placements")
	}
	for _, client := range []string{"client-b", "client-b", "client-a", "client-b", "client-a"} {
		want := a
		if client == "client-b" {
			want = b
		}
		if got := f.request(t, client, seatOpus, "", "execute"); got != want {
			t.Fatalf("%s moved from %s to %s", client, want, got)
		}
	}
}

func TestClaudeSeatRoutingExistingIdentities(t *testing.T) {
	for _, identity := range []string{"metadata", "derived-root"} {
		t.Run(identity, func(t *testing.T) {
			f := newSeatRoutingFixture(t, &RoundRobinSelector{})
			request := func(client string, continuation bool) string {
				metadata := "{}"
				root := client
				if identity == "metadata" {
					metadata = fmt.Sprintf(`{"user_id":%q}`, fmt.Sprintf(`{"session_id":%q}`, client))
					root = "identical prompt for two sessions"
				}
				messages := fmt.Sprintf(`{"role":"user","content":%q}`, root)
				if continuation {
					messages += `,{"role":"assistant","content":"reply"},{"role":"user","content":"next"}`
				}
				payload := []byte(fmt.Sprintf(`{"model":%q,"metadata":%s,"messages":[%s]}`, seatOpus, metadata, messages))
				response, errExecute := f.m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: seatOpus, Payload: payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "same-key"}})
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				return string(response.Payload)
			}
			a, b := request("a", false), request("b", false)
			if a == b {
				t.Fatal("independent existing identities collapsed onto one binding")
			}
			for _, client := range []string{"b", "b", "a", "b", "a"} {
				want := a
				if client == "b" {
					want = b
				}
				if got := request(client, true); got != want {
					t.Fatalf("%s identity changed during continuation: %s != %s", identity, got, want)
				}
			}
		})
	}
}

func TestClaudeSeatRoutingInactivePluginHost(t *testing.T) {
	for _, entrypoint := range []string{"single", "mixed"} {
		t.Run(entrypoint, func(t *testing.T) {
			f := newSeatRoutingFixture(t, &RoundRobinSelector{})
			f.m.SetPluginScheduler(&inactivePluginScheduler{})
			pick := func() string {
				opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"host-client"}}}
				if entrypoint == "mixed" {
					return f.request(t, "host-client", seatOpus, "", "execute")
				}
				auth, errPick := f.m.SelectAuth(context.Background(), "claude", seatOpus, opts)
				if errPick != nil {
					t.Fatal(errPick)
				}
				return auth.ID
			}
			first := pick()
			if second := pick(); second != first {
				t.Fatalf("inactive host bypassed affinity: %s -> %s", first, second)
			}
			f.quota(t, f.ids[0], 10)
			f.quota(t, f.ids[1], 100)
			if got := f.request(t, "fresh-with-host", seatOpus, "", "execute"); got != f.ids[1] {
				t.Fatalf("inactive host bypassed scarcity: %s", got)
			}
		})
	}
}

func TestClaudeSeatRoutingWeightedEligibility(t *testing.T) {
	for _, entrypoint := range []string{"single", "mixed"} {
		t.Run(entrypoint, func(t *testing.T) {
			f := newSeatRoutingFixture(t, &WeightedRoundRobinSelector{})
			auth, _ := f.m.GetByID(f.ids[0])
			auth.Attributes[AttributeWeight] = "0"
			if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			retry := time.Hour
			f.m.MarkResult(context.Background(), Result{AuthID: f.ids[1], Provider: "claude", Model: seatOpus,
				Error: &Error{HTTPStatus: 429, Message: "model limit"}, RetryAfter: &retry})
			var errPick error
			if entrypoint == "single" {
				_, errPick = f.m.SelectAuth(context.Background(), "claude", seatOpus, cliproxyexecutor.Options{})
			} else {
				_, _, _, errPick = f.m.pickNextMixed(context.Background(), []string{"claude"}, seatOpus, cliproxyexecutor.Options{}, nil)
			}
			var cooldown *modelCooldownError
			if !errors.As(errPick, &cooldown) || cooldown.StatusCode() != 429 || cooldown.Headers().Get("Retry-After") == "" {
				t.Fatalf("zero weight masked eligible cooldown: %v", errPick)
			}
		})
	}
}

func TestClaudeSeatRoutingWeightedForcedCredits(t *testing.T) {
	for _, entrypoint := range []string{"single", "mixed"} {
		for _, forced := range []string{"pin", "warm"} {
			t.Run(entrypoint+"/"+forced, func(t *testing.T) {
				f := newSeatRoutingFixture(t, &WeightedRoundRobinSelector{})
				auth, _ := f.m.GetByID(f.ids[0])
				auth.Attributes[AttributeWeight] = "3"
				if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				pick := func(client, pin string) string {
					opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {client}}, Metadata: map[string]any{}}
					if pin != "" {
						opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = pin
					}
					var selected *Auth
					var errPick error
					if entrypoint == "single" {
						selected, errPick = f.m.SelectAuth(context.Background(), "claude", seatOpus, opts)
					} else {
						selected, _, _, errPick = f.m.pickNextMixed(context.Background(), []string{"claude"}, seatOpus, opts, nil)
					}
					if errPick != nil {
						t.Fatal(errPick)
					}
					return selected.ID
				}
				for i := 0; i < 4; i++ {
					pin := f.ids[1]
					if forced == "warm" && i > 0 {
						pin = ""
					}
					if got := pick("bound", pin); got != f.ids[1] {
						t.Fatalf("forced selection escaped: %s", got)
					}
				}
				for i := 0; i < 12; i++ {
					if got := pick(fmt.Sprintf("fresh-%d", i), ""); got != f.ids[0] {
						t.Fatalf("request %d did not compensate forced traffic: %s", i, got)
					}
				}
			})
		}
	}
}

func TestClaudeSeatRoutingScarceModelAliases(t *testing.T) {
	for _, name := range []string{"alias", "prefix"} {
		t.Run(name, func(t *testing.T) {
			f := newSeatRoutingFixture(t, &FillFirstSelector{})
			f.quota(t, f.ids[0], 10)
			f.quota(t, f.ids[1], 100)
			if name == "alias" {
				f.m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: quotaFable, Alias: "fable"}}})
			}
			for _, id := range f.ids {
				route := "fable"
				if name == "prefix" {
					auth, _ := f.m.GetByID(id)
					auth.Prefix = "seat"
					if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
						t.Fatal(errUpdate)
					}
					route = "seat/" + quotaFable
				}
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: route}, {ID: seatOpus}})
			}
			if got := f.request(t, "fresh-alias", seatOpus, "", "execute"); got != f.ids[1] {
				t.Fatalf("scarce upstream model hidden behind %s: selected %s", name, got)
			}
		})
	}
}

func TestClaudeSeatRoutingConfiguredIdentity(t *testing.T) {
	for _, scenario := range []string{"parent", "lcp", "lookup"} {
		t.Run(scenario, func(t *testing.T) {
			selector := NewSessionAffinitySelector(&RoundRobinSelector{})
			t.Cleanup(selector.Stop)
			f := newSeatRoutingFixture(t, selector)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude,
				Headers: http.Header{"X-Claude-Code-Session-Id": {"root"}}, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"},
				OriginalRequest: []byte(`{"messages":[{"role":"user","content":"root task"}]}`)}
			if scenario == "lcp" {
				opts.Headers = nil
			}
			first, errFirst := f.m.SelectAuth(context.Background(), "claude", seatOpus, opts)
			if errFirst != nil {
				t.Fatal(errFirst)
			}
			switch scenario {
			case "parent":
				opts.Headers.Set("X-Claude-Code-Agent-Id", "child")
			case "lcp":
				if opts.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] == nil {
					t.Fatal("configured LCP identity was discarded")
				}
				opts.Metadata = map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}
				opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"root task"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`)
			case "lookup":
				auth, status := f.m.LookupSessionAffinity("claude", seatOpus, "root")
				if auth == nil || auth.ID != first.ID || status != "bound" {
					t.Fatalf("lookup lost active affinity: %v %s", auth, status)
				}
				return
			}
			second, errSecond := f.m.SelectAuth(context.Background(), "claude", seatOpus, opts)
			if errSecond != nil || second.ID != first.ID {
				t.Fatalf("configured %s identity lost its seat: %v %v", scenario, second, errSecond)
			}
		})
	}
}

func TestClaudeSeatRoutingAvailabilityOverridesLegacyRebindMode(t *testing.T) {
	for _, mode := range []string{SessionAffinityRebindOnNever, SessionAffinityRebindOnQuota} {
		t.Run(mode, func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, RebindOn: mode})
			t.Cleanup(selector.Stop)
			f := newSeatRoutingFixture(t, selector)
			first := f.request(t, "required-failover", seatOpus, "", "execute")
			f.failSeat = first
			second := f.request(t, "required-failover", seatOpus, "", "execute")
			if second == first {
				t.Fatal("legacy mode refused the available alternative")
			}
			f.failSeat = ""
			if got := f.request(t, "required-failover", seatOpus, "", "execute"); got != second {
				t.Fatal("legacy mode restored the failed seat instead of retaining the new cache")
			}
		})
	}
}
