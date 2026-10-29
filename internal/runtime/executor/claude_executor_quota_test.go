package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeQuotaFable429FailsOverAndSonnetKeepsFirstSeat(t *testing.T) {
	const fable, sonnet = "claude-fable-5-1", "claude-sonnet-5-5"
	attempts := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			http.Error(w, "read failed", 400)
			return
		}
		model := gjson.GetBytes(body, "model").String()
		seat := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		attempts <- seat + ":" + model
		w.Header().Set("Content-Type", "application/json")
		if seat == "first" && model == fable {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Fable limit"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"msg-ok","type":"message","model":%q,"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, model)
	}))
	defer upstream.Close()
	m := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
	cfg := &config.Config{RequestRetry: 3}
	cfg.Claude.Quota.Enabled = true
	m.SetConfig(cfg)
	m.SetRetryConfig(3, 0, 0)
	m.RegisterExecutor(NewClaudeExecutor(cfg))
	for i, seat := range []string{"first", "second"} {
		id := t.Name() + seat
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: fable}, {ID: sonnet}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		_, errRegister := m.Register(context.Background(), &cliproxyauth.Auth{
			ID: id, Provider: "claude", Status: cliproxyauth.StatusActive,
			Attributes: map[string]string{"api_key": seat, "base_url": upstream.URL, "priority": fmt.Sprint(2 - i)},
		})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		result, errExecute := m.Execute(r.Context(), []string{"claude"}, cliproxyexecutor.Request{
			Model: model, Payload: []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, model)),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		if errExecute != nil {
			http.Error(w, errExecute.Error(), 502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result.Payload)
	}))
	defer proxy.Close()
	for _, model := range []string{fable, sonnet} {
		resp, errGet := http.Get(proxy.URL + "?model=" + model)
		if errGet != nil {
			t.Fatal(errGet)
		}
		body, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if errRead != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "msg-ok") {
			t.Fatalf("client status=%d body=%s err=%v", resp.StatusCode, body, errRead)
		}
	}
	close(attempts)
	var got []string
	for attempt := range attempts {
		got = append(got, attempt)
	}
	if want := []string{"first:" + fable, "second:" + fable, "first:" + sonnet}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attempts=%v want=%v", got, want)
	}
}
