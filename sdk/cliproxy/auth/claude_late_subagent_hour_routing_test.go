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

// lateSubagentRouting puts the late subagent key on for a seat hold fixture and
// records the late subagent hour each executed request received.
func lateSubagentRouting(t *testing.T, subscription bool) (*seatRoutingFixture, *seatHoldClock, *[]any) {
	t.Helper()
	f, clock := newSeatHoldFixture(t, subscription)
	cfg := &config.Config{}
	cfg.Claude.Quota.Enabled = true
	cfg.Claude.Quota.PriorityAtReset = true
	cfg.ClaudeHeaderDefaults.LateSubagentHour = true
	f.m.SetConfig(cfg)
	seen := &[]any{}
	execute := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		*seen = append(*seen, opts.Metadata[cliproxyexecutor.ClaudeLateSubagentHourMetadataKey])
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	f.m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude",
		mockCustomErrorExecutor: mockCustomErrorExecutor{identifier: "claude", executeFn: execute, countFn: execute}})
	return f, clock, seen
}

// lateSubagentTurn sends one turn with bare markers; agent makes it a subagent,
// and callerHour puts a late subagent hour into the caller's own metadata.
func (f *seatRoutingFixture) lateSubagentTurn(t *testing.T, agent string, callerHour bool) string {
	t.Helper()
	payload := []byte(`{"model":"` + seatOpus + `","system":[{"type":"text","text":"private prompt","cache_control":` + seatHoldBare + `}],"messages":[{"role":"user","content":"hello"}]}`)
	headers := http.Header{"X-Claude-Code-Session-Id": {"late-root"}}
	if agent != "" {
		headers.Set("X-Claude-Code-Agent-Id", agent)
	}
	metadata := map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "shared-access-key-scope"}
	if callerHour {
		metadata[cliproxyexecutor.ClaudeLateSubagentHourMetadataKey] = true
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers, Metadata: metadata}
	if _, errExecute := f.m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: seatOpus, Payload: payload}, opts); errExecute != nil {
		t.Fatal(errExecute)
	}
	requests := f.events("claude routing: request")
	reason, _ := requests[len(requests)-1].Data["reason"].(string)
	return reason
}

// Only the router's own mark reaches the executor: one a caller's metadata
// already holds is dropped, for a main conversation and an unmarked subagent.
func TestClaudeLateSubagentHourComesOnlyFromTheRouter(t *testing.T) {
	f, clock, seen := lateSubagentRouting(t, true)
	f.lateSubagentTurn(t, "", true)
	f.lateSubagentTurn(t, "late-child", true)
	clock.advance(10 * time.Minute)
	if reason := f.lateSubagentTurn(t, "late-child", false); reason != "cache_expired" {
		t.Fatalf("late return: reason %q, want cache_expired", reason)
	}
	want := []any{nil, nil, true}
	if len(*seen) != len(want) {
		t.Fatalf("executed %d requests, want %d", len(*seen), len(want))
	}
	for i := range want {
		if (*seen)[i] != want[i] {
			t.Fatalf("request %d: late subagent hour %v, want %v (all: %v)", i+1, (*seen)[i], want[i], *seen)
		}
	}
}

// On a key seat the one-hour rule does not apply, so with the key on a late
// subagent keeps today's five-minute hold and is never sent at the hour.
func TestClaudeLateSubagentHourNeedsASubscriptionSeat(t *testing.T) {
	f, clock, seen := lateSubagentRouting(t, false)
	f.lateSubagentTurn(t, "late-child", false)
	for _, gap := range []time.Duration{10 * time.Minute, 30 * time.Minute} {
		clock.advance(gap)
		if reason := f.lateSubagentTurn(t, "late-child", false); reason != "cache_expired" {
			t.Fatalf("return after %s: reason %q, want cache_expired", gap, reason)
		}
	}
	for i, hour := range *seen {
		if hour != nil {
			t.Fatalf("request %d carried the late subagent hour on a key seat", i+1)
		}
	}
}
