package management

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Every value below is a fixture. No test in this file reaches the real
// provider, holds a real token or spends a real reset: the HTTP client dials
// only the local TLS server that stands in for api.anthropic.com.
const (
	resetFixtureOrganization      = "00000000-0000-4000-8000-0000000000aa"
	resetFixtureToken             = "fixture-access-token"
	resetFixtureOtherOrganization = "00000000-0000-4000-8000-0000000000bb"
	resetFixtureOtherToken        = "fixture-access-token-other"
)

// resetFixtureAccounts maps each fixture token to its account's organization.
var resetFixtureAccounts = map[string]string{resetFixtureToken: resetFixtureOrganization, resetFixtureOtherToken: resetFixtureOtherOrganization}

type fakeResetGrant struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	ResetsLeft      int      `json:"resets_left"`
	EndsAt          string   `json:"ends_at,omitempty"`
	Type            string   `json:"type,omitempty"`
	GivesBackWeekly bool     `json:"gives_back_weekly"`
	Clears          []string `json:"clears"`
	UsableNow       bool     `json:"usable_now"`
	Paused          bool     `json:"paused"`
}

type fakeResetClaim struct {
	Program   string `json:"program"`
	GrantID   string `json:"grant_id"`
	RequestID string `json:"request_id"`
}

type fakeResetProvider struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	grants        []fakeResetGrant
	eligible      bool
	reason        string
	cooldownUntil string
	profileStatus int
	readStatus    int
	claimStatus   int
	claimResult   string
	requests      int
	reads         int
	claims        []fakeResetClaim
	onClaim       func(fakeResetClaim)
	onRead        func()
	onProfile     func()
}

