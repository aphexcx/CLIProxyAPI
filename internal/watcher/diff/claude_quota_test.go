package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeQuotaReloadDiff(t *testing.T) {
	before := &config.Config{}
	before.NormalizeClaudeConfig()
	for name, edit := range map[string]func(*config.ClaudeQuotaConfig){
		"enabled":     func(q *config.ClaudeQuotaConfig) { q.Enabled = true },
		"interval":    func(q *config.ClaudeQuotaConfig) { q.PollInterval = "2m" },
		"cap":         func(q *config.ClaudeQuotaConfig) { *q.CapPercent = 99 },
		"window caps": func(q *config.ClaudeQuotaConfig) { q.WindowCaps["Fable"] = 99 },
		"models":      func(q *config.ClaudeQuotaConfig) { q.ModelWindows["Fable"] = []string{"another-model"} },
		"rank":        func(q *config.ClaudeQuotaConfig) { q.PriorityAtReset = true },
	} {
		t.Run(name, func(t *testing.T) {
			after := before.CloneForRuntime()
			edit(&after.Claude.Quota)
			if !slices.Contains(BuildConfigChangeDetails(before, after), "upstream.claude.quota: updated") {
				t.Fatal("quota change missing from reload diff")
			}
		})
	}
	if slices.Contains(BuildConfigChangeDetails(&config.Config{}, before), "upstream.claude.quota: updated") {
		t.Fatal("defaults create a spurious change")
	}
}
