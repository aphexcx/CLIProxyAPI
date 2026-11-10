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

type resetStatusPoller struct{ cachedUsagePoller }

func (p *resetStatusPoller) PollQuotaUsage(ctx context.Context, auth *coreauth.Auth) (map[string]coreauth.QuotaWindow, json.RawMessage, error) {
	windows, err := p.PollQuota(ctx, auth)
	if err != nil {
		return nil, nil, err
	}
	// The OAuth parser preserves the display-name alias used by newer responses.
	windows["Fable 5"] = windows["Fable"]
	delete(windows, "Fable")
	weekly, expiry := time.Now().Add(72*time.Hour), time.Now().Add(7*24*time.Hour)
	body, _ := json.Marshal(gin.H{"cedar_ember": coreauth.ClaudeLimitResets{
		Eligible: true, AtLimit: true, WeeklyResetsAt: &weekly,
		Grants: []coreauth.ClaudeResetGrant{{ID: "synthetic-launch", Label: "Launch", ResetsLeft: 2, EndsAt: &expiry, Type: "weekly", Clears: []string{"seven_day_overage_included"}, UsableNow: true}},
	}})
	return windows, body, err
}

func TestLimitResetsReadsCacheOnlyAndPreservesFailureAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Claude.Quota.Enabled = true
		m := coreauth.NewManager(nil, nil, nil)
		m.SetConfig(cfg)
		_, err := m.Register(context.Background(), &coreauth.Auth{ID: "fixture", FileName: "fixture.json", Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "never-sent-fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		p := &resetStatusPoller{}
		m.RegisterExecutor(p)
		h := &Handler{cfg: cfg, authManager: m}
		read := func(path string, code int) []claudeLimitResetSeat {
			t.Helper()
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
			h.GetClaudeLimitResets(ctx)
			if rec.Code != code {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
			}
			var response struct {
				Seats []claudeLimitResetSeat `json:"seats"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			return response.Seats
		}
		if got := read("/?auth=fixture.json", 200); len(got) != 1 || got[0].CedarEmber != nil || !got[0].Stale {
			t.Fatalf("unpolled=%+v", got)
		}
		if p.calls.Load() != 0 {
			t.Fatal("GET initiated a poll")
		}
		m.StartQuotaPolling(context.Background())
		t.Cleanup(m.StopQuotaPolling)
		synctest.Wait()
		for range 10 {
			got := read("/?auth=fixture.json", 200)
			if len(got) != 1 || got[0].CedarEmber.Grants[0].ResetsLeft != 2 || got[0].BestClickRank == nil || *got[0].BestClickRank != 1 || got[0].Stale {
				t.Fatalf("cached=%+v", got)
			}
		}
		read("/?auth=absent", 404)
		if p.calls.Load() != 1 {
			t.Fatal("GET bypassed quota cadence")
		}
		p.fail.Store(true)
		<-time.After(121 * time.Second)
		synctest.Wait()
		got := read("/", 200)[0]
		if got.CedarEmber == nil || got.AgeSeconds == nil || *got.AgeSeconds != 121 || !got.Stale || got.Error != "usage poll failed (HTTP 429)" || got.BestClickRank != nil {
			t.Fatalf("failed poll=%+v", got)
		}
	})
}

func TestLimitResetRanking(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	seat := func(name string, usage float64, weeklyHours, expiryHours int) claudeLimitResetSeat {
		weekly, expiry := now.Add(time.Duration(weeklyHours)*time.Hour), now.Add(time.Duration(expiryHours)*time.Hour)
		s := claudeLimitResetSeat{Name: name, FableUtilization: &usage, ClaudeLimitResetsSnapshot: coreauth.ClaudeLimitResetsSnapshot{CedarEmber: &coreauth.ClaudeLimitResets{Eligible: true, WeeklyResetsAt: &weekly, Grants: []coreauth.ClaudeResetGrant{{ResetsLeft: 1, EndsAt: &expiry, UsableNow: true, Clears: []string{"seven_day_overage_included"}}}}}}
		s.RankReason = resetRankReason(&s, now)
		return s
	}
	seats := []claudeLimitResetSeat{seat("soon-weekly", 100, 24, 200), seat("near", 95, 120, 150), seat("wall-later-expiry", 100, 72, 240), seat("wall", 100, 72, 200), seat("wall-long-weekly", 100, 96, 240), seat("low", 50, 96, 200)}
	rankClaudeResetSeats(seats)
	want := map[string]int{"wall-long-weekly": 1, "wall": 2, "wall-later-expiry": 3, "near": 4}
	for _, s := range seats {
		if expected := want[s.Name]; expected != 0 {
			if s.BestClickRank == nil || *s.BestClickRank != expected {
				t.Fatalf("%s rank=%v", s.Name, s.BestClickRank)
			}
		} else if s.BestClickRank != nil {
			t.Fatalf("ranked %s", s.Name)
		}
	}
	for _, mutate := range []func(*claudeLimitResetSeat){
		func(s *claudeLimitResetSeat) { s.Stale = true },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].ResetsLeft = 0 },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].UsableNow = false },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].EndsAt = &now },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Eligible = false },
		func(s *claudeLimitResetSeat) { s.CedarEmber.Grants[0].Clears = []string{"five_hour"} },
	} {
		s := seat("skip", 100, 72, 240)
		mutate(&s)
		if resetRankReason(&s, now) == "holder action only" {
			t.Fatal("unsafe recommendation")
		}
	}
}