func newFakeResetProvider(t *testing.T, grants ...fakeResetGrant) *fakeResetProvider {
	t.Helper()
	f := &fakeResetProvider{t: t, grants: grants, eligible: true, claimResult: "reset"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeResetProvider) serve(w http.ResponseWriter, r *http.Request) {
	// Hooks run outside the lock, so a hook that blocks one claim does not
	// block the other seats' reads.
	var claim fakeResetClaim
	isClaim := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reset_rate_limits")
	if isClaim {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&claim); err != nil || claim.Program != "cedar_ember" || r.Header.Get("Content-Type") != "application/json" {
			f.t.Errorf("malformed claim: %v %+v", err, claim)
		}
	}
	f.mu.Lock()
	onRead, onProfile, onClaim := f.onRead, f.onProfile, f.onClaim
	f.mu.Unlock()
	switch {
	case isClaim && onClaim != nil:
		onClaim(claim)
	case r.URL.Path == "/api/oauth/usage" && onRead != nil:
		onRead()
	case r.URL.Path == "/api/oauth/profile" && onProfile != nil:
		onProfile()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Host != "api.anthropic.com" {
		f.t.Errorf("request for host %q", r.Host)
	}
	// Every spend request closes its connection: no socket outlives a spend.
	if !r.Close {
		f.t.Errorf("request %s %s keeps its connection alive", r.Method, r.URL.Path)
	}
	organization := resetFixtureAccounts[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if organization == "" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || !strings.HasPrefix(r.Header.Get("User-Agent"), "claude-cli/") {
		f.t.Errorf("unexpected headers for %s: %v", r.URL.Path, r.Header)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/oauth/profile":
		if f.profileStatus != 0 {
			w.WriteHeader(f.profileStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account":      map[string]any{"uuid": "00000000-0000-4000-8000-000000000001", "email": "fixture@example.invalid"},
			"organization": map[string]any{"uuid": organization},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/oauth/usage" && r.URL.RawQuery == "cedar_ember=1&skip_spend=1":
		f.reads++
		if f.readStatus != 0 {
			w.WriteHeader(f.readStatus)
			return
		}
		status := map[string]any{"eligible": f.eligible, "ineligible_reason": nil, "at_limit": true, "grants": f.grants}
		if f.reason != "" {
			status["ineligible_reason"] = f.reason
		}
		if f.cooldownUntil != "" {
			status["cooldown_until"] = f.cooldownUntil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 100}, "cedar_ember": status})
	case isClaim && r.URL.Path == "/api/organizations/"+organization+"/reset_rate_limits" && r.URL.RawQuery == "":
		f.claims = append(f.claims, claim)
		if f.claimStatus != 0 {
			w.WriteHeader(f.claimStatus)
			return
		}
		left := 0
		for i := range f.grants {
			if f.grants[i].ID == claim.GrantID && f.claimResult == "reset" {
				f.grants[i].ResetsLeft--
				left = f.grants[i].ResetsLeft
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": f.claimResult, "resets_left": left, "cleared": []string{"seven_day"}})
	default:
		f.t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// client dials only the fake, whatever host a request names.
func (f *fakeResetProvider) client() *http.Client {
	transport := f.server.Client().Transport.(*http.Transport).Clone()
	address := f.server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	transport.TLSClientConfig.ServerName = "example.com"
	return &http.Client{Transport: transport}
}

func (f *fakeResetProvider) counts() (requests, reads int, claims []fakeResetClaim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.reads, append([]fakeResetClaim(nil), f.claims...)
}

func (f *fakeResetProvider) set(update func(*fakeResetProvider)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

func resetFixtureGrants() []fakeResetGrant {
	base := time.Now().UTC().Truncate(time.Second)
	day := func(days int) string { return base.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339) }
	return []fakeResetGrant{
		{ID: "fable-late", Label: "Fixture Fable", ResetsLeft: 1, EndsAt: day(60), Type: "weekly", GivesBackWeekly: true, Clears: []string{"seven_day_overage_included"}, UsableNow: true},
		{ID: "fable-soon", Label: "Fixture Fable", ResetsLeft: 1, EndsAt: day(30), Type: "full", GivesBackWeekly: true, Clears: []string{"seven_day_overage_included"}, UsableNow: true},
		{ID: "full-b", Label: "Fixture full", ResetsLeft: 1, EndsAt: day(20), Type: "full", Clears: []string{"five_hour", "seven_day"}, UsableNow: true},
		{ID: "full-a", Label: "Fixture full", ResetsLeft: 1, EndsAt: day(20), Type: "full", Clears: []string{"five_hour", "seven_day"}, UsableNow: true},
		{ID: "weekly-1", Label: "Fixture weekly", ResetsLeft: 1, EndsAt: day(10), Type: "weekly", Clears: []string{"seven_day"}, UsableNow: true},
		{ID: "session-1", Label: "Fixture session", ResetsLeft: 1, EndsAt: day(5), Type: "session", Clears: []string{"five_hour"}, UsableNow: true},
		{ID: "turbo-1", Label: "Fixture unknown", ResetsLeft: 3, EndsAt: day(1), Type: "turbo", Clears: []string{"seven_day"}, UsableNow: true},
		{ID: "untyped-1", Label: "Fixture untyped", ResetsLeft: 3, EndsAt: day(1), Clears: []string{"seven_day"}, UsableNow: true},
	}
}

// fixtureClock is the handler's controllable clock; tests advance it, never sleep.
type fixtureClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fixtureClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fixtureClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type resetSpendFixture struct {
	h        *Handler
	provider *fakeResetProvider
	index    string
	logDir   string
	clock    *fixtureClock
}

func newResetSpendFixture(t *testing.T, grants ...fakeResetGrant) *resetSpendFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &resetSpendFixture{provider: newFakeResetProvider(t, grants...), logDir: t.TempDir(), clock: &fixtureClock{at: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)}}
	cfg := &config.Config{}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	for _, seat := range []*coreauth.Auth{
		{ID: "fixture", FileName: "fixture.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureToken}},
		{ID: "twin", FileName: "twin.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureToken}},
		{ID: "other", FileName: "other.json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureOtherToken}},
		{ID: "codex", FileName: "codex.json", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"access_token": resetFixtureToken}},
	} {
		if _, err := m.Register(context.Background(), seat); err != nil {
			t.Fatal(err)
		}
	}
	f.h = &Handler{cfg: cfg, authManager: m, logDir: f.logDir}
	fake := f.provider.client()
	f.h.claudeResetClient = func(cfg *config.Config, proxyURL string) *claudeauth.ClaudeAuth {
		return claudeauth.NewClaudeAuthWithProxyURL(cfg, proxyURL, claudeauth.WithHTTPClient(fake))
	}
	f.h.claudeResetSpends.now = f.clock.now
	f.index = f.seatIndex(t, "fixture")
	return f
}

func (f *resetSpendFixture) seatIndex(t *testing.T, id string) string {
	t.Helper()
	for _, auth := range f.h.authManager.List() {
		if auth.ID == id {
			return auth.EnsureIndex()
		}
	}
	t.Fatalf("seat %s not registered", id)
	return ""
}

func (f *resetSpendFixture) spend(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/claude/limit-resets/spend", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	f.h.SpendClaudeLimitReset(ctx)
	if strings.Contains(rec.Body.String(), resetFixtureToken) || strings.Contains(rec.Body.String(), resetFixtureOrganization) {
		t.Fatal("token leaked into the answer")
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer is not JSON: %s", rec.Body)
	}
	return rec.Code, answer
}

func spendBody(fields map[string]any) string {
	encoded, _ := json.Marshal(fields)
	return string(encoded)
}

func TestClaudeResetSpendRefusesARequestThatNamesNoKind(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for _, body := range []string{
		spendBody(map[string]any{"auth_index": f.index}),
		spendBody(map[string]any{"auth_index": f.index, "kind": ""}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "   "}),
		spendBody(map[string]any{"auth_index": f.index, "kind": nil}),
		spendBody(map[string]any{"auth_index": f.index, "grant_id": "full-a"}),
		spendBody(map[string]any{"auth_index": f.index, "grant_id": "fable-soon", "retry_request_id": "x"}),
		`{"auth_index":"` + f.index + `","kind":["fable"]}`,
		``,
	} {
		code, answer := f.spend(t, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("a request naming no kind reached the provider %d times", requests)
	}
}

func TestClaudeResetSpendRefusesKindsOutsideTheTable(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for _, kind := range []string{"turbo", "unknown", "5-hour", "fable reset", "weekly,fable", "any"} {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": kind}))
		if code != http.StatusBadRequest {
			t.Fatalf("%q: status=%d answer=%v", kind, code, answer)
		}
	}
	// A misspelled field is refused rather than ignored: "grantid" would
	// otherwise spend the soonest grant instead of the one the caller meant.
	code, _ := f.spend(t, `{"auth_index":"`+f.index+`","kind":"fable","grantid":"fable-late"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", code)
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("an unknown kind reached the provider %d times", requests)
	}
}

func TestClaudeResetSpendSpendsOnlyTheNamedKindSoonestFirst(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	want := []struct{ kind, grant string }{{"fable", "fable-soon"}, {"full", "full-a"}, {"weekly", "weekly-1"}, {"session", "session-1"}, {"FULL", "full-b"}}
	for i, step := range want {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": step.kind}))
		if code != http.StatusOK || answer["result"] != "reset" || answer["grant_id"] != step.grant || answer["kind"] != strings.ToLower(step.kind) || answer["auth_index"] != f.index {
			t.Fatalf("%s: status=%d answer=%v", step.kind, code, answer)
		}
		if requestID, _ := answer["request_id"].(string); requestID == "" {
			t.Fatalf("%s: answer has no request id: %v", step.kind, answer)
		}
		_, _, claims := f.provider.counts()
		if len(claims) != i+1 || claims[i].GrantID != step.grant || claims[i].RequestID != answer["request_id"] {
			t.Fatalf("%s: claims=%+v", step.kind, claims)
		}
	}
	// A fresh read decides every spend: both full grants are now spent, so a
	// third full spend finds none, whatever an earlier read listed.
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
	if code != http.StatusConflict {
		t.Fatalf("spent kind: status=%d answer=%v", code, answer)
	}
	kinds, _ := answer["kinds"].(map[string]any)
	full, _ := kinds["full"].(map[string]any)
	if full == nil || full["usable_resets"] != float64(0) {
		t.Fatalf("409 must say what is left of each kind: %v", answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != len(want) {
		t.Fatalf("claims=%d", len(claims))
	}
}

func TestClaudeResetSpendHonoursOnlyAGrantOfTheNamedKind(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for _, refused := range []struct {
		kind, grant string
		code        int
	}{
		{"weekly", "fable-late", http.StatusBadRequest},
		{"full", "fable-soon", http.StatusBadRequest},
		{"weekly", "turbo-1", http.StatusBadRequest},
		{"weekly", "untyped-1", http.StatusBadRequest},
		{"fable", "full-a", http.StatusBadRequest},
		{"fable", "no-such-grant", http.StatusConflict},
		{"fable", "Bad/Grant", http.StatusBadRequest},
	} {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": refused.kind, "grant_id": refused.grant}))
		if code != refused.code {
			t.Fatalf("%s/%s: status=%d answer=%v", refused.kind, refused.grant, code, answer)
		}
	}
	if _, _, claims := f.provider.counts(); len(claims) != 0 {
		t.Fatalf("a mismatched grant was claimed: %+v", claims)
	}
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-late"}))
	if code != http.StatusOK || answer["grant_id"] != "fable-late" {
		t.Fatalf("named grant: status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 || claims[0].GrantID != "fable-late" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestClaudeResetSpendNeverMapsAnUnknownGrantType(t *testing.T) {
	grants := resetFixtureGrants()
	f := newResetSpendFixture(t, grants[6], grants[7])
	for _, kind := range []string{"fable", "full", "weekly", "session"} {
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": kind}))
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", kind, code, answer)
		}
		kinds, _ := answer["kinds"].(map[string]any)
		unknown, _ := kinds["unknown"].(map[string]any)
		if len(kinds) != 1 || unknown == nil || unknown["resets_left"] != float64(6) || unknown["usable_resets"] != float64(0) {
			t.Fatalf("%s: kinds=%v", kind, kinds)
		}
	}
	if _, reads, claims := f.provider.counts(); len(claims) != 0 || reads != 4 {
		t.Fatalf("reads=%d claims=%+v", reads, claims)
	}
}

func TestClaudeResetSpendSpendsNothingWhenTheSeatCannotSpend(t *testing.T) {
	cases := []struct {
		name   string
		update func(*fakeResetProvider)
		code   int
	}{
		{"profile read fails", func(p *fakeResetProvider) { p.profileStatus = http.StatusUnauthorized }, http.StatusBadGateway},
		{"grants read rate limited", func(p *fakeResetProvider) { p.readStatus = http.StatusTooManyRequests }, http.StatusBadGateway},
		{"seat ineligible", func(p *fakeResetProvider) { p.eligible, p.reason = false, "surface" }, http.StatusConflict},
		{"cooldown", func(p *fakeResetProvider) {
			p.cooldownUntil = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		}, http.StatusConflict},
		{"paused", func(p *fakeResetProvider) {
			for i := range p.grants {
				p.grants[i].Paused = true
			}
		}, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			f.provider.set(tc.update)
			code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
			if code != tc.code {
				t.Fatalf("status=%d answer=%v", code, answer)
			}
			if _, _, claims := f.provider.counts(); len(claims) != 0 {
				t.Fatalf("claims=%+v", claims)
			}
		})
	}
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	for index, want := range map[string]int{"": http.StatusBadRequest, "no-such-seat": http.StatusNotFound, f.seatIndex(t, "codex"): http.StatusNotFound} {
		if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": index, "kind": "fable"})); code != want {
			t.Fatalf("seat %q: status=%d answer=%v", index, code, answer)
		}
	}
	if requests, _, _ := f.provider.counts(); requests != 0 {
		t.Fatalf("an unknown seat reached the provider %d times", requests)
	}
}

func TestClaudeResetSpendUnknownOutcomeRetriesOnlyWithTheSameRequestID(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	code, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	if code != http.StatusOK || first["result"] != "unknown" || requestID == "" || first["grant_id"] != "fable-soon" || first["retry_until"] == nil {
		t.Fatalf("status=%d answer=%v", code, first)
	}
	claimCount := func() int { _, _, claims := f.provider.counts(); return len(claims) }
	// The hub never retries on its own, and a new spend on the seat waits.
	for _, body := range []string{
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "full"}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": "another-request"}),
	} {
		if code, answer := f.spend(t, body); code != http.StatusConflict {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	for _, body := range []string{
		spendBody(map[string]any{"auth_index": f.index, "kind": "full", "retry_request_id": requestID}),
		spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "grant_id": "fable-late", "retry_request_id": requestID}),
	} {
		if code, answer := f.spend(t, body); code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d answer=%v", body, code, answer)
		}
	}
	if claimCount() != 1 {
		t.Fatalf("claims=%d", claimCount())
	}
	// A second credential file of the same account is held too; a seat of
	// another account is not.
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "weekly"})); code != http.StatusConflict {
		t.Fatalf("twin seat: status=%d answer=%v", code, answer)
	}
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "other"), "kind": "weekly"})); code != http.StatusOK || answer["grant_id"] != "weekly-1" {
		t.Fatalf("other seat: status=%d answer=%v", code, answer)
	}
	if claimCount() != 2 {
		t.Fatalf("claims=%d", claimCount())
	}
	// A refusal on a retry cannot prove the first claim did not spend.
	f.clock.advance(4 * time.Minute)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus, p.claimResult = 0, "cooldown" })
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if code != http.StatusOK || answer["result"] != "cooldown" || answer["request_id"] != requestID || answer["retry"] != true {
		t.Fatalf("retry: status=%d answer=%v", code, answer)
	}
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusConflict {
		t.Fatalf("still unsettled: status=%d answer=%v", code, answer)
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimResult = "reset" })
	code, answer = f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["request_id"] != requestID {
		t.Fatalf("settling retry: status=%d answer=%v", code, answer)
	}
	_, _, claims := f.provider.counts()
	if len(claims) != 4 || claims[0].RequestID != requestID {
		t.Fatalf("claims=%+v", claims)
	}
	for _, claim := range claims[2:] {
		if claim.RequestID != requestID || claim.GrantID != "fable-soon" {
			t.Fatalf("a retry changed the claim: %+v", claims)
		}
	}
	// Settled: the request id cannot be sent again.
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})); code != http.StatusConflict {
		t.Fatalf("settled retry: status=%d answer=%v", code, answer)
	}

	// Ten minutes after an unknown outcome the retry is refused, and the seat
	// takes a new, deliberate spend again.
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusInternalServerError })
	_, lost := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
	lostID, _ := lost["request_id"].(string)
	if lost["result"] != "unknown" || lostID == "" {
		t.Fatalf("answer=%v", lost)
	}
	f.clock.advance(10 * time.Minute)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full", "retry_request_id": lostID})); code != http.StatusConflict {
		t.Fatalf("expired retry: status=%d answer=%v", code, answer)
	}
	code, answer = f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["request_id"] == lostID {
		t.Fatalf("after expiry: status=%d answer=%v", code, answer)
	}
}

func TestClaudeResetSpendWritesOneRecordPerSpend(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	if code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	before, _ := answer["kinds_before"].(map[string]any)
	after, _ := answer["kinds_after"].(map[string]any)
	fableBefore, _ := before["fable"].(map[string]any)
	fableAfter, _ := after["fable"].(map[string]any)
	if fableBefore["resets_left"] != float64(2) || fableAfter["resets_left"] != float64(1) {
		t.Fatalf("before=%v after=%v", before, after)
	}

	file, err := os.Open(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if errDecode := json.Unmarshal(scanner.Bytes(), &record); errDecode != nil {
			t.Fatalf("ledger line is not JSON: %s", scanner.Text())
		}
		records = append(records, record)
	}
	// Two lines per spend: "sent", synced before the claim left, and
	// "answered", the record of the outcome.
	if len(records) != 2 || records[0]["phase"] != "sent" || records[0]["result"] != "unknown" || records[0]["request_id"] != answer["request_id"] || records[1]["phase"] != "answered" {
		t.Fatalf("records=%v", records)
	}
	record := records[1]
	for _, field := range []string{"phase", "at", "auth_index", "kind", "grant_id", "request_id", "result", "resets_left_before", "resets_left_after"} {
		if _, ok := record[field]; !ok {
			t.Fatalf("record lacks %s: %v", field, record)
		}
	}
	leftBefore, _ := record["resets_left_before"].(map[string]any)
	leftAfter, _ := record["resets_left_after"].(map[string]any)
	if record["auth_index"] != f.index || record["kind"] != "fable" || record["grant_id"] != "fable-soon" || record["result"] != "reset" || leftBefore["fable"] != float64(2) || leftAfter["fable"] != float64(1) {
		t.Fatalf("record=%v", record)
	}
	raw, _ := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if strings.Contains(string(raw), resetFixtureToken) || strings.Contains(string(raw), resetFixtureOrganization) || strings.Contains(string(raw), "example.invalid") {
		t.Fatalf("ledger holds a secret or an identity: %s", raw)
	}

	// The credentials read shows the last spend per seat, and a hub restarted
	// on the same log directory still shows it.
	restarted := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: f.logDir}
	for _, h := range []*Handler{f.h, restarted} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
		ctx.Set(ConfigV8ContextKey, true)
		h.ListAuthFiles(ctx)
		var list struct {
			Files []map[string]any `json:"files"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &list); errDecode != nil {
			t.Fatal(errDecode)
		}
		seen := 0
		for _, entry := range list.Files {
			last, present := entry["claude_last_reset_spend"]
			switch entry["id"] {
			case "fixture":
				spent, _ := last.(map[string]any)
				if spent["kind"] != "fable" || spent["result"] != "reset" || spent["at"] == nil {
					t.Fatalf("fixture entry=%v", entry)
				}
				seen++
			case "other":
				if !present || last != nil {
					t.Fatalf("a seat never spent through this hub reads null: %v", entry)
				}
				seen++
			case "codex":
				if present {
					t.Fatalf("a Codex seat carries a Claude spend field: %v", entry)
				}
				seen++
			}
		}
		if seen != 3 {
			t.Fatalf("list=%s", rec.Body)
		}
	}
}

// The hold is on disk before the claim leaves, so a hub that restarts (or
// dies) while an answer is outstanding still refuses a new claim on the seat
// and still accepts the retry with the same request id.
func TestClaudeResetSpendHoldSurvivesARestart(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	ledgerPath := filepath.Join(f.logDir, claudeResetSpendLedgerName)
	f.provider.set(func(p *fakeResetProvider) {
		p.claimStatus = http.StatusBadGateway
		p.onClaim = func(claim fakeResetClaim) {
			raw, err := os.ReadFile(ledgerPath)
			if err != nil || !strings.Contains(string(raw), `"phase":"sent"`) || !strings.Contains(string(raw), claim.RequestID) {
				t.Errorf("the claim left before its sent line was on disk: %v %s", err, raw)
			}
		}
	})
	_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	if first["result"] != "unknown" || requestID == "" {
		t.Fatalf("answer=%v", first)
	}
	restart := func() *Handler {
		h := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: f.logDir, claudeResetClient: f.h.claudeResetClient}
		h.claudeResetSpends.now = f.clock.now
		return h
	}
	f.h = restart()
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusConflict {
		t.Fatalf("after restart: status=%d answer=%v", code, answer)
	}
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus, p.onClaim = 0, nil })
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
	if code != http.StatusOK || answer["result"] != "reset" || answer["request_id"] != requestID {
		t.Fatalf("retry after restart: status=%d answer=%v", code, answer)
	}
	// Settled on disk too: a second restart lets the seat spend again.
	f.h = restart()
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusOK || answer["result"] != "reset" {
		t.Fatalf("after settling: status=%d answer=%v", code, answer)
	}

	// A hub that died after the sent line, before any answer.
	g := newResetSpendFixture(t, resetFixtureGrants()...)
	sent, _ := json.Marshal(claudeResetSpendRecord{Phase: claudeResetPhaseSent, At: g.clock.now().Add(-time.Minute), AuthIndex: g.index, Kind: "fable", GrantID: "fable-late", RequestID: "restored-request", Result: "unknown"})
	if err := os.WriteFile(filepath.Join(g.logDir, claudeResetSpendLedgerName), append(sent, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, answer := g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable"})); code != http.StatusConflict {
		t.Fatalf("restored hold: status=%d answer=%v", code, answer)
	}
	code, answer = g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable", "retry_request_id": "restored-request"}))
	_, _, claims := g.provider.counts()
	if code != http.StatusOK || len(claims) != 1 || claims[0].RequestID != "restored-request" || claims[0].GrantID != "fable-late" {
		t.Fatalf("restored retry: status=%d answer=%v claims=%+v", code, answer, claims)
	}
}

func TestClaudeResetSpendRefusesWhenTheLedgerCannotBeWritten(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.h.logDir = blocked
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 0 {
		t.Fatalf("a claim left without its ledger line: %+v", claims)
	}
}

func (f *resetSpendFixture) restart() {
	h := &Handler{cfg: f.h.cfg, authManager: f.h.authManager, logDir: f.logDir, claudeResetClient: f.h.claudeResetClient}
	h.claudeResetSpends.now = f.clock.now
	f.h = h
}

// Codex round 1, class "claim identity not persisted": a hold restored from
// the ledger still binds its account, for a new claim on another credential
// file of that account and for the retry itself.
func TestClaudeResetSpendRestartKeepsTheAccountBinding(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	if first["result"] != "unknown" || requestID == "" {
		t.Fatalf("answer=%v", first)
	}
	raw, _ := os.ReadFile(filepath.Join(f.logDir, claudeResetSpendLedgerName))
	if strings.Contains(string(raw), resetFixtureOrganization) {
		t.Fatalf("the ledger holds the organization: %s", raw)
	}
	f.restart()
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = 0 })
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "weekly"})); code != http.StatusConflict {
		t.Fatalf("twin after restart: status=%d answer=%v", code, answer)
	}
	// The seat's credential now belongs to another account: the retry must
	// not claim the original grant id against the replacement organization.
	for _, auth := range f.h.authManager.List() {
		if auth.ID == "fixture" {
			auth.Metadata = map[string]any{"access_token": resetFixtureOtherToken}
			if _, err := f.h.authManager.Update(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID})); code != http.StatusConflict {
		t.Fatalf("retry on a replaced credential: status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 1 {
		t.Fatalf("claims=%+v", claims)
	}
}

