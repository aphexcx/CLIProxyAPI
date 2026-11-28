package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// seatHoldFilePath is a hold file beside a made-up config in a fresh directory.
func seatHoldFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.yaml.claude-seat-holds")
}

// startSeatHolds is a hub that reads and keeps its hold file at path.
func startSeatHolds(t *testing.T, path string) (*seatRoutingFixture, *seatHoldClock) {
	t.Helper()
	f, clock := newSeatHoldFixture(t, false)
	f.m.StartClaudeSeatHolds(context.Background(), path)
	t.Cleanup(f.m.StopClaudeSeatHolds)
	return f, clock
}

// restartSeatHolds stops the hub, whose shutdown writes the hold file, and
// starts a fresh manager on the same seats pause later, as a release does.
// prepare runs on the fresh manager before it reads the file.
func restartSeatHolds(t *testing.T, before *seatRoutingFixture, clock *seatHoldClock, path string, pause time.Duration, prepare func(*seatRoutingFixture)) *seatRoutingFixture {
	t.Helper()
	before.m.StopClaudeSeatHolds()
	after, restarted := newSeatHoldFixture(t, false)
	restarted.now = clock.now.Add(pause)
	if prepare != nil {
		prepare(after)
	}
	after.m.StartClaudeSeatHolds(context.Background(), path)
	t.Cleanup(after.m.StopClaudeSeatHolds)
	return after
}

// startSeatHoldTicks keeps the hold file at path on ticks the test sends.
func startSeatHoldTicks(t *testing.T, path string) (*seatRoutingFixture, *seatHoldClock, chan time.Time) {
	t.Helper()
	f, clock := newSeatHoldFixture(t, false)
	ticks := make(chan time.Time)
	f.m.startClaudeSeatHolds(context.Background(), path, ticks, func() {})
	t.Cleanup(f.m.StopClaudeSeatHolds)
	return f, clock, ticks
}

// tick sends one save tick and returns once its save is done: the loop takes
// the second tick only after the first one's save returned. The second one's
// save may still run, so a test ticks only while every write succeeds (the
// second then finds nothing changed) and its clock stays still.
func tick(ticks chan time.Time) {
	ticks <- time.Time{}
	ticks <- time.Time{}
}

// saveTick runs what the loop runs on a tick, and returns when it is done.
func saveTick(f *seatRoutingFixture, path string) {
	f.m.saveClaudeSeatHoldsLogged(path)
}

func seatHoldFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return data
}

func seatHoldFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, errStat := os.Stat(path)
	if errStat != nil && !os.IsNotExist(errStat) {
		t.Fatal(errStat)
	}
	return errStat == nil
}

// renderedSeatHoldLine is the line the hub's log formatter prints for entry,
// which drops any field logFieldOrder does not name.
func renderedSeatHoldLine(t *testing.T, entry log.Entry) string {
	t.Helper()
	formatted, errFormat := (&logging.LogFormatter{}).Format(&entry)
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	return string(formatted)
}

// A conversation's hold is written at shutdown and read by the fresh manager a
// restart starts, so the conversation's next request stays on its seat.
func TestClaudeSeatHoldSurvivesRestart(t *testing.T) {
	path := seatHoldFilePath(t)
	before, clock := startSeatHolds(t, path)
	other, _ := before.requestMarker(t, "seat-hold-other", seatHoldBare)
	seat, reason := before.requestMarker(t, "seat-hold-restart", seatHoldBare)
	if seat == other || reason != "new_client" {
		t.Fatalf("setup: conversations on %s and %s (%s), want one on each seat", other, seat, reason)
	}
	clock.advance(time.Minute)
	after := restartSeatHolds(t, before, clock, path, 11*time.Second, nil)
	got, reason := after.requestMarker(t, "seat-hold-restart", seatHoldBare)
	if got != seat || reason != "cache_warm" {
		t.Fatalf("after the restart the conversation went to %s (%s), want its held seat %s (cache_warm)", got, reason, seat)
	}
}

