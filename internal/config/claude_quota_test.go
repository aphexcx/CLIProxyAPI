package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestClaudeQuotaDefaultsAndV8RoundTrip(t *testing.T) {
	defaults, errParse := ParseConfigBytes([]byte("server: {port: 8317}"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	q := defaults.Claude.Quota
	if q.Enabled || q.PriorityAtReset || q.PollInterval != "1m" || q.CapFor("Fable") != 100 || !reflect.DeepEqual(q.ModelWindows, map[string][]string{"Fable": {"claude-fable-5-1"}}) {
		t.Fatalf("defaults=%+v", q)
	}
	data := []byte(`upstream:
  claude:
    quota:
      enabled: true
      poll-interval: 1m
      cap-percent: 100
      window-caps: {Fable: 99}
      model-windows: {Fable: [claude-fable-5-1]}
      priority-at-reset: true
`)
	if errValidate := ValidateV8Config(data); errValidate != nil {
		t.Fatal(errValidate)
	}
	cfg, errParse := ParseConfigBytes(data)
	if errParse != nil {
		t.Fatal(errParse)
	}
	encoded, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	encoded, _, errMarshal = NormalizeConfigLayout(encoded, true)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errValidate := ValidateV8Config(encoded); errValidate != nil {
		t.Fatal(errValidate)
	}
	var node yaml.Node
	if errDecode := yaml.Unmarshal(encoded, &node); errDecode != nil {
		t.Fatal(errDecode)
	}
	if yamlPath(node.Content[0], "upstream.claude.quota.model-windows.Fable") == nil {
		t.Fatal("canonical quota path missing")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, data, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	loaded, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if errSave := SaveConfigPreserveComments(path, cfg, true); errSave != nil {
		t.Fatal(errSave)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, raw := range [][]byte{encoded, saved} {
		roundTrip, errRoundTrip := ParseConfigBytes(raw)
		if errRoundTrip != nil {
			t.Fatal(errRoundTrip)
		}
		if !reflect.DeepEqual(cfg.Claude.Quota, roundTrip.Claude.Quota) || !reflect.DeepEqual(cfg.Claude.Quota, loaded.Claude.Quota) {
			t.Fatal("quota round trip changed config")
		}
	}
	clone := cfg.CloneForRuntime()
	clone.Claude.Quota.ModelWindows["Fable"][0] = "changed"
	clone.Claude.Quota.WindowCaps["Fable"] = 1
	*clone.Claude.Quota.CapPercent = 1
	if cfg.Claude.Quota.ModelWindows["Fable"][0] != "claude-fable-5-1" || cfg.Claude.Quota.CapFor("Fable") != 99 || *cfg.Claude.Quota.CapPercent != 100 {
		t.Fatal("runtime clone aliases quota config")
	}
}

func TestClaudeQuotaValidation(t *testing.T) {
	for _, body := range []string{
		"window-caps: {five_hour: 99}", "window-caps: {seven_day: 99}",
		"model-windows: {five_hour: [claude-fable-5-1]}", "model-windows: {seven_day: [claude-fable-5-1]}",
		"cap-percent: 0", "cap-percent: 101", "cap-percent: .nan", "window-caps: {Fable: -1}",
		"poll-interval: -1s", "poll-interval: invalid", "model-windows: {Fable: ['']}",
	} {
		t.Run(body, func(t *testing.T) {
			data := []byte("upstream:\n  claude:\n    quota:\n      " + body + "\n")
			if _, errParse := ParseConfigBytes(data); errParse == nil {
				t.Fatal("parse accepted invalid quota")
			}
			if errValidate := ValidateV8Config(data); errValidate == nil {
				t.Fatal("v8 accepted invalid quota")
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, data, 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			if _, errLoad := LoadConfig(path); errLoad == nil {
				t.Fatal("load accepted invalid quota")
			}
		})
	}
	cfg, errParse := ParseConfigBytes([]byte("upstream: {claude: {quota: {cap-percent: 99, window-caps: {five_hour: 100}, model-windows: {}}}}"))
	if errParse != nil || len(cfg.Claude.Quota.ModelWindows) != 0 {
		t.Fatalf("explicit empty model mapping: cfg=%v err=%v", cfg, errParse)
	}
}

func TestClaudeQuotaPollInterval(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("server: {port: 8317}"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	interval, errDuration := time.ParseDuration(cfg.Claude.Quota.PollInterval)
	if errDuration != nil || interval != time.Minute {
		t.Errorf("default interval=%v err=%v", interval, errDuration)
	}
	for _, interval := range []string{"10s", "15s", "59.999s", "1m", "2m"} {
		t.Run(interval, func(t *testing.T) {
			data := []byte("upstream: {claude: {quota: {poll-interval: " + interval + "}}}")
			_, errParse := ParseConfigBytes(data)
			errValidate := ValidateV8Config(data)
			for _, err := range []error{errParse, errValidate} {
				if interval == "10s" || interval == "15s" || interval == "59.999s" {
					if err == nil || !strings.Contains(err.Error(), "at least 1m") {
						t.Fatalf("interval %s must explain the minimum: %v", interval, err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
