package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// renderedSeatMoveLine formats the one captured entry with the given message
// through the server's own log formatter and returns the printed line with its
// space-separated tokens, so a field the formatter drops is absent here even
// though it is present in the entry's data. Every field the entry carries must
// be printed, so a field added to the log call later without a formatter entry
// fails here too.
func renderedSeatMoveLine(t *testing.T, hook *logtest.Hook, message string) (string, map[string]struct{}) {
	t.Helper()
	var matched []*log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Message == message {
			matched = append(matched, entry)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("%q lines = %d, want 1", message, len(matched))
	}
	formatted, errFormat := (&logging.LogFormatter{}).Format(matched[0])
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	line := strings.TrimRight(string(formatted), "\n")
	tokens := map[string]struct{}{}
	printed := map[string]struct{}{}
	for _, token := range strings.Fields(line) {
		tokens[token] = struct{}{}
		if key, _, found := strings.Cut(token, "="); found {
			printed[key] = struct{}{}
		}
	}
	for key := range matched[0].Data {
		if _, ok := printed[key]; !ok {
			t.Errorf("rendered %q line drops the entry's field %s:\n%s", message, key, line)
		}
	}
	return line, tokens
}

// The "client moved" and "move outcome" lines must print their fields in the
// server's log, not only carry them in the entry: a conversation is served on
// one seat, that seat is disabled during its hold, and the conversation moves
// to the other seat of the same organization. The fixture reply reads 40 and
// writes 100 with 3 uncached input tokens, so the move is RE-PAID. Both lines
// are rendered through logging.LogFormatter and checked as printed text.
func TestClaudeSeatMoveLogLinesRenderTheirFields(t *testing.T) {
	// A made-up organization; no real account is named.
	const organization = "00000000-0000-4000-8000-00000000000b"
	digest := sha256.Sum256([]byte(organization))
	organizationLogName := hex.EncodeToString(digest[:])[:8]

	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			logger := log.StandardLogger()
			previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
			previousLevel := logger.GetLevel()
			previousOutput := logger.Out
			logger.SetLevel(log.InfoLevel)
			logger.SetOutput(io.Discard)
			hook := logtest.NewLocal(logger)
			t.Cleanup(func() {
				logger.SetLevel(previousLevel)
				logger.ReplaceHooks(previousHooks)
				logger.SetOutput(previousOutput)
			})

			cfg := &config.Config{}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewClaudeExecutor(cfg))
			upstreams := map[string]*oneHourUpstream{}
			for _, suffix := range []string{"a", "b"} {
				server, upstream := newOneHourUpstream(t, nil)
				id := "seat-move-log-" + t.Name() + "-" + suffix
				upstreams[id] = upstream
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: oneHourModel}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
					ID: id, Provider: "claude", Status: cliproxyauth.StatusActive,
					Attributes: map[string]string{"api_key": oneHourKeySeatKey, "base_url": server.URL},
					Metadata:   map[string]any{"organization_uuid": organization},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			payload := []byte(`{"model":"` + oneHourModel + `","max_tokens":16,"stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hello"}]}`)
			call := func() {
				t.Helper()
				opts := cliproxyexecutor.Options{Stream: stream, SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
					Headers: http.Header{"X-Session-Id": []string{"seat-move-log-fixture-session"}}, Metadata: map[string]any{}}
				req := cliproxyexecutor.Request{Model: oneHourModel, Payload: payload}
				if !stream {
					if _, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					return
				}
				result, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			}

			call()
			first := ""
			for id, upstream := range upstreams {
				if upstream.count() == 1 {
					first = id
				}
			}
			if first == "" {
				t.Fatal("the first request reached no seat")
			}
			auth, found := manager.GetByID(first)
			if !found {
				t.Fatalf("seat %s is not registered", first)
			}
			auth.Disabled = true
			if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			call()

			for _, tc := range []struct {
				message string
				want    []string
			}{
				{message: "claude routing: client moved", want: []string{
					"previous_organization=" + organizationLogName,
					"organization=" + organizationLogName,
					"same_org_available=1",
				}},
				{message: "claude routing: move outcome", want: []string{
					"kind=main", "relation=same_org", "warm=true", "outcome=re_paid", "replaced=false",
					"read=40", "written=100", "input=3",
				}},
			} {
				line, tokens := renderedSeatMoveLine(t, hook, tc.message)
				for _, want := range tc.want {
					if _, ok := tokens[want]; !ok {
						t.Errorf("rendered %q line does not print %s:\n%s", tc.message, want, line)
					}
				}
			}
		})
	}
}