func TestClaudeSeatHoldFileDropsStaleHoldsAtLoad(t *testing.T) {
	disable := func(status bool) func(*testing.T, *seatRoutingFixture, string) {
		return func(t *testing.T, f *seatRoutingFixture, seat string) {
			auth, _ := f.m.GetByID(seat)
			if status {
				auth.Status = StatusDisabled
			} else {
				auth.Disabled = true
			}
			if _, errUpdate := f.m.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
		}
	}
	edit := func(old, replacement string) func(*testing.T, string, string) {
		return func(t *testing.T, path, seat string) {
			old := strings.ReplaceAll(old, "<seat>", seat)
			data := string(seatHoldFileBytes(t, path))
			if !strings.Contains(data, old) {
				t.Fatalf("setup: the file has no %s: %s", old, data)
			}
			if errWrite := os.WriteFile(path, []byte(strings.ReplaceAll(data, old, replacement)), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		pause   time.Duration
		edit    func(t *testing.T, path, seat string)
		prepare func(t *testing.T, f *seatRoutingFixture, seat string)
		// count is the restore line's field that counts the drop; "" drops the file.
		count string
	}{
		{name: "expired", pause: 5 * time.Minute, count: "holds_expired"},
		{name: "disabled seat", prepare: disable(false), count: "holds_disabled_seat"},
		{name: "seat status disabled", prepare: disable(true), count: "holds_disabled_seat"},
		{name: "gone seat", edit: edit(`"auth_id":"<seat>"`, `"auth_id":"seat-hold-gone-fixture"`), count: "holds_gone_seat"},
		{name: "key not a client key", edit: edit(`"keys":["`, `"keys":["not-a-client-key","`), count: "holds_invalid"},
		{name: "other version", edit: edit(`"version":1`, `"version":2`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seatHoldFilePath(t)
			before, clock := startSeatHolds(t, path)
			seat, _ := before.requestMarker(t, "seat-hold-stale", seatHoldBare)
			before.m.StopClaudeSeatHolds()
			if tc.edit != nil {
				tc.edit(t, path, seat)
			}
			after := restartSeatHolds(t, before, clock, path, tc.pause, func(f *seatRoutingFixture) {
				if tc.prepare != nil {
					tc.prepare(t, f, seat)
				}
			})
			if tc.count == "" {
				dropped := after.events("claude routing: seat hold file dropped")
				if len(dropped) != 1 {
					t.Fatalf("want the file dropped once, got %+v", dropped)
				}
				if line := renderedSeatHoldLine(t, dropped[0]); !strings.Contains(line, "version=2 want_version=1") {
					t.Fatalf("the rendered line lost the versions: %s", line)
				}
			} else {
				restored := after.events("claude routing: seat holds restored")
				if len(restored) != 1 || restored[0].Data[tc.count] != 1 || restored[0].Data["holds_restored"] != 0 {
					t.Fatalf("want one hold dropped as %s and none restored, got %+v", tc.count, restored)
				}
				line := renderedSeatHoldLine(t, restored[0])
				for _, field := range []string{"holds_restored=0", tc.count + "=1"} {
					if !strings.Contains(line, field) {
						t.Fatalf("the rendered line lost %s: %s", field, line)
					}
				}
			}
			if _, reason := after.requestMarker(t, "seat-hold-stale", seatHoldBare); reason != "new_client" {
				t.Fatalf("a dropped hold still placed the conversation (%s), want new_client", reason)
			}
		})
	}
}

// A seat that capped while the hub was down answers the restored hold's first
// request with a 429; the existing retry moves the conversation, and the move
// is logged as one of a conversation that never restarted.
func TestClaudeSeatHoldRestoredOnCappedSeatMovesByRetry(t *testing.T) {
	path := seatHoldFilePath(t)
	before, clock := startSeatHolds(t, path)
	seat, _ := before.requestMarker(t, "seat-hold-capped", seatHoldBare)
	after := restartSeatHolds(t, before, clock, path, 11*time.Second, func(f *seatRoutingFixture) {
		f.m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude", executeFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			f.attempts = append(f.attempts, auth.ID)
			if auth.ID == seat {
				return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "fake seat capped"}
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		}})
	})
	got, reason := after.requestMarker(t, "seat-hold-capped", seatHoldBare)
	if got == seat || len(after.attempts) != 2 || after.attempts[0] != seat || after.attempts[1] != got {
		t.Fatalf("attempts %v ended on %s, want the held seat %s first, then another", after.attempts, got, seat)
	}
	requests := after.events("claude routing: request")
	if len(requests) != 2 || requests[0].Data["reason"] != "cache_warm" || reason != "upstream_error" {
		t.Fatalf("want the restored hold placed warm, then moved for upstream_error; got %+v", requests)
	}
	if moves := after.events("claude routing: client moved"); len(moves) != 1 || moves[0].Data["reason"] != "upstream_error" {
		t.Fatalf("want the restored hold's move logged once, got %+v", moves)
	}
}