// Codex round 1, class "account serialization incomplete": a retry holds the
// account while its claim is in flight, even past the retry window.
func TestClaudeResetSpendRetryHoldsTheAccountWhileInFlight(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
	_, first := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
	requestID, _ := first["request_id"].(string)
	f.clock.advance(9 * time.Minute)
	arrived, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	f.provider.set(func(p *fakeResetProvider) {
		p.claimStatus = 0
		// Only the retry's claim is held; any later claim passes straight on.
		p.onClaim = func(fakeResetClaim) {
			if held.CompareAndSwap(false, true) {
				close(arrived)
				<-release
			}
		}
	})
	done := make(chan int, 1)
	go func() {
		code, _ := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable", "retry_request_id": requestID}))
		done <- code
	}()
	<-arrived
	f.clock.advance(2 * time.Minute)
	code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.seatIndex(t, "twin"), "kind": "weekly"}))
	close(release)
	if retried := <-done; retried != http.StatusOK {
		t.Fatalf("retry status=%d", retried)
	}
	if code != http.StatusConflict {
		t.Fatalf("twin during an in-flight retry: status=%d answer=%v", code, answer)
	}
	if _, _, claims := f.provider.counts(); len(claims) != 2 {
		t.Fatalf("claims=%+v", claims)
	}
}

