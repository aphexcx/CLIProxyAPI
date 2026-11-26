package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// These tests cover the one-hour cache rule: a confirmed native Claude Code main
// conversation on a subscription seat gets "ttl":"1h" on every cache marker it
// placed without a lifetime, and everything else keeps what the client sent.

const (
	oneHourSubscriptionKey = "sk-ant-oat-one-hour-rule-fixture"
	oneHourKeySeatKey      = "sk-ant-api03-one-hour-rule-fixture"
	oneHourModel           = "claude-opus-4-6"
	oneHourLifetimeRefusal = `{"type":"error","error":{"type":"invalid_request_error","message":"system.1.cache_control.ttl: the one-hour lifetime is not available"}}`
	oneHourOtherRefusal    = `{"type":"error","error":{"type":"invalid_request_error","message":"messages: text content blocks must be non-empty"}}`
	oneHourJSONReply       = `{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":40,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100}}}`
	oneHourSSEReply        = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"cache_creation_input_tokens\":100,\"cache_read_input_tokens\":40,\"cache_creation\":{\"ephemeral_5m_input_tokens\":0,\"ephemeral_1h_input_tokens\":100}}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// oneHourUserID and oneHourNativeHeaders reuse the measured native Claude Code
// fixture, so detection confirms the caller as native Claude Code.
const oneHourUserID = claudeNativeHelperUserID

const oneHourBare = `{"type":"ephemeral"}`

var oneHourPaths = []bool{false, true}

func oneHourPathName(stream bool) string {
	if stream {
		return "stream"
	}
	return "execute"
}

func oneHourNativeHeaders() http.Header {
	return claudeNativeHelperHeaders("claude-code-20250219,"+claudeNativeHelperCoreBetas, "identity")
}

func oneHourCacheField(raw string) string {
	if raw == "" {
		return ""
	}
	return `,"cache_control":` + raw
}

// oneHourPayload is a native main-conversation body with a marker slot on a
// tool, a system block, the last message and the top level.
func oneHourPayload(tool, system, message, top, extra string) []byte {
	topField := ""
	if top != "" {
		topField = `"cache_control":` + top + `,`
	}
	return []byte(`{"model":"` + oneHourModel + `","max_tokens":1024,` + topField + extra +
		`"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{}}` + oneHourCacheField(tool) + `}],` +
		`"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Project rules."` + oneHourCacheField(system) + `}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"first question"}]},{"role":"assistant","content":[{"type":"text","text":"first answer"}]},{"role":"user","content":[{"type":"text","text":"second question"` + oneHourCacheField(message) + `}]}],` +
		`"metadata":{"user_id":` + strconv.Quote(oneHourUserID) + `}}`)
}

// oneHourMarkers maps each cache marker of an upstream body to its raw JSON,
// keyed by the block that carries it, so block shifts (a billing header added to
// system) do not move the assertions.
func oneHourMarkers(t *testing.T, body []byte) map[string]string {
	t.Helper()
	markers := map[string]string{}
	if top := ojson.GetBytes(body, "cache_control"); top.Exists() {
		markers["top"] = top.Raw
	}
	for _, tool := range ojson.GetBytes(body, "tools").Array() {
		if cc := tool.Get("cache_control"); cc.Exists() {
			markers["tool:"+tool.Get("name").String()] = cc.Raw
		}
	}
	for _, block := range ojson.GetBytes(body, "system").Array() {
		if cc := block.Get("cache_control"); cc.Exists() {
			markers["system:"+block.Get("text").String()] = cc.Raw
		}
	}
	for _, message := range ojson.GetBytes(body, "messages").Array() {
		for _, block := range message.Get("content").Array() {
			if cc := block.Get("cache_control"); cc.Exists() {
				markers["message:"+block.Get("text").String()] = cc.Raw
			}
		}
	}
	return markers
}

func oneHourMarkerTTL(raw string) string {
	return ojson.Parse(raw).Get("ttl").String()
}

type oneHourUpstream struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
}

