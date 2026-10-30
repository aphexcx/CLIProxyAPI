package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type registryQuotaPoller struct {
	coreauth.ProviderExecutor
	windows chan map[string]coreauth.QuotaWindow
}

func (*registryQuotaPoller) Identifier() string { return "claude" }
func (p *registryQuotaPoller) PollQuota(ctx context.Context, _ *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	select {
	case windows := <-p.windows:
		return windows, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestClaudeQuotaSoleSeatModelsStayListedAndReleaseProjection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const fable, sonnet = "claude-fable-5-1", "claude-sonnet-5-5"
		id := t.Name()
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: fable}, {ID: sonnet}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		m := coreauth.NewManager(nil, nil, nil)
		cfg := &config.Config{RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		cfg.Claude.Quota.PollInterval = "1m"
		m.SetConfig(cfg)
		if _, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "test-access"}}); errRegister != nil {
			t.Fatal(errRegister)
		}
		poller := &registryQuotaPoller{windows: make(chan map[string]coreauth.QuotaWindow, 1)}
		reset := time.Now().Add(time.Hour)
		poller.windows <- map[string]coreauth.QuotaWindow{"Fable": {Utilization: 100, ResetsAt: reset, ObservedAt: time.Now()}}
		m.RegisterExecutor(poller)
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		h := openai.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
		assertListed := func(stage string) {
			t.Helper()
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			h.OpenAIModels(ctx)
			var response struct {
				Data []struct{ ID string } `json:"data"`
			}
			if errDecode := json.Unmarshal(rec.Body.Bytes(), &response); errDecode != nil || rec.Code != http.StatusOK {
				t.Fatalf("%s: status=%d decode=%v", stage, rec.Code, errDecode)
			}
			seen := make(map[string]bool)
			for _, model := range response.Data {
				seen[model.ID] = true
			}
			if !seen[fable] || !seen[sonnet] {
				t.Fatalf("%s: held model disappeared from /v1/models: %s", stage, rec.Body.String())
			}
		}
		assertListed("hold installed")
		if reg.GetModelCount(fable) != 0 || reg.GetModelCount(sonnet) != 1 {
			t.Fatal("hold did not publish its routing projection")
		}
		m.MarkResult(context.Background(), coreauth.Result{AuthID: id, Provider: "claude", Model: sonnet, Success: true})
		assertListed("Sonnet succeeded under Fable hold")
		<-time.After(time.Second)
		poller.windows <- map[string]coreauth.QuotaWindow{"Fable": {Utilization: 0, ResetsAt: reset, ObservedAt: time.Now()}}
		<-time.After(70 * time.Second)
		synctest.Wait()
		assertListed("hold released")
		if reg.GetModelCount(fable) != 1 {
			t.Fatal("release left the registry suspended until another execution result")
		}
	})
}
