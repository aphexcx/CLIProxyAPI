package executor

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// These tests cover the executor's half of the late subagent hour: a native
// Claude Code subagent request the seat router marks (it came back after its
// five-minute hold expired, claude-header-defaults.late-subagent-hour) gets the
// one-hour rule a main conversation gets, and nothing else changes.

func lateSubagentHeaders() http.Header {
	headers := oneHourNativeHeaders()
	headers.Set("X-Claude-Code-Agent-Id", "agent-late-subagent")
	return headers
}

func lateSubagentMark(hour bool) map[string]any {
	return map[string]any{cliproxyexecutor.ClaudeLateSubagentHourMetadataKey: hour}
}

// lateSubagentConfig is a config with claude-header-defaults.late-subagent-hour
// on; the executor honours the router's mark only while it is.
func lateSubagentConfig() *config.Config {
	return &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{LateSubagentHour: true}}
}

// A marked subagent on a subscription seat: its bare markers go upstream at the
// hour with the beta the hour needs, marker for marker as a main conversation's.
func TestClaudeLateSubagentHourSendsMarkedSubagentAtTheHour(t *testing.T) {
	payload := oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			main, _, errMain := runOneHourCall(t, oneHourCall{stream: stream, payload: payload})
			if errMain != nil {
				t.Fatalf("main conversation: %v", errMain)
			}
			late, _, errLate := runOneHourCall(t, oneHourCall{cfg: lateSubagentConfig(), stream: stream, headers: lateSubagentHeaders(), metadata: lateSubagentMark(true), payload: payload})
			if errLate != nil {
				t.Fatalf("late subagent: %v", errLate)
			}
			markers := oneHourMarkers(t, late.body(0))
			for _, label := range []string{"tool:Read", "system:Project rules.", "message:second question"} {
				if got := oneHourMarkerTTL(markers[label]); got != "1h" {
					t.Fatalf("%s ttl = %q, want 1h; markers=%v", label, got, markers)
				}
			}
			if want := oneHourMarkers(t, main.body(0)); !reflect.DeepEqual(markers, want) {
				t.Fatalf("late subagent markers %v, want the main conversation's %v", markers, want)
			}
			if !oneHourBeta(late.header(0)) {
				t.Fatalf("Anthropic-Beta = %q, want %s", late.header(0).Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta)
			}
		})
	}
}

// Guards: a mark with the key off, or one the one-hour rule cannot honour,
// leaves the subagent request as the client wrote it, at five minutes and
// without the beta.
func TestClaudeLateSubagentHourLeavesOtherSubagentRequestsUnchanged(t *testing.T) {
	ruleOff := false
	payload := oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")
	cases := []struct {
		name string
		call oneHourCall
	}{
		{name: "key off", call: oneHourCall{metadata: lateSubagentMark(true), payload: payload}},
		{name: "no mark", call: oneHourCall{cfg: lateSubagentConfig(), payload: payload}},
		{name: "mark false", call: oneHourCall{cfg: lateSubagentConfig(), metadata: lateSubagentMark(false), payload: payload}},
		{name: "key seat", call: oneHourCall{cfg: lateSubagentConfig(), apiKey: oneHourKeySeatKey, metadata: lateSubagentMark(true), payload: payload}},
		{name: "one-hour-cache off", call: oneHourCall{cfg: &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OneHourCache: &ruleOff, LateSubagentHour: true}},
			metadata: lateSubagentMark(true), payload: payload}},
		{name: "explicit cache mode", call: oneHourCall{cfg: lateSubagentConfig(), metadata: lateSubagentMark(true),
			payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", `"prompt_cache_options":{"mode":"explicit"},`)}},
		{name: "token counting", call: oneHourCall{cfg: lateSubagentConfig(), count: true, metadata: lateSubagentMark(true), payload: payload}},
	}
	for _, tc := range cases {
		for _, stream := range oneHourPaths {
			if tc.call.count && stream {
				continue
			}
			t.Run(tc.name+"/"+oneHourPathName(stream), func(t *testing.T) {
				call := tc.call
				call.stream = stream
				call.headers = lateSubagentHeaders()
				upstream, _, err := runOneHourCall(t, call)
				if err != nil {
					t.Fatalf("request error = %v", err)
				}
				markers := oneHourMarkers(t, upstream.body(0))
				if len(markers) == 0 {
					t.Fatalf("no cache marker reached upstream: %s", upstream.body(0))
				}
				for label, raw := range markers {
					if got := oneHourMarkerTTL(raw); got != "" {
						t.Fatalf("%s ttl = %q, want none; markers=%v", label, got, markers)
					}
				}
				if oneHourBeta(upstream.header(0)) {
					t.Fatalf("extended-cache-ttl beta sent: %q", upstream.header(0).Get("Anthropic-Beta"))
				}
			})
		}
	}
}

