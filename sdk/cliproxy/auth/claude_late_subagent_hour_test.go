package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// These tests run the seat router and the real Claude executor together, so
// they show both halves of the late subagent hour: the hold the router gives a
// subagent conversation and the cache lifetime its request carries upstream.

const (
	lateSubagentModel     = "claude-opus-4-6"
	lateSubagentJSONReply = `{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	lateSubagentSSEReply  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// lateSubagentSent is one request as the fake upstream received it.
type lateSubagentSent struct {
	seat string
	body []byte
	beta string
}

type lateSubagentHub struct {
	m     *coreauth.Manager
	hook  *logtest.Hook
	now   time.Time
	mu    sync.Mutex
	sent  []lateSubagentSent
	seats map[string]string
}

// newLateSubagentHub builds a hub from config YAML with two subscription seats
// that answer from one fake upstream, and puts the seat router on a clock the
// test moves.
func newLateSubagentHub(t *testing.T, configYAML string) *lateSubagentHub {
	t.Helper()
	cfg, errParse := config.ParseConfigBytes([]byte(configYAML))
	if errParse != nil {
		t.Fatalf("parse config: %v", errParse)
	}
	hub := &lateSubagentHub{now: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC), seats: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hub.mu.Lock()
		hub.sent = append(hub.sent, lateSubagentSent{
			seat: hub.seats[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")],
			body: bytes.Clone(body),
			beta: r.Header.Get("Anthropic-Beta"),
		})
		hub.mu.Unlock()
		if ojson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, lateSubagentSSEReply)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, lateSubagentJSONReply)
	}))
	t.Cleanup(server.Close)

	hub.m = coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	hub.m.SetRetryConfig(0, 0, 0)
	hub.m.SetConfig(cfg)
	hub.m.RegisterExecutor(executor.NewClaudeExecutor(cfg))
	coreauth.SetClaudeSeatClockForTest(hub.m, func() time.Time { return hub.now })
	for _, suffix := range []string{"a", "b"} {
		id := t.Name() + "-" + suffix
		token := "sk-ant-oat-late-subagent-fixture-" + suffix
		hub.seats[token] = id
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: lateSubagentModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := hub.m.Register(context.Background(), &coreauth.Auth{
			ID: id, Provider: "claude", Status: coreauth.StatusActive,
			Attributes: map[string]string{"api_key": token, "base_url": server.URL, "cloak_mode": "never"},
			// A known account and device pool keep the executor from looking the
			// seat's profile up anywhere: only the fake upstream is ever called.
			Metadata: map[string]any{
				"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				claudeauth.ClaudeDeviceIDsMetadataKey: []string{
					"0000000000000000000000000000000000000000000000000000000000000000",
				},
			},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hub.hook = logtest.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previousHooks) })
	return hub
}

// request sends one turn of a conversation through the manager and returns
// what the upstream received and the routing reason the router logged. An
// agent id makes the conversation a Claude Code subagent of session root.
func (h *lateSubagentHub) request(t *testing.T, path, agent string) (lateSubagentSent, string) {
	t.Helper()
	payload := []byte(`{"model":"` + lateSubagentModel + `","max_tokens":1024,` +
		`"system":[{"type":"text","text":"Project rules.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"next question","cache_control":{"type":"ephemeral"}}]}]}`)
	headers := http.Header{
		"User-Agent":               {"custom-agent/1.0"},
		"Anthropic-Beta":           {"oauth-2025-04-20"},
		"X-Claude-Code-Session-Id": {"late-subagent-root"},
	}
	if agent != "" {
		headers.Set("X-Claude-Code-Agent-Id", agent)
	}
	opts := cliproxyexecutor.Options{Stream: path == "stream", SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
		Headers: headers, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "late-subagent-scope"}}
	req := cliproxyexecutor.Request{Model: lateSubagentModel, Payload: payload}
	before := len(h.hook.AllEntries())
	if path == "stream" {
		result, errStream := h.m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
		if errStream != nil {
			t.Fatal(errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
	} else if _, errExecute := h.m.Execute(context.Background(), []string{"claude"}, req, opts); errExecute != nil {
		t.Fatal(errExecute)
	}
	reason := ""
	for _, entry := range h.hook.AllEntries()[before:] {
		if entry.Message == "claude routing: request" {
			reason, _ = entry.Data["reason"].(string)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) == 0 {
		t.Fatal("nothing reached the upstream")
	}
	return h.sent[len(h.sent)-1], reason
}

// lateSubagentTTLs lists the ttl of every cache marker on the system blocks and
// messages of a sent body, "" for a marker that names none.
func lateSubagentTTLs(body []byte) []string {
	var ttls []string
	collect := func(block ojson.Result) {
		if control := block.Get("cache_control"); control.Exists() {
			ttls = append(ttls, control.Get("ttl").String())
		}
	}
	for _, block := range ojson.GetBytes(body, "system").Array() {
		collect(block)
	}
	for _, message := range ojson.GetBytes(body, "messages").Array() {
		for _, block := range message.Get("content").Array() {
			collect(block)
		}
	}
	return ttls
}

func lateSubagentAllTTL(body []byte, want string) bool {
	ttls := lateSubagentTTLs(body)
	for _, ttl := range ttls {
		if ttl != want {
			return false
		}
	}
	return len(ttls) > 0
}

const lateSubagentKeyOn = "claude-header-defaults:\n  late-subagent-hour: true\n"

// A1: with the key on, a subagent conversation that comes back after its
// five-minute hold expired is sent at the hour, with the beta the hour needs,
// and is held for that hour.
func TestClaudeLateSubagentGetsTheHourAfterItsHoldExpired(t *testing.T) {
	for _, path := range []string{"execute", "stream"} {
		t.Run(path, func(t *testing.T) {
			hub := newLateSubagentHub(t, lateSubagentKeyOn)
			first, _ := hub.request(t, path, "late-child")
			if !lateSubagentAllTTL(first.body, "") {
				t.Fatalf("first request: marker ttls %v, want the five minutes as the client wrote them", lateSubagentTTLs(first.body))
			}
			hub.now = hub.now.Add(10 * time.Minute)
			late, reason := hub.request(t, path, "late-child")
			if reason != "cache_expired" {
				t.Fatalf("return after ten minutes: reason %q, want cache_expired (the subagent's five-minute hold is over)", reason)
			}
			if !lateSubagentAllTTL(late.body, "1h") {
				t.Fatalf("late return: marker ttls %v, want 1h on every marker", lateSubagentTTLs(late.body))
			}
			if !strings.Contains(late.beta, "extended-cache-ttl-2025-04-11") {
				t.Fatalf("late return: Anthropic-Beta %q lacks extended-cache-ttl-2025-04-11", late.beta)
			}
			hub.now = hub.now.Add(30 * time.Minute)
			next, reason := hub.request(t, path, "late-child")
			if reason != "cache_warm" || next.seat != late.seat {
				t.Fatalf("return thirty minutes after the late one: reason %q on %s, want cache_warm on %s", reason, next.seat, late.seat)
			}
			if !lateSubagentAllTTL(next.body, "1h") {
				t.Fatalf("after the mark: marker ttls %v, want 1h on every marker", lateSubagentTTLs(next.body))
			}
		})
	}
}