// Codex round 1, class "ledger recovery fails open": a torn tail never hides
// the next hold, and a ledger the hub cannot read or trust stops every spend.
func TestClaudeResetSpendLedgerRecoveryNeverFailsOpen(t *testing.T) {
	complete, _ := json.Marshal(claudeResetSpendRecord{Phase: claudeResetPhaseAnswered, AuthIndex: "elsewhere", RequestID: "earlier", Result: "reset"})
	for name, tail := range map[string][]byte{"torn tail": []byte(`{"phase":`), "cut after a complete record": complete} {
		t.Run(name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			if err := os.WriteFile(filepath.Join(f.logDir, claudeResetSpendLedgerName), tail, 0o600); err != nil {
				t.Fatal(err)
			}
			f.provider.set(func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway })
			if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK || answer["result"] != "unknown" {
				t.Fatalf("status=%d answer=%v", code, answer)
			}
			for restart := 0; restart < 2; restart++ {
				f.restart()
				if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "full"})); code != http.StatusConflict || strings.Contains(fmt.Sprint(answer["error"]), "cannot be read") {
					t.Fatalf("restart %d: status=%d answer=%v", restart, code, answer)
				}
			}
		})
	}
	for name, prepare := range map[string]func(path string) error{
		"corrupt line": func(path string) error {
			return os.WriteFile(path, []byte("not a record\n"), 0o600)
		},
		"record without a phase": func(path string) error {
			return os.WriteFile(path, []byte(`{"auth_index":"x","request_id":"y"}`+"\n"), 0o600)
		},
		"unreadable": func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			if err := prepare(filepath.Join(f.logDir, claudeResetSpendLedgerName)); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: status=%d answer=%v", attempt, code, answer)
				}
			}
			if requests, _, _ := f.provider.counts(); requests != 0 {
				t.Fatalf("a spend reached the provider over a ledger it cannot trust: %d", requests)
			}
		})
	}
}

