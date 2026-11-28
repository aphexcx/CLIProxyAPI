package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestClaudeSeatHoldFilePathBesideConfig(t *testing.T) {
	if got := claudeSeatHoldFilePath(" "); got != "" {
		t.Fatalf("without a config path the holds stay in memory, got file %q", got)
	}
	dir := t.TempDir()
	first := claudeSeatHoldFilePath(filepath.Join(dir, "config.yaml"))
	second := claudeSeatHoldFilePath(filepath.Join(dir, "config-second.yaml"))
	if filepath.Dir(first) != dir || first == second {
		t.Fatalf("want one file per config beside it, got %q and %q", first, second)
	}
	if strings.HasSuffix(strings.ToLower(first), ".json") {
		t.Fatalf("hold file %q would read as a credential in an auth directory", first)
	}
}

// runSeatHoldService runs a hub from the config at cfgPath with one Claude
// API-key seat whose base URL is a closed local port, so nothing reaches a
// provider; seats are only picked here, never called. stop ends the run as a
// TERM does.
func runSeatHoldService(t *testing.T, cfgPath string) (service *Service, stop func()) {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if errClose := listener.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	authDir := filepath.Join(filepath.Dir(cfgPath), "auths")
	yaml := fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %q\nclaude-api-key:\n  - api-key: test-only-fixture-key\n    base-url: http://127.0.0.1:1\n    models:\n      - name: claude-opus-4-6\n", port, authDir)
	if errWrite := os.WriteFile(cfgPath, []byte(yaml), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(cfgPath)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(cfgPath).Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	var errDial error
	for i := 0; i < 100; i++ {
		var conn net.Conn
		if conn, errDial = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); errDial == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if errDial != nil {
		cancel()
		t.Fatalf("hub did not start: %v", errDial)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case errRun := <-done:
			if errRun != nil && !errors.Is(errRun, context.Canceled) {
				t.Errorf("run: %v", errRun)
			}
		case <-time.After(10 * time.Second):
			t.Error("hub did not stop")
		}
	}
	t.Cleanup(stop)
	return service, stop
}

// pickSeat routes one request of the conversation, as a request does before
// it is sent, and returns the seat and the routing reason.
func pickSeat(t *testing.T, service *Service, hook *logtest.Hook, conversation string) (seat, reason string) {
	t.Helper()
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hello"}]}`)
	auth, errSelect := service.coreManager.SelectAuth(context.Background(), "claude", "claude-opus-4-6", cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
		Headers: http.Header{"X-Session-Id": []string{conversation}}})
	if errSelect != nil {
		t.Fatal(errSelect)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Message == "claude routing: request" {
			reason, _ = entry.Data["reason"].(string)
		}
	}
	return auth.ID, reason
}

// The hub's own start and shutdown keep a conversation's hold across a
// restart: Run reads the file after the seats load, Shutdown writes it.
func TestServiceKeepsClaudeSeatHoldsAcrossRestart(t *testing.T) {
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(previousHooks) })
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	holdPath := claudeSeatHoldFilePath(cfgPath)

	first, stop := runSeatHoldService(t, cfgPath)
	seat, reason := pickSeat(t, first, hook, "seat-hold-service-conversation")
	if reason != "new_client" {
		t.Fatalf("setup: first placement %s (%s), want new_client", seat, reason)
	}
	stop()
	data, errRead := os.ReadFile(holdPath)
	if errRead != nil {
		t.Fatalf("shutdown wrote no hold file: %v", errRead)
	}
	if !strings.Contains(string(data), seat) {
		t.Fatalf("hold file does not hold seat %s: %s", seat, data)
	}

	hook.Reset()
	second, _ := runSeatHoldService(t, cfgPath)
	restored := 0
	for _, entry := range hook.AllEntries() {
		if entry.Message == "claude routing: seat holds restored" {
			restored, _ = entry.Data["holds_restored"].(int)
		}
	}
	if restored != 1 {
		t.Fatalf("start restored %d holds, want 1 (read after the seats load)", restored)
	}
	if got, reason := pickSeat(t, second, hook, "seat-hold-service-conversation"); got != seat || reason != "cache_warm" {
		t.Fatalf("after the restart the conversation went to %s (%s), want %s (cache_warm)", got, reason, seat)
	}
}
