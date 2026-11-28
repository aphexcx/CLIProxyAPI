package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
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
// the second tick only after the first one's save returned, and the second
// finds nothing changed.
func tick(ticks chan time.Time) {
	ticks <- time.Time{}
	ticks <- time.Time{}
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
			data := strings.ReplaceAll(string(seatHoldFileBytes(t, path)), strings.ReplaceAll(old, "<seat>", seat), replacement)
			if errWrite := os.WriteFile(path, []byte(data), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		pause   time.Duration
		edit    func(t *testing.T, path, seat string)
		prepare func(t *testing.T, f *seatRoutingFixture, seat string)
		// count is the restore log's field that counts the drop; "" drops the file.
		count string
	}{
		{name: "expired", pause: 5 * time.Minute, count: "expired"},
		{name: "disabled seat", prepare: disable(false), count: "disabled_seat"},
		{name: "seat status disabled", prepare: disable(true), count: "disabled_seat"},
		{name: "gone seat", edit: edit(`"auth_id":"<seat>"`, `"auth_id":"seat-hold-gone-fixture"`), count: "gone_seat"},
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
				if dropped := after.events("claude routing: seat hold file dropped"); len(dropped) != 1 {
					t.Fatalf("want the file dropped once, got %+v", dropped)
				}
			} else {
				restored := after.events("claude routing: seat holds restored")
				if len(restored) != 1 || restored[0].Data[tc.count] != 1 || restored[0].Data["restored"] != 0 {
					t.Fatalf("want one hold dropped as %s and none restored, got %+v", tc.count, restored)
				}
			}
			if _, reason := after.requestMarker(t, "seat-hold-stale", seatHoldBare); reason != "new_client" {
				t.Fatalf("a dropped hold still placed the conversation (%s), want new_client", reason)
			}
		})
	}
}

// A seat that capped while the hub was down answers the restored hold's first
// request with a 429; the existing retry moves the conversation.
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
		// Its parent is a file, so both the read and the write fail.
		"unreachable path": filepath.Join(notADirectory, "config.yaml.claude-seat-holds"),
		"corrupt file":     corrupt,
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := startSeatHolds(t, path)
			if read := f.events("claude routing: seat hold file not read"); len(read) != 1 || read[0].Level != log.WarnLevel {
				t.Fatalf("want one warning for the failed read, got %+v", read)
			}
			if _, reason := f.requestMarker(t, "seat-hold-failing", seatHoldBare); reason != "new_client" {
				t.Fatalf("the hub did not serve after a failed read: %s", reason)
			}
			f.m.StopClaudeSeatHolds()
			saved := f.events("claude routing: seat hold file not saved")
			if name == "unreachable path" && (len(saved) != 1 || saved[0].Level != log.WarnLevel) {
				t.Fatalf("want one warning for the failed write, got %+v", saved)
			}
			if name == "corrupt file" && (len(saved) != 0 || !json.Valid(seatHoldFileBytes(t, path))) {
				t.Fatalf("the shutdown did not replace the corrupt file: %+v", saved)
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
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("hold file mode %o, want 600", mode)
	}
	data := seatHoldFileBytes(t, path)
	for _, secret := range []string{"test-only-token", "seat-hold-private-conversation", "shared-access-key-scope", "private prompt"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("hold file carries %q: %s", secret, data)
		}
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

func TestClaudeSeatHoldFileReplacedByRename(t *testing.T) {
	path := seatHoldFilePath(t)
	f, clock, ticks := startSeatHoldTicks(t, path)
	f.requestMarker(t, "seat-hold-rename", seatHoldBare)
	tick(ticks)
	old := seatHoldFileBytes(t, path)
	// A write in place would show through a second link to the old file.
	link := path + ".before"
	if errLink := os.Link(path, link); errLink != nil {
		t.Fatal(errLink)
	}
	clock.advance(time.Minute)
	f.requestMarker(t, "seat-hold-rename", seatHoldBare)
	tick(ticks)
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
