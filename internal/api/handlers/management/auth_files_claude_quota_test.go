package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestV8CredentialsExposeClaudeQuotaWindowsRankAndHold(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	m := coreauth.NewManager(nil, nil, nil)
	cfg := &config.Config{AuthDir: t.TempDir()}
	m.SetConfig(cfg)
	registerAuthForLookupTest(t, m, &coreauth.Auth{
		ID: "quota-test", Provider: "claude", Status: coreauth.StatusActive,
		Attributes:  map[string]string{"runtime_only": "true", "priority": "4", "quota_rank": "1"},
		Quota:       coreauth.QuotaState{Windows: map[string]coreauth.QuotaWindow{"Fable": {Utilization: 100, ResetsAt: reset, ObservedAt: now}}, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "allowed"}},
		ModelStates: map[string]*coreauth.ModelState{"claude-fable-5-1": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: reset, Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota_cap", NextRecoverAt: reset}}},
	})
	h := NewHandlerWithoutConfigFilePath(cfg, m)
	for _, v8 := range []bool{true, false} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
		ctx.Set(ConfigV8ContextKey, v8)
		h.ListAuthFiles(ctx)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Files []struct {
				Priority int  `json:"priority"`
				Rank     *int `json:"quota_rank"`
				Quota    struct {
					Windows map[string]coreauth.QuotaWindow `json:"windows"`
					Signals map[string]string               `json:"signals"`
				} `json:"quota"`
				Cooldowns []coreauth.CooldownView `json:"cooldowns"`
			} `json:"files"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(payload.Files) != 1 {
			t.Fatalf("files=%s", rec.Body.String())
		}
		entry := payload.Files[0]
		if entry.Priority != 4 || len(entry.Quota.Signals) != 1 {
			t.Fatal("existing priority/signals changed")
		}
		if v8 {
			if entry.Rank == nil || *entry.Rank != 1 || entry.Quota.Windows["Fable"].Utilization != 100 || !entry.Quota.Windows["Fable"].ResetsAt.Equal(reset) {
				t.Fatal("missing quota windows/rank")
			}
			if len(entry.Cooldowns) != 1 || entry.Cooldowns[0].Reason != "quota_cap" || entry.Cooldowns[0].Scope != "model" {
				t.Fatal("hold projection incorrect")
			}
		} else if entry.Rank != nil || entry.Quota.Windows != nil {
			t.Fatal("v8 fields leaked into deprecated response")
		}
	}
}
