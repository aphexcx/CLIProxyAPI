package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The fixture was captured over HTTP against a separately built main server at
// the recorded SHA. It is never regenerated from the implementation under test.
func TestClaudeExecutorRollbackMatchesMain(t *testing.T) {
	data, err := os.ReadFile("testdata/claude_rollback_main.json")
	if err != nil {
		t.Fatal(err)
	}
	type wire struct {
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	}
	var fixture struct {
		BaseSHA string `json:"base_sha"`
		Cases   []struct {
			Name      string            `json:"name"`
			Stabilize bool              `json:"stabilize"`
			Profile   string            `json:"profile"`
			Cloak     string            `json:"cloak"`
			Headers   map[string]string `json:"headers"`
			Body      json.RawMessage   `json:"body"`
			Expected  wire              `json:"expected"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.BaseSHA != "ca4382d09a1338ba000fc1d7bdc94ebe97b2421a" || len(fixture.Cases) != 24 {
		t.Fatal("unexpected rollback oracle provenance or case count")
	}
	for _, tc := range fixture.Cases {
		for _, enabled := range []bool{false, true} {
			// Enabled behavior matches main only outside the cloak policy.
			if enabled && tc.Cloak != "never" && tc.Profile != "" {
				continue
			}
			mode := "off/"
			if enabled {
				mode = "on/"
			}
			t.Run(mode+tc.Name, func(t *testing.T) {
				helps.ResetClaudeDeviceProfileCache()
				var seen wire
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen.Body, _ = io.ReadAll(r.Body)
					seen.Headers = make(map[string]string)
					for k, values := range r.Header {
						name := strings.ToLower(k)
						if name != "authorization" && name != "x-api-key" && name != "content-length" {
							seen.Headers[name] = strings.Join(values, ",")
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				}))
				defer server.Close()
				cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
					StabilizeDeviceProfile: &tc.Stabilize, PassthroughNewerClient: &enabled, OS: "Linux", Arch: "x64",
				}}
				auth := &cliproxyauth.Auth{ID: "rollback", Provider: "claude", Attributes: map[string]string{
					"api_key": "scratch-upstream-only", "base_url": server.URL,
					"fingerprint_profile": tc.Profile, "cloak_mode": tc.Cloak,
				}}
				headers := make(http.Header)
				for k, v := range tc.Headers {
					headers.Set(k, v)
				}
				headers.Set("Content-Type", "application/json")
				var body map[string]any
				if errDecode := json.Unmarshal(tc.Body, &body); errDecode != nil {
					t.Fatal(errDecode)
				}
				_, errExecute := NewClaudeExecutor(cfg).Execute(context.Background(), auth,
					cliproxyexecutor.Request{Model: body["model"].(string), Payload: tc.Body},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: tc.Body, Headers: headers})
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				got := normalizeClaudeRollbackWire(t, seen.Headers, seen.Body)
				want := normalizeClaudeRollbackWire(t, tc.Expected.Headers, tc.Expected.Body)
				for field, expected := range want {
					if !reflect.DeepEqual(got[field], expected) {
						actualJSON, _ := json.Marshal(got[field])
						expectedJSON, _ := json.Marshal(expected)
						t.Errorf("%s differs from main: got %s; want %s", field, actualJSON, expectedJSON)
					}
				}
			})
		}
	}
}

func normalizeClaudeRollbackWire(t *testing.T, headers map[string]string, raw json.RawMessage) map[string]any {
	t.Helper()
	// Only the clock and generated session UUID vary across independent server
	// processes. Preserve beta order, all software fields and the complete body.
	date := regexp.MustCompile(`Today's date is \d{4}-\d{2}-\d{2}`)
	raw = date.ReplaceAll(raw, []byte("Today's date is <date>"))
	sessionID := headers["x-claude-code-session-id"]
	if sessionID != "" && sessionID != "11111111-2222-4333-8444-555555555555" {
		raw = json.RawMessage(strings.ReplaceAll(string(raw), sessionID, "<generated-session>"))
		headers = copyClaudeRollbackHeaders(headers)
		headers["x-claude-code-session-id"] = "<generated-session>"
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"headers": headers, "body": body}
}

func copyClaudeRollbackHeaders(headers map[string]string) map[string]string {
	copyHeaders := make(map[string]string, len(headers))
	for k, v := range headers {
		copyHeaders[k] = v
	}
	return copyHeaders
}