func (u *oneHourUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func (u *oneHourUpstream) body(i int) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[i]
}

func (u *oneHourUpstream) header(i int) http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[i]
}

// newOneHourUpstream answers like Anthropic. refuse, when it returns a non-empty
// error body, answers that request with HTTP 400.
func newOneHourUpstream(t *testing.T, refuse func(body []byte) string) (*httptest.Server, *oneHourUpstream) {
	t.Helper()
	upstream := &oneHourUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.bodies = append(upstream.bodies, bytes.Clone(body))
		upstream.headers = append(upstream.headers, r.Header.Clone())
		upstream.paths = append(upstream.paths, r.URL.Path)
		upstream.mu.Unlock()
		if refuse != nil {
			if errBody := refuse(body); errBody != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, errBody)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":5}`)
			return
		}
		if ojson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, oneHourSSEReply)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, oneHourJSONReply)
	}))
	t.Cleanup(server.Close)
	return server, upstream
}

type oneHourCall struct {
	cfg      *config.Config
	apiKey   string
	stream   bool
	count    bool
	headers  http.Header
	payload  []byte
	metadata map[string]any
	attrs    map[string]string
	refuse   func(body []byte) string
}

func runOneHourCall(t *testing.T, call oneHourCall) (*oneHourUpstream, *cliproxyauth.Auth, error) {
	t.Helper()
	server, upstream := newOneHourUpstream(t, call.refuse)
	cfg := call.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	apiKey := call.apiKey
	if apiKey == "" {
		apiKey = oneHourSubscriptionKey
	}
	headers := call.headers
	if headers == nil {
		headers = oneHourNativeHeaders()
	}
	auth := &cliproxyauth.Auth{
		ID:       "one-hour-" + t.Name(),
		Provider: "claude",
		Metadata: claudeOAuthTestMetadata(),
		Attributes: map[string]string{
			"api_key":  apiKey,
			"base_url": server.URL,
		},
	}
	for name, value := range call.attrs {
		auth.Attributes[name] = value
	}
	executor := NewClaudeExecutor(cfg)
	req := cliproxyexecutor.Request{Model: oneHourModel, Payload: call.payload}
	opts := cliproxyexecutor.Options{
		Stream:          call.stream,
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: call.payload,
		Headers:         headers,
		Metadata:        call.metadata,
	}
	switch {
	case call.count:
		// CountTokens estimates locally off Anthropic's own address; call the
		// upstream path directly so the test server sees the body.
		_, errCount := executor.countTokensUpstream(context.Background(), auth, req, opts)
		return upstream, auth, errCount
	case call.stream:
		result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
		if errStream != nil {
			return upstream, auth, errStream
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				return upstream, auth, chunk.Err
			}
		}
		return upstream, auth, nil
	default:
		_, errExecute := executor.Execute(context.Background(), auth, req, opts)
		return upstream, auth, errExecute
	}
}

func oneHourBeta(headers http.Header) bool {
	return strings.Contains(headers.Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta)
}

// Test 1: bare markers on a tool, a system block and the last message all go
// upstream at one hour, with the beta the body needs.
func TestClaudeOneHourCacheRuleUpgradesBareMarkersOnSubscriptionSeat(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			body := upstream.body(0)
			markers := oneHourMarkers(t, body)
			for _, label := range []string{"tool:Read", "system:Project rules.", "message:second question"} {
				if got := oneHourMarkerTTL(markers[label]); got != "1h" {
					t.Fatalf("%s ttl = %q, want 1h; markers=%v", label, got, markers)
				}
			}
			if !oneHourBeta(upstream.header(0)) {
				t.Fatalf("Anthropic-Beta = %q, want %s", upstream.header(0).Get("Anthropic-Beta"), claudeExtendedCacheTTLBeta)
			}
		})
	}
}