// The payload rules stay the last word: a rule that sets or removes a marker's
// ttl wins over the late subagent hour in the body that is sent.
func TestClaudeLateSubagentHourYieldsToPayloadRules(t *testing.T) {
	cfg := lateSubagentConfig()
	cfg.Payload = config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "claude-*"}}, Params: map[string]any{"system.1.cache_control.ttl": "5m"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "claude-*"}}, Params: []string{"messages.2.content.0.cache_control.ttl"}}},
	}
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{cfg: cfg, stream: stream, headers: lateSubagentHeaders(), metadata: lateSubagentMark(true),
				payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			body := upstream.body(0)
			if got := ojson.GetBytes(body, "system.1.cache_control.ttl").String(); got != "5m" {
				t.Fatalf("payload override lost: system.1 ttl = %q; body=%s", got, body)
			}
			if ojson.GetBytes(body, "messages.2.content.0.cache_control.ttl").Exists() {
				t.Fatalf("payload filter lost: message marker still carries a ttl; body=%s", body)
			}
			if got := oneHourMarkerTTL(oneHourMarkers(t, body)["tool:Read"]); got != "1h" {
				t.Fatalf("tool marker untouched by the payload rules ttl = %q, want 1h; body=%s", got, body)
			}
		})
	}
}

// A refusal of the hour on a marked subagent is answered as on a main
// conversation: one re-send as the client wrote it.
func TestClaudeLateSubagentHourRefusalResendsAsWritten(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{cfg: lateSubagentConfig(), stream: stream, headers: lateSubagentHeaders(), metadata: lateSubagentMark(true),
				payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", ""), refuse: oneHourRefuseHour})
			if err != nil {
				t.Fatalf("caller got error %v, want the re-sent request's 200", err)
			}
			if got := upstream.count(); got != 2 {
				t.Fatalf("upstream saw %d requests, want 2 (refused, then re-sent)", got)
			}
			if strings.Contains(string(upstream.body(1)), `"ttl"`) {
				t.Fatalf("re-sent body keeps a ttl the rule added: %s", upstream.body(1))
			}
		})
	}
}

// A Thread on a subscription seat declares extended-cache-ttl from its first
// turn, subagent or not, so a subagent marked after its thread began is sent at
// the hour inside that thread, and its fixed betas still cover the body.
func TestClaudeLateSubagentHourInsideAThread(t *testing.T) {
	server, upstream := newOneHourUpstream(t, nil)
	executor := NewClaudeExecutor(lateSubagentConfig())
	auth := &cliproxyauth.Auth{ID: "late-subagent-" + t.Name(), Provider: "claude", Metadata: claudeOAuthTestMetadata(),
		Attributes: map[string]string{"api_key": oneHourSubscriptionKey, "base_url": server.URL}}
	headers := lateSubagentHeaders()
	headers.Set("Anthropic-Beta", headers.Get("Anthropic-Beta")+",message-threads-2026-08-12")
	send := func(thread string, metadata map[string]any) error {
		payload := oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", `"thread":`+thread+`,`)
		_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: oneHourModel, Payload: payload},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers, Metadata: metadata})
		return errExecute
	}
	if errCreate := send(`{"type":"create"}`, nil); errCreate != nil {
		t.Fatalf("thread create: %v", errCreate)
	}
	if errContinue := send(`{"type":"continue","previous_message_id":"msg_1"}`, lateSubagentMark(true)); errContinue != nil {
		t.Fatalf("marked continuation: %v, want it sent", errContinue)
	}
	if got := upstream.count(); got != 2 {
		t.Fatalf("upstream saw %d requests, want 2", got)
	}
	for i, wantTTL := range []string{"", "1h"} {
		for label, raw := range oneHourMarkers(t, upstream.body(i)) {
			if got := oneHourMarkerTTL(raw); got != wantTTL {
				t.Fatalf("request %d: %s ttl = %q, want %q", i+1, label, got, wantTTL)
			}
		}
		if !oneHourBeta(upstream.header(i)) {
			t.Fatalf("request %d: Anthropic-Beta = %q lacks %s", i+1, upstream.header(i).Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta)
		}
	}
}
