package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	seatHoldBare      = `{"type":"ephemeral"}`
	seatHoldExplicit5 = `{"type":"ephemeral","ttl":"5m"}`
)

// seatHoldClock drives the router's own clock, so no test sleeps.
type seatHoldClock struct{ now time.Time }

func (c *seatHoldClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newSeatHoldFixture(t *testing.T, subscription bool) (*seatRoutingFixture, *seatHoldClock) {
	t.Helper()
	f := newSeatRoutingFixture(t, &RoundRobinSelector{})
	clock := &seatHoldClock{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	f.m.claudeSeats.now = func() time.Time { return clock.now }
	if subscription {
		for _, id := range f.ids {
			auth, _ := f.m.GetByID(id)
			auth.Metadata["access_token"] = "sk-ant-oat-seat-hold-fixture"
			if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
		}
	}
	return f, clock
}

func setSeatHoldKey(f *seatRoutingFixture, on bool) {
	cfg := &config.Config{}
	cfg.Claude.Quota.Enabled = true
	cfg.Claude.Quota.PriorityAtReset = true
	cfg.ClaudeHeaderDefaults.OneHourCache = &on
	f.m.SetConfig(cfg)
}

func (f *seatRoutingFixture) requestMarker(t *testing.T, client, marker string) (seat, reason string) {
	t.Helper()
	payload := []byte(`{"model":"` + seatOpus + `","system":[{"type":"text","text":"private prompt","cache_control":` + marker + `}],"messages":[{"role":"user","content":"hello"}]}`)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
		Headers:  http.Header{"X-Session-Id": []string{client}},
		Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "shared-access-key-scope"}}
	response, errExecute := f.m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: seatOpus, Payload: payload}, opts)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	requests := f.events("claude routing: request")
	if len(requests) == 0 {
		t.Fatal("no routing request logged")
	}
	reason, _ = requests[len(requests)-1].Data["reason"].(string)
	return string(response.Payload), reason
}

func TestClaudeOneHourCacheKeyDefaultsOn(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		cfg  *config.Config
		want bool
	}{
		{nil, true},
		{&config.Config{}, true},
		{&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OneHourCache: &on}}, true},
		{&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OneHourCache: &off}}, false},
	} {
		if got := ClaudeOneHourCacheEnabled(tc.cfg); got != tc.want {
			t.Fatalf("ClaudeOneHourCacheEnabled(%+v) = %v, want %v", tc.cfg, got, tc.want)
		}
	}
}

// Test 2a: on a subscription seat with the key on, a bare marker holds the seat
// for the hour the executor gives it upstream.
func TestClaudeSeatHoldMatchesOneHourRule(t *testing.T) {
	f, clock := newSeatHoldFixture(t, true)
	first, _ := f.requestMarker(t, "hold", seatHoldBare)
	clock.advance(10 * time.Minute)
	seat, reason := f.requestMarker(t, "hold", seatHoldBare)
	if seat != first || reason != "cache_warm" {
		t.Fatalf("ten-minute return: seat=%s reason=%s, want %s cache_warm", seat, reason, first)
	}
	clock.advance(time.Hour + time.Second)
	if _, reason = f.requestMarker(t, "hold", seatHoldBare); reason != "cache_expired" {
		t.Fatalf("return after more than an hour: reason=%s, want cache_expired", reason)
	}
}

// Guards for 2a: the key off, a key seat, or an explicit 5m hold five minutes as before.
func TestClaudeSeatHoldKeepsFiveMinutesOutsideTheRule(t *testing.T) {
	for _, tc := range []struct {
		name         string
		subscription bool
		keyOff       bool
		marker       string
	}{
		{name: "key off", subscription: true, keyOff: true, marker: seatHoldBare},
		{name: "key seat", marker: seatHoldBare},
		{name: "explicit five minutes", subscription: true, marker: seatHoldExplicit5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, clock := newSeatHoldFixture(t, tc.subscription)
			if tc.keyOff {
				setSeatHoldKey(f, false)
			}
			f.requestMarker(t, "hold", tc.marker)
			clock.advance(10 * time.Minute)
			if _, reason := f.requestMarker(t, "hold", tc.marker); reason != "cache_expired" {
				t.Fatalf("ten-minute return: reason=%s, want cache_expired", reason)
			}
		})
	}
}

// The router reads the key from the manager's runtime config, so a reload
// changes the hold without a restart. A rebind never shortens a hold already
// granted: the hour written before the key went off stays held until it lapses.
func TestClaudeSeatHoldFollowsKeyReload(t *testing.T) {
	f, clock := newSeatHoldFixture(t, true)
	f.requestMarker(t, "reload", seatHoldBare)
	setSeatHoldKey(f, false)
	clock.advance(time.Minute)
	if _, reason := f.requestMarker(t, "reload", seatHoldBare); reason != "cache_warm" {
		t.Fatalf("return inside the hour already granted: reason=%s, want cache_warm", reason)
	}
	clock.advance(time.Hour)
	f.requestMarker(t, "reload", seatHoldBare)
	clock.advance(10 * time.Minute)
	if _, reason := f.requestMarker(t, "reload", seatHoldBare); reason != "cache_expired" {
		t.Fatalf("with the key off a new hold is five minutes: reason=%s, want cache_expired", reason)
	}
	setSeatHoldKey(f, true)
	f.requestMarker(t, "reload", seatHoldBare)
	clock.advance(10 * time.Minute)
	if _, reason := f.requestMarker(t, "reload", seatHoldBare); reason != "cache_warm" {
		t.Fatalf("after the key came back on: reason=%s, want cache_warm", reason)
	}
}

// The router hands the executor the client hash it logs, on the execute and
// the stream path.
func TestClaudeSeatRoutingPassesClientHashToExecutor(t *testing.T) {
	for _, path := range []string{"execute", "stream"} {
		t.Run(path, func(t *testing.T) {
			f := newSeatRoutingFixture(t, &RoundRobinSelector{})
			var seen []string
			execute := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				hash, _ := opts.Metadata[cliproxyexecutor.ClaudeSeatClientHashMetadataKey].(string)
				seen = append(seen, hash)
				return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
			}
			f.m.RegisterExecutor(&customStreamMockExecutor{
				identifier:              "claude",
				mockCustomErrorExecutor: mockCustomErrorExecutor{identifier: "claude", executeFn: execute, countFn: execute},
				streamFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					response, _ := execute(ctx, auth, req, opts)
					chunks := make(chan cliproxyexecutor.StreamChunk, 1)
					chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				},
			})
			f.request(t, "hash", seatOpus, "", path)
			requests := f.events("claude routing: request")
			if len(requests) != 1 || len(seen) != 1 {
				t.Fatalf("requests logged=%d executed=%d, want 1 and 1", len(requests), len(seen))
			}
			if logged, _ := requests[0].Data["client_hash"].(string); logged == "" || seen[0] != logged {
				t.Fatalf("executor saw client hash %q, routing log says %q", seen[0], logged)
			}
		})
	}
}