// A client that is not Claude Code, sending its own bare markers to a
// subscription seat that does not cloak it, gets the same rule.
func TestClaudeOneHourCacheRuleCoversOtherClientsOwnMarkers(t *testing.T) {
	headers := http.Header{"User-Agent": {"custom-agent/1.0"}, "Anthropic-Beta": {"oauth-2025-04-20"}}
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, headers: headers, attrs: map[string]string{"cloak_mode": "never"},
				payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			markers := oneHourMarkers(t, upstream.body(0))
			for _, label := range []string{"tool:Read", "system:Project rules.", "message:second question"} {
				if got := oneHourMarkerTTL(markers[label]); got != "1h" {
					t.Fatalf("%s ttl = %q, want 1h; markers=%v", label, got, markers)
				}
			}
		})
	}
}

// Test 2: an explicit 5m is the caller's choice. Markers before it get the hour,
// it stays, and the order rule keeps any later marker off the hour.
func TestClaudeOneHourCacheRuleKeepsExplicitFiveMinutes(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: oneHourPayload(oneHourBare, `{"type":"ephemeral","ttl":"5m"}`, oneHourBare, "", "")})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			markers := oneHourMarkers(t, upstream.body(0))
			if got := oneHourMarkerTTL(markers["tool:Read"]); got != "1h" {
				t.Fatalf("tool ttl = %q, want 1h; markers=%v", got, markers)
			}
			if got := oneHourMarkerTTL(markers["system:Project rules."]); got != "5m" {
				t.Fatalf("explicit 5m marker ttl = %q, want 5m kept; markers=%v", got, markers)
			}
			if got := oneHourMarkerTTL(markers["message:second question"]); got == "1h" {
				t.Fatalf("marker after an explicit 5m carries 1h; markers=%v", markers)
			}
		})
	}
}

// Test 2, second half: a bare marker keeps every key the caller wrote, on a
// block and at the top level.
func TestClaudeOneHourCacheRuleKeepsCallerMarkerKeys(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			payload := oneHourPayload("", `{"type":"ephemeral","scope":"global","evict_on_complete":true}`, "", `{"type":"ephemeral","evict_on_complete":true}`, "")
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: payload})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			markers := oneHourMarkers(t, upstream.body(0))
			if got, want := markers["system:Project rules."], `{"type":"ephemeral","ttl":"1h","scope":"global","evict_on_complete":true}`; got != want {
				t.Fatalf("system marker = %s, want %s", got, want)
			}
			if got, want := markers["top"], `{"type":"ephemeral","ttl":"1h","evict_on_complete":true}`; got != want {
				t.Fatalf("top-level marker = %s, want %s", got, want)
			}
		})
	}
}

// Test 3: the payload rules are the last word. A rule that sets or removes a
// marker's ttl wins over the one-hour rule in the body that is sent.
func TestClaudeOneHourCacheRuleYieldsToPayloadRules(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "claude-*"}}, Params: map[string]any{"system.1.cache_control.ttl": "5m"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "claude-*"}}, Params: []string{"messages.2.content.0.cache_control.ttl"}}},
	}}
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{cfg: cfg, stream: stream, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")})
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

type oneHourUsageCapture struct {
	mu       sync.Mutex
	authID   string
	records  []usage.Record
	sentinel chan struct{}
}

func (c *oneHourUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID == c.authID+"-sentinel" {
		close(c.sentinel)
		return
	}
	if record.AuthID != c.authID {
		return
	}
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
}

type oneHourUsageNoop struct{}

func (oneHourUsageNoop) HandleUsage(context.Context, usage.Record) {}

func newOneHourUsageCapture(t *testing.T, authID string) *oneHourUsageCapture {
	t.Helper()
	capture := &oneHourUsageCapture{authID: authID, sentinel: make(chan struct{})}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), oneHourUsageNoop{}) })
	return capture
}