// Codex round 1, class "stale clock": a grant is judged at the time of the
// claim, not at the time the spend began.
func TestClaudeResetSpendJudgesGrantsAtTheTimeOfTheClaim(t *testing.T) {
	at := func(f *resetSpendFixture, d time.Duration) string { return f.clock.now().Add(d).Format(time.RFC3339) }
	t.Run("selection after the read", func(t *testing.T) {
		f := newResetSpendFixture(t)
		f.provider.set(func(p *fakeResetProvider) {
			p.grants = []fakeResetGrant{
				{ID: "fable-a", ResetsLeft: 1, EndsAt: at(f, time.Second), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true},
				{ID: "fable-b", ResetsLeft: 1, EndsAt: at(f, time.Hour), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true},
			}
			p.onRead = func() { f.clock.advance(2 * time.Second) }
		})
		code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"}))
		if code != http.StatusOK || answer["grant_id"] != "fable-b" {
			t.Fatalf("status=%d answer=%v", code, answer)
		}
	})
	t.Run("expiry before the claim", func(t *testing.T) {
		g := newResetSpendFixture(t)
		g.provider.set(func(p *fakeResetProvider) {
			p.grants = []fakeResetGrant{{ID: "fable-c", ResetsLeft: 1, EndsAt: at(g, 5*time.Second), Type: "full", GivesBackWeekly: true, Clears: []string{}, UsableNow: true}}
			p.onProfile = func() { g.clock.advance(10 * time.Second) }
		})
		if code, answer := g.spend(t, spendBody(map[string]any{"auth_index": g.index, "kind": "fable"})); code != http.StatusConflict {
			t.Fatalf("expired before the claim: status=%d answer=%v", code, answer)
		}
		if _, _, claims := g.provider.counts(); len(claims) != 0 {
			t.Fatalf("an expired grant was claimed: %+v", claims)
		}
	})
}

