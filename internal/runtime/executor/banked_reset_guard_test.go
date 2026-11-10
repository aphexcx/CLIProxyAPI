package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type resetGuardTransport struct{ t *testing.T }

func (r resetGuardTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Fatal("credential-bearing request reached upstream")
	return nil, errors.New("unexpected fixture send")
}

func TestBankedResetRelabeledKimiInference(t *testing.T) {
	// This is the exact persisted state produced by the old fields PATCH. Even
	// an already-compromised file must not send a claim after watcher synthesis.
	for _, source := range []translator.Format{translator.FormatClaude, translator.FormatOpenAI, translator.FormatOpenAIResponse} {
		t.Run(string(source), func(t *testing.T) {
			cfg := &config.Config{}
			seats, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()}, "fixture.json", []byte(`{"type":"kimi","access_token":"fixture-claude-token","base_url":"https://fixture.invalid/api/organizations/fixture/reset_rate_limits?x="}`))
			if err != nil || len(seats) != 1 || seats[0].Provider != "kimi" {
				t.Fatalf("synthesis: %v seats=%d", err, len(seats))
			}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", resetGuardTransport{t})
			request := exec.Request{Model: "kimi-k2", Payload: []byte(`{"model":"kimi-k2","max_tokens":8,"messages":[{"role":"user","content":"fixture"}],"input":"fixture","program":"cedar_ember","grant_id":"fixture-grant","request_id":"fixture-request"}`)}
			_, err = NewKimiExecutor(cfg).Execute(ctx, seats[0], request, exec.Options{SourceFormat: source})
			if !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
				t.Fatalf("expected claim denial, got %v", err)
			}
		})
	}
}

func TestBankedResetEveryProviderSend(t *testing.T) {
	cfg := &config.Config{}
	providers := []coreauth.ProviderExecutor{
		NewClaudeExecutor(cfg), NewKimiExecutor(cfg), NewAntigravityExecutor(cfg),
		NewCodexExecutor(cfg), NewOpenAICompatExecutor("openai-compatibility", cfg),
		NewGeminiExecutor(cfg), NewGeminiVertexExecutor(cfg), NewMetaExecutor(cfg),
		NewDevinExecutor(cfg), NewXAIExecutor(cfg),
	}
	for _, provider := range providers {
		t.Run(provider.Identifier(), func(t *testing.T) {
			target := "https://fixture.invalid/api/organizations/fixture/reset_rate_limits?x="
			auth := &coreauth.Auth{ID: "fixture", Provider: provider.Identifier(),
				Attributes: map[string]string{"base_url": target, "api_key": "fixture-key"},
				Metadata:   map[string]any{"base_url": target, "access_token": "fixture-token", "token": "fixture-token", "project_id": "fixture-project", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", resetGuardTransport{t})
			req, err := http.NewRequestWithContext(ctx, "POST", target+"/v1/messages", strings.NewReader(`{"program":"cedar_ember","grant_id":"fixture"}`))
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.HttpRequest(ctx, auth, req)
			if !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
				t.Fatalf("expected denial, got %v", err)
			}
			// Exercise each provider's own base-URL request construction too.
			_, err = provider.Execute(ctx, auth, exec.Request{Model: "fixture-model", Payload: []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"fixture"}],"max_tokens":8,"program":"cedar_ember","grant_id":"fixture"}`)}, exec.Options{SourceFormat: translator.FormatOpenAI})
			if !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
				t.Fatalf("inference expected denial, got %v", err)
			}
			_, err = provider.ExecuteStream(ctx, auth, exec.Request{Model: "fixture-model", Payload: []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"fixture"}],"max_tokens":8,"stream":true,"program":"cedar_ember","grant_id":"fixture"}`)}, exec.Options{SourceFormat: translator.FormatOpenAI, Stream: true})
			if !errors.Is(err, httptransport.ErrResetRateLimitsDenied) {
				t.Fatalf("stream inference expected denial, got %v", err)
			}
		})
	}
}
