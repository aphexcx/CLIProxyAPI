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

// lateSubagentKeyOn puts the late subagent key on for a fixture and records
// the late subagent hour each executed request received.
func lateSubagentKeyOn(f *seatRoutingFixture) *[]any {
	cfg := &config.Config{}
	cfg.Claude.Quota.Enabled = true
	cfg.Claude.Quota.PriorityAtReset = true
	cfg.ClaudeHeaderDefaults.LateSubagentHour = true
	f.m.SetConfig(cfg)
	seen := &[]any{}
	execute := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		f.attempts = append(f.attempts, auth.ID)
		if f.failSeat == auth.ID {
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "fake upstream unavailable"}
		}
		*seen = append(*seen, opts.Metadata[cliproxyexecutor.ClaudeLateSubagentHourMetadataKey])
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	f.m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude",
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
		}})
	return seen
}

func lateSubagentRouting(t *testing.T, subscription bool) (*seatRoutingFixture, *seatHoldClock, *[]any) {
	t.Helper()
	f, clock := newSeatHoldFixture(t, subscription)
	return f, clock, lateSubagentKeyOn(f)
}

// seatHoldSubscriptionSeats makes a fixture's seats subscription seats, as
// newSeatHoldFixture does, for a manager that restartSeatHolds builds.
func seatHoldSubscriptionSeats(t *testing.T, f *seatRoutingFixture) {
	t.Helper()
	for _, id := range f.ids {
		auth, _ := f.m.GetByID(id)
		auth.Metadata["access_token"] = "sk-ant-oat-seat-hold-fixture"
		if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
}

// lateTurn is one turn of a test conversation on session late-root. agent
// makes it a subagent by header, system replaces its first system text, and
// callerHour puts a late subagent hour into the caller's own metadata.
type lateTurn struct {
	path       string
	agent      string
	system     string
	callerHour bool
}

// send sends the turn with bare markers and returns the routing reason.
func (f *seatRoutingFixture) send(t *testing.T, turn lateTurn) string {
	t.Helper()
	system := turn.system
	if system == "" {
		system = "private prompt"
	}
	payload := []byte(`{"model":"` + seatOpus + `","system":[{"type":"text","text":"` + system + `","cache_control":` + seatHoldBare + `}],"messages":[{"role":"user","content":"hello"}]}`)
	headers := http.Header{"X-Claude-Code-Session-Id": {"late-root"}}
	if turn.agent != "" {
		headers.Set("X-Claude-Code-Agent-Id", turn.agent)
	}
	metadata := map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "shared-access-key-scope"}
	if turn.callerHour {
		metadata[cliproxyexecutor.ClaudeLateSubagentHourMetadataKey] = true
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers, Metadata: metadata}
	req := cliproxyexecutor.Request{Model: seatOpus, Payload: payload}
	if turn.path == "stream" {
		stream, errStream := f.m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
		if errStream != nil {
			t.Fatal(errStream)
		}
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
	} else if _, errExecute := f.m.Execute(context.Background(), []string{"claude"}, req, opts); errExecute != nil {
		t.Fatal(errExecute)
	}
	requests := f.events("claude routing: request")
	reason, _ := requests[len(requests)-1].Data["reason"].(string)
	return reason
}

func lateSubagentSeen(t *testing.T, seen *[]any, want ...any) {
	t.Helper()
	if len(*seen) != len(want) {
		t.Fatalf("executed %d requests, want %d (late subagent hours %v)", len(*seen), len(want), *seen)
	}
	for i := range want {
		if (*seen)[i] != want[i] {
			t.Fatalf("request %d: late subagent hour %v, want %v (all: %v)", i+1, (*seen)[i], want[i], *seen)
		}
	}
}

// Only the router's own mark reaches the executor: one a caller's metadata
// already holds is dropped, for a main conversation and an unmarked subagent.
func TestClaudeLateSubagentHourComesOnlyFromTheRouter(t *testing.T) {
	for _, path := range []string{"execute", "stream"} {
		t.Run(path, func(t *testing.T) {
			f, clock, seen := lateSubagentRouting(t, true)
			f.send(t, lateTurn{path: path, callerHour: true})
			f.send(t, lateTurn{path: path, agent: "late-child", callerHour: true})
			clock.advance(10 * time.Minute)
			if reason := f.send(t, lateTurn{path: path, agent: "late-child"}); reason != "cache_expired" {
				t.Fatalf("late return: reason %q, want cache_expired", reason)
			}
			lateSubagentSeen(t, seen, nil, nil, true)
		})
	}
}

// On a key seat the one-hour rule does not apply, so with the key on a late
// subagent keeps today's five-minute hold and is never sent at the hour.
func TestClaudeLateSubagentHourNeedsASubscriptionSeat(t *testing.T) {
	f, clock, seen := lateSubagentRouting(t, false)
	f.send(t, lateTurn{agent: "late-child"})
	for _, gap := range []time.Duration{10 * time.Minute, 30 * time.Minute} {
		clock.advance(gap)
		if reason := f.send(t, lateTurn{agent: "late-child"}); reason != "cache_expired" {
			t.Fatalf("return after %s: reason %q, want cache_expired", gap, reason)
		}
	}
	lateSubagentSeen(t, seen, nil, nil, nil)
}

// The mark is saved with the hold: a marked subagent whose hold comes back
// from the hold file after a restart is still held, and sent, at the hour.
func TestClaudeLateSubagentHourSurvivesRestart(t *testing.T) {
	path := seatHoldFilePath(t)
	before, clock, _ := lateSubagentRouting(t, true)
	before.m.StartClaudeSeatHolds(context.Background(), path)
	t.Cleanup(before.m.StopClaudeSeatHolds)
	before.send(t, lateTurn{agent: "late-child"})
	clock.advance(10 * time.Minute)
	if reason := before.send(t, lateTurn{agent: "late-child"}); reason != "cache_expired" {
		t.Fatalf("late return: reason %q, want cache_expired", reason)
	}
	clock.advance(time.Minute)
	var seen *[]any
	after := restartSeatHolds(t, before, clock, path, 11*time.Second, func(f *seatRoutingFixture) {
		seatHoldSubscriptionSeats(t, f)
		seen = lateSubagentKeyOn(f)
	})
	if reason := after.send(t, lateTurn{agent: "late-child"}); reason != "cache_warm" {
		t.Fatalf("after the restart: reason %q, want cache_warm", reason)
	}
	lateSubagentSeen(t, seen, true)
}

// A subagent the session code files under its main conversation's own session
// (a subagent flag with no agent id) shares that conversation's hold, so the
// rule leaves it alone: when the shared seat fails during the subagent's turn
// and the hold moves, the main conversation is still held for its hour there.
func TestClaudeLateSubagentHourLeavesASharedSessionAlone(t *testing.T) {
	f, clock, seen := lateSubagentRouting(t, true)
	sharedSubagent := lateTurn{system: "x-anthropic-billing-header: cc_is_subagent=true;"}
	f.send(t, lateTurn{})
	f.failSeat = f.attempts[0]
	if reason := f.send(t, sharedSubagent); reason != "upstream_error" {
		t.Fatalf("shared subagent on a failing seat: reason %q, want upstream_error", reason)
	}
	f.failSeat = ""
	clock.advance(10 * time.Minute)
	if reason := f.send(t, lateTurn{}); reason != "cache_warm" {
		t.Fatalf("main conversation ten minutes after the move: reason %q, want cache_warm", reason)
	}
	lateSubagentSeen(t, seen, nil, nil, nil)
}