// A restored hold whose seat cannot serve the conversation's first request
// after the restart (here a model cooldown the fresh hub knows) moves on that
// request, and the move is logged as one of a conversation that never
// restarted: the held seat is restored as the conversation's last seat too.
func TestClaudeSeatHoldRestoredOnCoolingSeatIsAMove(t *testing.T) {
	path := seatHoldFilePath(t)
	before, clock := startSeatHolds(t, path)
	seat, _ := before.requestMarker(t, "seat-hold-cooling", seatHoldBare)
	after := restartSeatHolds(t, before, clock, path, 11*time.Second, nil)
	retry := time.Hour
	after.m.MarkResult(context.Background(), Result{AuthID: seat, Provider: "claude", Model: seatOpus,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "fake model limit"}, RetryAfter: &retry})
	got, reason := after.requestMarker(t, "seat-hold-cooling", seatHoldBare)
	if got == seat || reason != "model_cooldown" {
		t.Fatalf("the conversation went to %s (%s), want another seat for model_cooldown", got, reason)
	}
	if moves := after.events("claude routing: client moved"); len(moves) != 1 || moves[0].Data["reason"] != "model_cooldown" {
		t.Fatalf("want the restored hold's move logged once, got %+v", moves)
	}
}

// A hold the fresh manager placed before it read the file is newer than the
// file's and stays.
func TestClaudeSeatHoldFileNeverReplacesALiveHold(t *testing.T) {
	path := seatHoldFilePath(t)
	before, clock := startSeatHolds(t, path)
	other, _ := before.requestMarker(t, "seat-hold-other", seatHoldBare)
	held, _ := before.requestMarker(t, "seat-hold-live", seatHoldBare)
	before.m.StopClaudeSeatHolds()
	after, restarted := newSeatHoldFixture(t, false)
	restarted.now = clock.now
	live, _ := after.requestMarker(t, "seat-hold-live", seatHoldBare)
	if live == held || held == other {
		t.Fatalf("setup: the file holds %s and the live hold is on %s, want two seats", held, live)
	}
	after.m.StartClaudeSeatHolds(context.Background(), path)
	t.Cleanup(after.m.StopClaudeSeatHolds)
	if got, reason := after.requestMarker(t, "seat-hold-live", seatHoldBare); got != live || reason != "cache_warm" {
		t.Fatalf("the file's hold replaced the live one: %s (%s), want %s (cache_warm)", got, reason, live)
	}
}

