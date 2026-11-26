package executor

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func oneHourKeyOffConfig() *config.Config {
	off := false
	cfg := &config.Config{}
	cfg.ClaudeHeaderDefaults.OneHourCache = &off
	return cfg
}

// Guard: with claude-header-defaults.one-hour-cache off, a native main
// conversation on a subscription seat goes upstream as the client wrote it.
func TestClaudeOneHourCacheKeyOffLeavesNativeRequestUnchanged(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			upstream, _, err := runOneHourCall(t, oneHourCall{cfg: oneHourKeyOffConfig(), stream: stream, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			for label, raw := range oneHourMarkers(t, upstream.body(0)) {
				if raw != oneHourBare {
					t.Fatalf("%s = %s, want %s with the key off", label, raw, oneHourBare)
				}
			}
		})
	}
}

// Test 4, the parts that need the meter: the re-sent request is byte for byte
// the request the hub sends with the rule off (same body, same betas), and the
// refusal is counted once.
func TestClaudeOneHourCacheRefusalResendMatchesRuleOffAndIsCounted(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			payload := oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")
			ruleOff, _, err := runOneHourCall(t, oneHourCall{cfg: oneHourKeyOffConfig(), stream: stream, payload: payload})
			if err != nil {
				t.Fatalf("rule-off request error = %v", err)
			}
			before := oneHourRuleCounts()
			refused, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: payload, refuse: oneHourRefuseHour})
			if err != nil {
				t.Fatalf("caller error = %v", err)
			}
			if got := refused.count(); got != 2 {
				t.Fatalf("upstream saw %d requests, want 2", got)
			}
			if !bytes.Equal(refused.body(1), ruleOff.body(0)) {
				t.Fatalf("re-sent body differs from the rule-off body:\nre-sent:  %s\nrule off: %s", refused.body(1), ruleOff.body(0))
			}
			if got, want := refused.header(1).Get("Anthropic-Beta"), ruleOff.header(0).Get("Anthropic-Beta"); got != want {
				t.Fatalf("re-sent betas = %q, want the rule-off betas %q", got, want)
			}
			after := oneHourRuleCounts()
			if got := after.Refused - before.Refused; got != 1 {
				t.Fatalf("refused counted %d times, want 1", got)
			}
			if got := after.Applied - before.Applied; got != 0 {
				t.Fatalf("applied counted %d, want 0: the reply came from the request sent with the rule off", got)
			}
		})
	}
}

func oneHourRuleCounts() helps.ClaudeCacheRuleCounts {
	snapshot := helps.DefaultClaudeCacheMeter().Snapshot()
	if len(snapshot.Days) == 0 {
		return helps.ClaudeCacheRuleCounts{}
	}
	return snapshot.Days[0].Rule
}

func oneHourMeterRow(client, kind string) helps.ClaudeCacheClientRow {
	snapshot := helps.DefaultClaudeCacheMeter().Snapshot()
	if len(snapshot.Days) == 0 {
		return helps.ClaudeCacheClientRow{}
	}
	for _, row := range snapshot.Days[0].Clients {
		if row.Client == client && row.Kind == kind {
			return row
		}
	}
	return helps.ClaudeCacheClientRow{}
}

// The feed: a successful Messages reply is metered under the router's client
// hash (its first 12 characters) and the request's kind; token counting is not.
func TestClaudeOneHourCacheMeterFeed(t *testing.T) {
	subagent := oneHourNativeHeaders()
	subagent.Set("X-Claude-Code-Agent-Id", "agent-one-hour-meter")
	cases := []struct {
		name        string
		hash        string
		kind        string
		ruleApplied int64
	}{
		{name: "main", hash: "a1b2c3d4e5f60718293a4b5c6d7e8f90", kind: helps.ClaudeCacheKindMain, ruleApplied: 1},
		{name: "subagent", hash: "b1b2c3d4e5f60718293a4b5c6d7e8f90", kind: helps.ClaudeCacheKindSubagent},
	}
	for _, tc := range cases {
		for _, stream := range oneHourPaths {
			t.Run(tc.name+"/"+oneHourPathName(stream), func(t *testing.T) {
				headers := oneHourNativeHeaders()
				if tc.kind == helps.ClaudeCacheKindSubagent {
					headers = subagent
				}
				client := tc.hash[:12]
				before := oneHourMeterRow(client, tc.kind)
				_, _, err := runOneHourCall(t, oneHourCall{stream: stream, headers: headers, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", ""),
					metadata: map[string]any{cliproxyexecutor.ClaudeSeatClientHashMetadataKey: tc.hash}})
				if err != nil {
					t.Fatalf("request error = %v", err)
				}
				after := oneHourMeterRow(client, tc.kind)
				if got := after.Requests - before.Requests; got != 1 {
					t.Fatalf("requests += %d, want 1", got)
				}
				if got := after.Write1h - before.Write1h; got != 100 {
					t.Fatalf("write_1h += %d, want 100", got)
				}
				if got := after.Read - before.Read; got != 40 {
					t.Fatalf("read += %d, want 40", got)
				}
				if got := after.RuleApplied - before.RuleApplied; got != tc.ruleApplied {
					t.Fatalf("rule_applied += %d, want %d", got, tc.ruleApplied)
				}
			})
		}
	}
	t.Run("token counting is not metered", func(t *testing.T) {
		const hash = "c1b2c3d4e5f60718293a4b5c6d7e8f90"
		_, _, err := runOneHourCall(t, oneHourCall{count: true, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", ""),
			metadata: map[string]any{cliproxyexecutor.ClaudeSeatClientHashMetadataKey: hash}})
		if err != nil {
			t.Fatalf("count error = %v", err)
		}
		for _, kind := range []string{helps.ClaudeCacheKindMain, helps.ClaudeCacheKindSubagent, helps.ClaudeCacheKindHelper} {
			if row := oneHourMeterRow(hash[:12], kind); row.Requests != 0 {
				t.Fatalf("token counting metered as %s: %+v", kind, row)
			}
		}
	})
}

// The executor's credential test and the seat router's are one predicate:
// they agree on every way a seat can carry its credential.
func TestClaudeSubscriptionSeatPredicateMatchesExecutor(t *testing.T) {
	for _, auth := range []*cliproxyauth.Auth{
		{Attributes: map[string]string{"api_key": "sk-ant-oat-attribute"}},
		{Attributes: map[string]string{"api_key": "sk-ant-api03-attribute"}},
		{Metadata: map[string]any{"access_token": "sk-ant-oat-metadata"}},
		{Metadata: map[string]any{"access_token": "plain-metadata-token"}},
		{Attributes: map[string]string{"api_key": "sk-ant-api03-wins"}, Metadata: map[string]any{"access_token": "sk-ant-oat-loses"}},
		{Attributes: map[string]string{"base_url": "https://gateway.invalid"}, Metadata: map[string]any{"access_token": "sk-ant-oat-gateway"}},
		{},
	} {
		apiKey, _ := claudeCreds(auth)
		if got, want := cliproxyauth.IsClaudeSubscriptionSeat(auth), isClaudeOAuthToken(apiKey); got != want {
			t.Fatalf("router says %v, executor says %v for %+v", got, want, auth)
		}
	}
}