// settled returns the records published for the auth so far. The usage queue
// is first-in first-out, so once a sentinel published after the request is
// seen, every record of the request has been delivered.
func (c *oneHourUsageCapture) settled(t *testing.T) []usage.Record {
	t.Helper()
	usage.PublishRecord(context.Background(), usage.Record{AuthID: c.authID + "-sentinel", Provider: "one-hour-sentinel"})
	select {
	case <-c.sentinel:
	case <-time.After(10 * time.Second):
		t.Fatal("usage queue did not deliver the sentinel record")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]usage.Record(nil), c.records...)
}

func oneHourRefuseHour(body []byte) string {
	if bytes.Contains(body, []byte(`"ttl":"1h"`)) {
		return oneHourLifetimeRefusal
	}
	return ""
}

// Test 4: a refusal of the hour is answered by one re-send as the client wrote
// it; the caller gets the 200, and the request leaves exactly one usage record.
func TestClaudeOneHourCacheRuleRefusalResendsOnce(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "claude-*"}}, Params: map[string]any{"top_k": 7}}},
	}}
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			capture := newOneHourUsageCapture(t, "one-hour-"+t.Name())
			payload := oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")
			upstream, _, err := runOneHourCall(t, oneHourCall{cfg: cfg, stream: stream, payload: payload, refuse: oneHourRefuseHour})
			if err != nil {
				t.Fatalf("caller got error %v, want the re-sent request's 200", err)
			}
			if got := upstream.count(); got != 2 {
				t.Fatalf("upstream saw %d requests, want 2 (refused, then re-sent)", got)
			}
			first, second := upstream.body(0), upstream.body(1)
			if !bytes.Contains(first, []byte(`"ttl":"1h"`)) {
				t.Fatalf("first attempt carried no one-hour marker: %s", first)
			}
			if bytes.Contains(second, []byte(`"ttl"`)) {
				t.Fatalf("re-sent body keeps a ttl the rule added: %s", second)
			}
			for i, body := range [][]byte{first, second} {
				if got := ojson.GetBytes(body, "top_k").Int(); got != 7 {
					t.Fatalf("attempt %d: payload rule not applied to the sent body (top_k=%d)", i+1, got)
				}
			}
			records := capture.settled(t)
			if len(records) != 1 {
				t.Fatalf("usage records = %d, want exactly 1: %+v", len(records), records)
			}
			if records[0].Failed {
				t.Fatalf("the only usage record is a failure: %+v", records[0])
			}
		})
	}
}

// Guard: a 400 that does not name the lifetime goes back to the caller after
// one upstream request.
func TestClaudeOneHourCacheRuleOtherRefusalReturnsToCaller(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", ""),
				refuse: func([]byte) string { return oneHourOtherRefusal }})
			if err == nil || !strings.Contains(err.Error(), "non-empty") {
				t.Fatalf("caller error = %v, want the upstream 400", err)
			}
			if got := upstream.count(); got != 1 {
				t.Fatalf("upstream saw %d requests, want 1", got)
			}
		})
	}
}

// Guard: a lifetime refusal of a request the rule did not change (the caller
// wrote the hour itself) goes back to the caller after one upstream request.
func TestClaudeOneHourCacheRuleRefusalOfUnchangedRequestReturnsToCaller(t *testing.T) {
	marker := `{"type":"ephemeral","ttl":"1h"}`
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: oneHourPayload(marker, marker, marker, "", ""), refuse: oneHourRefuseHour})
			if err == nil || !strings.Contains(err.Error(), "cache_control") {
				t.Fatalf("caller error = %v, want the upstream 400", err)
			}
			if got := upstream.count(); got != 1 {
				t.Fatalf("upstream saw %d requests, want 1", got)
			}
		})
	}
}