func TestClaudeSeatHoldFileFailuresAreLoggedNotFatal(t *testing.T) {
	notADirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if errWrite := os.WriteFile(notADirectory, []byte("x"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	corrupt := seatHoldFilePath(t)
	if errWrite := os.WriteFile(corrupt, []byte("{not json"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	for name, path := range map[string]string{
		// Its parent is a file, so both the read and every write fail.
		"unreachable path": filepath.Join(notADirectory, "config.yaml.claude-seat-holds"),
		"corrupt file":     corrupt,
	} {
		t.Run(name, func(t *testing.T) {
			if name == "unreachable path" && runtime.GOOS == "windows" {
				t.Skip("Windows reports a path under a file as not existing")
			}
			f, clock, _ := startSeatHoldTicks(t, path)
			read := f.events("claude routing: seat hold file not read")
			if len(read) != 1 || read[0].Level != log.WarnLevel || !strings.Contains(renderedSeatHoldLine(t, read[0]), "error=") {
				t.Fatalf("want one warning naming the failed read, got %+v", read)
			}
			if _, reason := f.requestMarker(t, "seat-hold-failing", seatHoldBare); reason != "new_client" {
				t.Fatalf("the hub did not serve after a failed read: %s", reason)
			}
			saveTick(f, path)
			clock.advance(time.Minute)
			f.requestMarker(t, "seat-hold-failing", seatHoldBare)
			saveTick(f, path)
			f.m.StopClaudeSeatHolds()
			saved := f.events("claude routing: seat hold file not saved")
			if name == "corrupt file" {
				if len(saved) != 0 || !json.Valid(seatHoldFileBytes(t, path)) {
					t.Fatalf("the first write did not replace the corrupt file: %+v", saved)
				}
				return
			}
			// The failed writes (each tick and the shutdown) warn once.
			var warnings []log.Entry
			for _, entry := range saved {
				if entry.Level <= log.WarnLevel {
					warnings = append(warnings, entry)
				}
			}
			if len(saved) == 0 || len(warnings) != 1 || !strings.Contains(renderedSeatHoldLine(t, warnings[0]), "error=") {
				t.Fatalf("want one warning naming the failed write, got %+v", saved)
			}
		})
	}
}

func TestClaudeSeatHoldFileIsPrivateAndNamesNoSecret(t *testing.T) {
	path := seatHoldFilePath(t)
	f, clock := startSeatHolds(t, path)
	seat, _ := f.requestMarker(t, "seat-hold-private-conversation", seatHoldBare)
	f.m.StopClaudeSeatHolds()
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if mode := info.Mode().Perm(); mode != 0o600 && runtime.GOOS != "windows" {
		t.Fatalf("hold file mode %o, want 600", mode)
	}
	data := seatHoldFileBytes(t, path)
	for _, secret := range []string{"test-only-token", "seat-hold-private-conversation", "shared-access-key-scope", "private prompt"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("hold file carries %q: %s", secret, data)
		}
	}
	// Only these fields: no identity, no pending move, nothing else.
	var shape struct {
		Holds []map[string]any `json:"holds"`
	}
	var top map[string]any
	if errParse := json.Unmarshal(data, &top); errParse != nil {
		t.Fatal(errParse)
	}
	if errParse := json.Unmarshal(data, &shape); errParse != nil {
		t.Fatal(errParse)
	}
	fieldNames := func(object map[string]any) string {
		names := make([]string, 0, len(object))
		for name := range object {
			names = append(names, name)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	if got := fieldNames(top); got != "holds,version" {
		t.Fatalf("hold file fields %s, want holds,version", got)
	}
	if len(shape.Holds) != 1 || fieldNames(shape.Holds[0]) != "auth_id,client,expires_at,keys" {
		t.Fatalf("hold fields %+v, want auth_id,client,expires_at,keys", shape.Holds)
	}
	var file struct {
		Version int `json:"version"`
		Holds   []struct {
			Client    string    `json:"client"`
			Keys      []string  `json:"keys"`
			AuthID    string    `json:"auth_id"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"holds"`
	}
	if errParse := json.Unmarshal(data, &file); errParse != nil {
		t.Fatal(errParse)
	}
	if file.Version != claudeSeatHoldFileVersion || len(file.Holds) != 1 {
		t.Fatalf("want one hold at version %d, got %s", claudeSeatHoldFileVersion, data)
	}
	hold := file.Holds[0]
	if hold.AuthID != seat || !hold.ExpiresAt.Equal(clock.now.Add(5*time.Minute)) || len(hold.Keys) == 0 {
		t.Fatalf("hold %+v, want seat %s until %s", hold, seat, clock.now.Add(5*time.Minute))
	}
	hash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, key := range append([]string{hold.Client}, hold.Keys...) {
		if !hash.MatchString(key) {
			t.Fatalf("hold key %q is not a 64-hex alias key", key)
		}
	}
}

// The file's keys are claudeClientKey's: a release that changes how it derives
// a key must change claudeSeatHoldFileVersion too, so an old file is dropped
// whole. Change both values of the pin together.
func TestClaudeSeatHoldFileVersionPinsClientKey(t *testing.T) {
	pinned := map[int]string{1: "c6abcdcc3b26dd3089f572b5c9e74c8acea88a564e8562f494d75d5aa777f4a1"}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "pin-scope"}}
	want, ok := pinned[claudeSeatHoldFileVersion]
	if got := claudeClientKey("pin-identity", opts); !ok || got != want {
		t.Fatalf("claudeClientKey gives %s at hold file version %d: a new key derivation needs a new file version and a new pin", got, claudeSeatHoldFileVersion)
	}
}

func TestClaudeSeatHoldFileReplacedByRename(t *testing.T) {
	path := seatHoldFilePath(t)
	f, clock, _ := startSeatHoldTicks(t, path)
	f.requestMarker(t, "seat-hold-rename", seatHoldBare)
	saveTick(f, path)
	old := seatHoldFileBytes(t, path)
	// A write in place would show through a second link to the old file.
	link := path + ".before"
	if errLink := os.Link(path, link); errLink != nil {
		t.Fatal(errLink)
	}
	clock.advance(time.Minute)
	f.requestMarker(t, "seat-hold-rename", seatHoldBare)
	saveTick(f, path)
	if string(seatHoldFileBytes(t, link)) != string(old) {
		t.Fatal("the hold file was rewritten in place, want a new file renamed over it")
	}
	if string(seatHoldFileBytes(t, path)) == string(old) {
		t.Fatal("the hold file was not updated")
	}
	if leftovers, _ := filepath.Glob(path + ".*.tmp"); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestClaudeSeatHoldFileSavedEveryIntervalWhenChanged(t *testing.T) {
	if claudeSeatHoldSaveInterval < 30*time.Second || claudeSeatHoldSaveInterval > 60*time.Second {
		t.Fatalf("save interval %s, want 30 to 60 seconds", claudeSeatHoldSaveInterval)
	}
	path := seatHoldFilePath(t)
	f, _, ticks := startSeatHoldTicks(t, path)
	tick(ticks)
	if seatHoldFileExists(t, path) {
		t.Fatal("a tick wrote the file with nothing held")
	}
	f.requestMarker(t, "seat-hold-interval", seatHoldBare)
	tick(ticks)
	if !seatHoldFileExists(t, path) {
		t.Fatal("a tick after a change did not write the file")
	}
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatal(errRemove)
	}
	tick(ticks)
	f.m.StopClaudeSeatHolds()
	if seatHoldFileExists(t, path) {
		t.Fatal("a tick or the shutdown wrote the file again with nothing changed")
	}
}
