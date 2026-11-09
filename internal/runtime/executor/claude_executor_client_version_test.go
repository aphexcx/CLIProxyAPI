package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeExecutorClientVersion(t *testing.T) {
	for _, path := range []string{"messages", "stream", "count_tokens"} {
		for _, tc := range []struct {
			name, ua, want, rule, toolType   string
			off, spoof, override, noMetadata bool
			adaptive                         bool
		}{
			{name: "newer", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400"},
			{name: "newer cloaked body", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true},
			{name: "newer cloaked fast override", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, rule: "fast"},
			{name: "newer native fast override", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", rule: "fast"},
			{name: "newer cloaked filtered features", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, rule: "filter-features"},
			{name: "newer cloaked adaptive", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, adaptive: true},
			{name: "newer filtered context management", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, adaptive: true, rule: "filter-context"},
			{name: "newer missing CLI beta", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, rule: "missing-cli-beta"},
			{name: "newer tool removal", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, toolType: "tool_removal"},
			{name: "newer tool reference addition", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", noMetadata: true, toolType: "tool_addition"},
			{name: "older", ua: "claude-cli/2.1.100 (external, cli)", want: "2.1.321"},
			{name: "curl", ua: "curl/8.0", want: "2.1.321"},
			{name: "management", ua: "Mozilla/5.0", want: "2.1.321"},
			{name: "spoof", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.321", spoof: true},
			{name: "off", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.321", off: true},
			{name: "configured baseline", ua: "curl/8.0", want: "2.1.350", override: true},
			{name: "payload override", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.321", off: true, rule: "override"},
			{name: "payload filter", ua: "claude-cli/2.1.400 (external, cli)", want: "2.1.400", rule: "filter"},
		} {
			// count_tokens accepts user/assistant turns only, not tool-change system turns.
			if path == "count_tokens" && tc.toolType != "" {
				continue
			}
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				helps.ResetClaudeDeviceProfileCache()
				stabilize, enabled := true, !tc.off
				cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
					StabilizeDeviceProfile: &stabilize, PassthroughNewerClient: &enabled,
					OS: "Linux", Arch: "x64",
				}}
				if tc.override {
					cfg.ClaudeHeaderDefaults.UserAgent = "claude-cli/2.1.350 (external, cli)"
				}
				models := []config.PayloadModelRule{{Name: "*", Protocol: "claude"}}
				if tc.rule == "override" {
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"system.0.text": "operator billing override"}}}
				} else if tc.rule == "filter" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"system"}}}
				} else if tc.rule == "fast" {
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"speed": "fast"}}}
				} else if tc.rule == "filter-features" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"fallbacks", "diagnostics"}}}
				} else if tc.rule == "filter-context" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"context_management"}}}
				}
				betas := "claude-code-20250219,oauth-2025-04-20,effort-2025-11-24,new-client-feature-2026-11-09,extended-cache-ttl-2025-04-11"
				if path == "count_tokens" {
					betas += ",token-counting-2024-11-01"
				}
				if tc.rule == "missing-cli-beta" {
					betas = strings.TrimPrefix(betas, "claude-code-20250219,")
				}
				headers := claudeNativeHelperHeaders(betas, "gzip, deflate, br, zstd")
				headers.Set("User-Agent", tc.ua)
				headers.Set("X-Stainless-Package-Version", "0.140.0")
				headers.Set("X-Stainless-Runtime-Version", "v27.0.0")
				if tc.spoof {
					headers.Set("X-Stainless-Runtime", "python")
				}
				payload := []byte(`{"model":"claude-fable-5-2","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"}}`)
				model := "claude-fable-5-2"
				if tc.toolType != "" {
					model = "claude-sonnet-5-5"
					doc := ojson.NewDoc(payload)
					_ = doc.Set("model", model)
					_ = doc.SetRawBytes("tools", []byte(`[{"name":"lookup_notes","input_schema":{"type":"object","properties":{}}}]`))
					_ = doc.SetRawBytes("messages", []byte(fmt.Sprintf(`[{"role":"user","content":"hi"},{"role":"system","content":[{"type":%q,"tool":{"type":"tool_reference","name":"lookup_notes"}}]},{"role":"user","content":"continue"}]`, tc.toolType)))
					payload = doc.Bytes()
				}
				if tc.noMetadata {
					payload, _ = ojson.DeleteBytes(payload, "metadata")
				}
				if tc.adaptive {
					doc := ojson.NewDoc(payload)
					_ = doc.Set("thinking.type", "adaptive")
					payload = doc.Bytes()
				}
				if !tc.noMetadata {
					doc := ojson.NewDoc(payload)
					_ = doc.SetRawBytes("system", []byte(`[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.400.abc; cc_entrypoint=cli;"}]`))
					payload = doc.Bytes()
				}
				var seenHeaders http.Header
				var seenBody []byte
				transport := claudeFingerprintRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					seenHeaders = r.Header.Clone()
					seenBody, _ = io.ReadAll(r.Body)
					response := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-fable-5-2","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					contentType := "application/json"
					if path == "count_tokens" {
						response = `{"input_tokens":1}`
					} else if path == "stream" {
						contentType = "text/event-stream"
						response = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + response + "}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
				})
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
				executor := NewClaudeExecutor(cfg)
				auth := claudeNativeHelperOAuthAuth("https://api.anthropic.com")
				req := cliproxyexecutor.Request{Model: model, Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers}
				switch path {
				case "messages":
					if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				case "stream":
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				case "count_tokens":
					if _, err := executor.CountTokens(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				}
				if got := seenHeaders.Get("User-Agent"); got != fmt.Sprintf("claude-cli/%s (external, cli)", tc.want) {
					t.Fatalf("UA = %q", got)
				}
				if helps.HeaderValueCaseInsensitive(seenHeaders, "X-Stainless-OS") != "Linux" || seenHeaders.Get("X-Stainless-Arch") != "x64" {
					t.Fatalf("seat platform changed: OS=%q arch=%q", helps.HeaderValueCaseInsensitive(seenHeaders, "X-Stainless-OS"), seenHeaders.Get("X-Stainless-Arch"))
				}
				wantPackage, wantRuntime := "0.128.0", "v26.12.1"
				if tc.want == "2.1.400" {
					wantPackage, wantRuntime = "0.140.0", "v27.0.0"
					got := helps.HeaderValueCaseInsensitive(seenHeaders, "Anthropic-Beta")
					for _, beta := range strings.Split(betas, ",") {
						if !strings.Contains(","+got+",", ","+beta+",") {
							t.Fatalf("caller beta %s lost: %s", beta, got)
						}
					}
					if !tc.noMetadata && tc.rule != "fast" && got != betas {
						t.Fatalf("betas = %q, want %q", got, betas)
					}
					if tc.rule == "fast" && (ojson.GetBytes(seenBody, "speed").String() != "fast" || !strings.Contains(got, claudeFastModeBeta)) {
						t.Fatalf("fast override lacks required beta: %s", got)
					}
					if tc.noMetadata && path != "count_tokens" {
						if tc.adaptive {
							contextPresent := ojson.GetBytes(seenBody, "context_management.edits").IsArray()
							contextBeta := strings.Contains(got, "context-management-2025-06-27")
							if contextPresent != (tc.rule != "filter-context") || contextBeta != contextPresent {
								t.Fatalf("context body/beta mismatch: present=%v, betas=%s", contextPresent, got)
							}
							if !strings.Contains(got, "interleaved-thinking-2025-05-14") {
								t.Fatal("adaptive thinking lost its beta")
							}
						}
						if !strings.Contains(got, claudeCodeBeta) {
							t.Fatal("injected CLI identity lost its beta")
						}
						if tc.rule == "filter-features" {
							for _, field := range []string{"fallbacks", "diagnostics"} {
								if ojson.GetBytes(seenBody, field).Exists() {
									t.Fatalf("filtered %s restored", field)
								}
							}
							for _, beta := range []string{claudeServerSideFallbackBeta, claudeFallbackCreditBeta, claudeCacheDiagnosisBeta} {
								if strings.Contains(got, beta) {
									t.Fatalf("filtered feature beta restored: %s", beta)
								}
							}
						} else if tc.toolType != "" {
							found := false
							for _, message := range ojson.GetBytes(seenBody, "messages").Array() {
								for _, block := range message.Get("content").Array() {
									found = found || block.Get("type").String() == tc.toolType
								}
							}
							if !found || !strings.Contains(got, claudeMidConvToolChangesBeta) {
								t.Fatalf("tool change or required beta lost: found=%v, betas=%s", found, got)
							}
							if strings.Contains(got, claudeInlineToolsBeta) {
								t.Fatal("reference-only tool change acquired inline-definition beta")
							}
						} else {
							for _, field := range []string{"fallbacks", "diagnostics"} {
								if !ojson.GetBytes(seenBody, field).Exists() {
									t.Fatalf("cloak did not inject %s", field)
								}
							}
							for _, beta := range []string{claudeServerSideFallbackBeta, claudeFallbackCreditBeta, claudeCacheDiagnosisBeta} {
								if !strings.Contains(got, beta) {
									t.Fatalf("cloaked body lacks %s: %s", beta, got)
								}
							}
						}
					}
				}
				if seenHeaders.Get("X-Stainless-Package-Version") != wantPackage || seenHeaders.Get("X-Stainless-Runtime-Version") != wantRuntime {
					t.Fatal("software tuple differs from selected version")
				}
				if seenHeaders.Get("X-Stainless-Lang") != "js" || seenHeaders.Get("X-Stainless-Runtime") != "node" {
					t.Fatal("mismatched runtime companion headers reached upstream")
				}
				if path != "count_tokens" {
					billing := ojson.GetBytes(seenBody, "system.0.text").String()
					if tc.rule == "override" && billing != "operator billing override" {
						t.Fatalf("payload override was overwritten: %q", billing)
					} else if tc.rule == "filter" && ojson.GetBytes(seenBody, "system").Exists() {
						t.Fatal("filtered system was restored")
					} else if tc.rule == "" && !strings.Contains(billing, "cc_version="+tc.want+".") {
						t.Fatalf("billing = %q", billing)
					}
					if device := ojson.Get(ojson.GetBytes(seenBody, "metadata.user_id").String(), "device_id").String(); device == "" || device == strings.Repeat("f", 64) {
						t.Fatalf("credential device identity missing or caller-supplied: %q", device)
					}
				}
			})
		}
	}
}