// Guard: when the order pass removes every hour the rule added (an explicit 5m
// on the first marker), the sent body is the client's, so a lifetime 400 goes
// back to the caller after one upstream request.
func TestClaudeOneHourCacheRuleRefusalWithoutSentHourReturnsToCaller(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: oneHourPayload(`{"type":"ephemeral","ttl":"5m"}`, oneHourBare, oneHourBare, "", ""),
				refuse: func([]byte) string { return oneHourLifetimeRefusal }})
			if err == nil || !strings.Contains(err.Error(), "cache_control") {
				t.Fatalf("caller error = %v, want the upstream 400", err)
			}
			if got := upstream.count(); got != 1 {
				t.Fatalf("upstream saw %d requests, want 1", got)
			}
			if bytes.Contains(upstream.body(0), []byte(`"ttl":"1h"`)) {
				t.Fatalf("the order pass left a one-hour marker after an explicit 5m: %s", upstream.body(0))
			}
		})
	}
}

// Guards: requests the rule must leave as the client wrote them.
func TestClaudeOneHourCacheRuleLeavesOtherRequestsUnchanged(t *testing.T) {
	subagent := oneHourNativeHeaders()
	subagent.Set("X-Claude-Code-Agent-Id", "agent-one-hour-guard")
	probe := []byte(`{"model":"` + oneHourModel + `","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"quota","cache_control":{"type":"ephemeral"}}]}],"metadata":{"user_id":` + strconv.Quote(oneHourUserID) + `}}`)
	title := []byte(`{"model":"` + oneHourModel + `","max_tokens":32000,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Return a short title."}],"messages":[{"role":"user","content":[{"type":"text","text":"name this session","cache_control":{"type":"ephemeral"}}]}],"tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"metadata":{"user_id":` + strconv.Quote(oneHourUserID) + `}}`)
	cases := []struct {
		name    string
		call    oneHourCall
		stream  bool
		ttl     string
		hasBeta bool
	}{
		{name: "subagent", call: oneHourCall{headers: subagent, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")}},
		{name: "probe", call: oneHourCall{payload: probe}},
		{name: "title request", call: oneHourCall{payload: title}},
		{name: "key seat", call: oneHourCall{apiKey: oneHourKeySeatKey, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")}},
		// The hub's OAuth beta set for a confirmed native main conversation already
		// carries extended-cache-ttl, before and after the rule; only the markers matter.
		{name: "explicit cache mode", hasBeta: true, call: oneHourCall{payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", `"prompt_cache_options":{"mode":"explicit"},`)}},
		{name: "token counting", call: oneHourCall{count: true, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")}},
		{name: "caller wrote the hour", ttl: "1h", hasBeta: true, call: oneHourCall{payload: oneHourPayload(`{"type":"ephemeral","ttl":"1h"}`, `{"type":"ephemeral","ttl":"1h"}`, `{"type":"ephemeral","ttl":"1h"}`, "", "")}},
	}
	for _, tc := range cases {
		for _, stream := range oneHourPaths {
			if tc.call.count && stream {
				continue
			}
			t.Run(tc.name+"/"+oneHourPathName(stream), func(t *testing.T) {
				call := tc.call
				call.stream = stream
				upstream, _, err := runOneHourCall(t, call)
				if err != nil {
					t.Fatalf("request error = %v", err)
				}
				body := upstream.body(0)
				markers := oneHourMarkers(t, body)
				if len(markers) == 0 {
					t.Fatalf("no cache marker reached upstream: %s", body)
				}
				for label, raw := range markers {
					if got := oneHourMarkerTTL(raw); got != tc.ttl {
						t.Fatalf("%s ttl = %q, want %q; markers=%v", label, got, tc.ttl, markers)
					}
				}
				if got := oneHourBeta(upstream.header(0)); got != tc.hasBeta {
					t.Fatalf("extended-cache-ttl beta = %v, want %v: %q", got, tc.hasBeta, upstream.header(0).Get("Anthropic-Beta"))
				}
			})
		}
	}
}
