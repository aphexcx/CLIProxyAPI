package management

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
		Quota:       coreauth.QuotaState{RankExpiresAt: reset, Windows: map[string]coreauth.QuotaWindow{"Fable": {Utilization: 100, ResetsAt: reset, ObservedAt: now}}, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "allowed"}},
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

type recoveredQuotaPoller struct {
	coreauth.ProviderExecutor
	provider string
}

func (p *recoveredQuotaPoller) Identifier() string { return p.provider }
func (*recoveredQuotaPoller) PollQuota(context.Context, *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	now := time.Now()
	windows := make(map[string]coreauth.QuotaWindow)
	for _, name := range []string{"five_hour", "seven_day", "Fable"} {
		windows[name] = coreauth.QuotaWindow{Utilization: 0, ResetsAt: now.Add(time.Hour), ObservedAt: now}
	}
	return windows, nil
}

func TestV8CredentialsUsageResetClearsCooldownListing(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := coreauth.NewManager(nil, nil, nil)
				cfg := &config.Config{AuthDir: t.TempDir()}
				cfg.Claude.Quota.Enabled = true
				m.SetConfig(cfg)
				auth := &coreauth.Auth{ID: "recovered-seat", Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}
				registerAuthForLookupTest(t, m, auth)
				delay := 2 * time.Hour
				m.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: provider, Model: "claude-fable-5-1", CredentialScope: true, RetryAfter: &delay, Error: &coreauth.Error{HTTPStatus: 429}})
				before, _ := m.GetByID(auth.ID)
				if len(coreauth.CooldownSnapshotForAuth(before, time.Now())) == 0 {
					t.Fatal("missing initial cooldown")
				}
				<-time.After(time.Second) // synctest clock: receipt must follow the 429.
				m.RegisterExecutor(&recoveredQuotaPoller{provider: provider})
				m.StartQuotaPolling(context.Background())
				defer m.StopQuotaPolling()
				synctest.Wait()
				h := &Handler{cfg: cfg, authManager: m}
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
				ctx.Set(ConfigV8ContextKey, true)
				h.ListAuthFiles(ctx)
				var payload struct {
					Files []struct {
						Unavailable bool                    `json:"unavailable"`
						Cooldowns   []coreauth.CooldownView `json:"cooldowns"`
						Quota       struct {
							Windows map[string]coreauth.QuotaWindow `json:"windows"`
						} `json:"quota"`
					} `json:"files"`
				}
				if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
					t.Fatal(errDecode)
				}
				if rec.Code != http.StatusOK || len(payload.Files) != 1 {
					t.Fatalf("unexpected listing: status=%d", rec.Code)
				}
				entry := payload.Files[0]
				if entry.Unavailable || len(entry.Cooldowns) != 0 || len(entry.Quota.Windows) < 2 || entry.Quota.Windows["seven_day"].Utilization != 0 {
					t.Fatalf("reset not reflected in listing: %+v", entry)
				}
			})
		})
	}
}