func TestClaudeBodyRequiredBetasConstantFeatures(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no feature", `{}`, ""},
		{"adaptive thinking", `{"thinking":{"type":"adaptive"}}`, "interleaved-thinking-2025-05-14"},
		{"disabled thinking", `{"thinking":{"type":"disabled"}}`, ""},
		{"context edits", `{"context_management":{"edits":[]}}`, "context-management-2025-06-27"},
		{"top cache scope", `{"cache_control":{"type":"ephemeral","scope":"global"}}`, "prompt-caching-scope-2026-01-05"},
		{"tool cache scope", `{"tools":[{"cache_control":{"type":"ephemeral","scope":"global"}}]}`, "prompt-caching-scope-2026-01-05"},
		{"system cache scope", `{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","scope":"global"}}]}`, "prompt-caching-scope-2026-01-05"},
		{"content cache scope", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","scope":"global"}}]}]}`, "prompt-caching-scope-2026-01-05"},
		{"unrelated scope", `{"scope":"global","system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}`, ""},
		{"tool removal", `{"model":"claude-sonnet-5-5","messages":[{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"lookup_notes"}}]}]}`, claudeMidConvToolChangesBeta},
		{"tool reference addition", `{"model":"claude-sonnet-5-5","messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"lookup_notes"}}]}]}`, claudeMidConvToolChangesBeta},
		{"tool inline addition", `{"model":"claude-sonnet-5-5","messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"definition":{"name":"lookup_notes"}}}]}]}`, claudeMidConvToolChangesBeta + "," + claudeInlineToolsBeta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeBodyRequiredBetas([]byte(tc.body), false); got != tc.want {
				t.Fatalf("required betas = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeNewerClientRequiredBetas(t *testing.T) {
	const incomingBetas = "claude-code-20250219,future-client-feature,effort-2025-11-24"
	headers := claudeNativeHelperHeaders(incomingBetas, "gzip, deflate, br, zstd")
	headers.Set("User-Agent", "claude-cli/2.1.400 (external, cli)")
	// Replay causes the executor to add binding controls, which the incoming
	// client could not know would be necessary for this selected upstream.
	body := applyClaudePreservedThinkingBinding([]byte(`{"model":"claude-haiku-5-5","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"test-signed-block"}]},{"role":"user","content":"continue"}]}`))
	if !ojson.GetBytes(body, "thinking.block_binding").Exists() {
		t.Fatal("replayed thinking did not trigger binding controls")
	}
	for _, strip := range []bool{false, true} {
		t.Run(fmt.Sprintf("operator-strip=%v", strip), func(t *testing.T) {
			cfg := &config.Config{}
			if strip {
				cfg.ClaudeHeaderDefaults.AnthropicBetaStrip = []string{claudeThinkingBindingBeta}
			}
			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			auth := claudeNativeHelperOAuthAuth("https://api.anthropic.com")
			if err = applyClaudeHeaders(req, auth, auth.Attributes["api_key"], false, nil, body, cfg, headers, true); err != nil {
				t.Fatal(err)
			}
			betas := req.Header.Get("Anthropic-Beta")
			for _, required := range []string{claudeCodeBeta, claudeOAuthBeta, "future-client-feature", claudeEffortBeta} {
				if !strings.Contains(betas, required) {
					t.Fatalf("missing %s: %s", required, betas)
				}
			}
			if strings.Contains(betas, claudeThinkingBindingBeta) == strip {
				t.Fatalf("binding beta/operator precedence incorrect: %s", betas)
			}
		})
	}
}