// Codex round 1, class "unsynchronized configuration": a spend reads the
// configuration once, under the handler's lock, while reloads replace it.
// Run with -race to see the race it guards against.
func TestClaudeResetSpendReadsConfigurationUnderTheLock(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	// The spend works from its own copy: the live object is edited in place
	// by management writes, so holding it after the lock is released races.
	build := f.h.claudeResetClient
	f.h.claudeResetClient = func(cfg *config.Config, proxyURL string) *claudeauth.ClaudeAuth {
		f.h.mu.Lock()
		live := cfg == f.h.cfg
		f.h.mu.Unlock()
		if live {
			t.Error("the spend holds the live configuration object")
		}
		return build(cfg, proxyURL)
	}
	stop := make(chan struct{})
	reloaded := make(chan struct{})
	go func() {
		defer close(reloaded)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%10 == 0 {
				f.h.SetConfig(&config.Config{})
				continue
			}
			// Management writes edit the live configuration in place, under
			// the handler's lock (updateStringField, PutProxyURL).
			f.h.mu.Lock()
			f.h.cfg.ProxyURL = ""
			f.h.mu.Unlock()
		}
	}()
	// "already_used" leaves the grants in place, so every spend reaches the claim.
	f.provider.set(func(p *fakeResetProvider) { p.claimResult = "already_used" })
	for i := 0; i < 30; i++ {
		kind := []string{"fable", "full", "weekly"}[i%3]
		if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": kind})); code != http.StatusOK {
			t.Fatalf("%s: status=%d answer=%v", kind, code, answer)
		}
	}
	close(stop)
	<-reloaded
}

