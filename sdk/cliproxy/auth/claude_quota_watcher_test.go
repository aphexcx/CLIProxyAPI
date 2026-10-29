package auth_test

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type watcherQuotaPoller struct{ coreauth.ProviderExecutor }

func (*watcherQuotaPoller) Identifier() string { return "claude" }
func (*watcherQuotaPoller) PollQuota(context.Context, *coreauth.Auth) (map[string]coreauth.QuotaWindow, error) {
	return map[string]coreauth.QuotaWindow{"Fable": {Utilization: 20, ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now()}}, nil
}

func TestClaudeQuotaWatcherSnapshotPreservesRankAndDoesNotPersistUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, path := newTrafficManager(t)
		_, _, errModify := manager.Modify(context.Background(), trafficAuthID, 0, func(auth *coreauth.Auth) error {
			auth.Metadata["access_token"] = "test-access"
			return nil
		})
		if errModify != nil {
			t.Fatal(errModify)
		}
		cfg := &config.Config{RequestRetry: 3}
		cfg.Claude.Quota.Enabled = true
		cfg.Claude.Quota.PriorityAtReset = true
		manager.SetConfig(cfg)
		manager.RegisterExecutor(&watcherQuotaPoller{})
		manager.StartQuotaPolling(context.Background())
		t.Cleanup(manager.StopQuotaPolling)
		synctest.Wait()
		before, _ := manager.GetByID(trafficAuthID)
		if before.Attributes["quota_rank"] != "1" || len(before.Quota.Windows) != 1 {
			t.Fatal("poll did not publish runtime quota")
		}
		snapshot := readTrafficSnapshot(t, path, "changed-model")
		if snapshot.Attributes["quota_rank"] != "" {
			t.Fatal("watcher synthesized runtime rank from disk")
		}
		requireApplied(t, manager, snapshot)
		after, _ := manager.GetByID(trafficAuthID)
		if after.Attributes["quota_rank"] != "1" || !reflect.DeepEqual(before.Quota.Windows, after.Quota.Windows) {
			t.Fatal("watcher update lost rank/windows")
		}
		// Force an ordinary persisted edit after polling, then inspect the auth file.
		if _, _, errModify := manager.Modify(context.Background(), trafficAuthID, 0, func(auth *coreauth.Auth) error { auth.Metadata["note"] = "updated"; return nil }); errModify != nil {
			t.Fatal(errModify)
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if strings.Contains(string(data), "quota_rank") || strings.Contains(string(data), "windows") || strings.Contains(string(data), "utilization") {
			t.Fatal("runtime quota leaked to auth file")
		}
	})
}