// Codex round 3, class "provider reset not reconciled with routing": a reset
// the provider confirms lifts the hub's own cooldowns on the seat, as the
// Codex reset does; an unknown outcome or a refusal leaves them in place.
func TestClaudeResetSpendLiftsTheHubCooldownOnlyAfterAReset(t *testing.T) {
	block := func(t *testing.T, f *resetSpendFixture) {
		t.Helper()
		next := time.Now().Add(7 * 24 * time.Hour)
		auth, ok := f.h.authManager.GetByID("fixture")
		if !ok {
			t.Fatal("fixture seat missing")
		}
		auth.Status, auth.StatusMessage, auth.Unavailable, auth.NextRetryAfter = coreauth.StatusError, "quota exhausted", true, next
		auth.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2}
		auth.ModelStates = map[string]*coreauth.ModelState{"claude-fixture-model": {
			Status: coreauth.StatusError, StatusMessage: "quota exhausted", Unavailable: true, NextRetryAfter: next,
			Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		}}
		if _, err := f.h.authManager.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		if blocked, _ := f.h.authManager.GetByID("fixture"); !blocked.Unavailable || !blocked.Quota.Exceeded {
			t.Fatalf("precondition: the seat is not blocked: %+v", blocked.Quota)
		}
	}
	blocked := func(f *resetSpendFixture) bool {
		auth, _ := f.h.authManager.GetByID("fixture")
		state := auth.ModelStates["claude-fixture-model"]
		return auth.Unavailable || auth.Quota.Exceeded || !auth.NextRetryAfter.IsZero() || (state != nil && (state.Unavailable || state.Quota.Exceeded))
	}
	for name, tc := range map[string]struct {
		update func(*fakeResetProvider)
		lifted bool
	}{
		"reset":        {func(*fakeResetProvider) {}, true},
		"already used": {func(p *fakeResetProvider) { p.claimResult = "already_used" }, true},
		"unknown":      {func(p *fakeResetProvider) { p.claimStatus = http.StatusBadGateway }, false},
		"refused":      {func(p *fakeResetProvider) { p.claimResult = "cooldown" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newResetSpendFixture(t, resetFixtureGrants()...)
			block(t, f)
			f.provider.set(tc.update)
			if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "weekly"})); code != http.StatusOK {
				t.Fatalf("status=%d answer=%v", code, answer)
			}
			if blocked(f) == tc.lifted {
				auth, _ := f.h.authManager.GetByID("fixture")
				t.Fatalf("lifted=%v want %v: unavailable=%v quota=%+v", !blocked(f), tc.lifted, auth.Unavailable, auth.Quota)
			}
		})
	}
}

// Codex round 2: a ledger the hub cannot read is reported as such by both
// reads, never as "never spent through this hub".
func TestClaudeResetSpendHistoryReportsAnUnreadableLedger(t *testing.T) {
	f := newResetSpendFixture(t, resetFixtureGrants()...)
	if code, answer := f.spend(t, spendBody(map[string]any{"auth_index": f.index, "kind": "fable"})); code != http.StatusOK {
		t.Fatalf("status=%d answer=%v", code, answer)
	}
	ledger, err := os.OpenFile(filepath.Join(f.logDir, claudeResetSpendLedgerName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.WriteString("not a record\n"); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	f.restart()

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	ctx.Set(ConfigV8ContextKey, true)
	f.h.ListAuthFiles(ctx)
	var list struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &list); errDecode != nil {
		t.Fatal(errDecode)
	}
	claudeSeats := 0
	for _, entry := range list.Files {
		if entry["provider"] != "claude" {
			continue
		}
		claudeSeats++
		if entry["claude_last_reset_spend"] != nil || entry["claude_last_reset_spend_error"] == nil {
			t.Fatalf("credentials entry hides the unreadable ledger: %v", entry)
		}
	}
	if claudeSeats != 3 {
		t.Fatalf("list=%s", rec.Body)
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/claude/limit-resets", nil)
	f.h.GetClaudeLimitResets(ctx)
	var resets struct {
		Seats []map[string]any `json:"seats"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &resets); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(resets.Seats) != 3 {
		t.Fatalf("limit-resets=%s", rec.Body)
	}
	for _, seat := range resets.Seats {
		if seat["last_spend"] != nil || seat["last_spend_error"] == nil {
			t.Fatalf("limit-resets seat hides the unreadable ledger: %v", seat)
		}
	}
}
