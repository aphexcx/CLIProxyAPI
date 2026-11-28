package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	log "github.com/sirupsen/logrus"
)

// The Claude seat hold file keeps each conversation's seat hold across a
// restart. Per hold it names the conversation's keys (claudeClientKey's 64-hex
// hashes), the seat's ID, the hold's expiry and, for a late subagent, its mark:
// no session identity in clear and no token. A failed read or write is logged
// and never stops the hub.
const (
	// claudeSeatHoldFileVersion must change whenever claudeClientKey changes how
	// it derives a key (its "claude-seat-v1" prefix): a file of another version
	// is dropped whole.
	claudeSeatHoldFileVersion = 1
	// claudeSeatHoldSaveInterval bounds the holds a crash loses.
	claudeSeatHoldSaveInterval = 45 * time.Second
)

type claudeSeatHoldFile struct {
	Version int                    `json:"version"`
	Holds   []claudeSeatHoldRecord `json:"holds"`
}

type claudeSeatHoldRecord struct {
	// Client is the conversation's own key, the routing log's client_hash.
	Client string `json:"client"`
	// Keys are the alias keys the hold is filed under.
	Keys      []string  `json:"keys"`
	AuthID    string    `json:"auth_id"`
	ExpiresAt time.Time `json:"expires_at"`
	// LateSubagent keeps the late subagent hour's mark (claudeSeatBinding).
	LateSubagent bool `json:"late_subagent,omitempty"`
}

// claudeSeatHoldStore is the hold file's lifecycle on one manager.
type claudeSeatHoldStore struct {
	// mu serializes the file's reads and writes, so an older snapshot never
	// replaces a newer one.
	mu     sync.Mutex
	path   string
	cancel context.CancelFunc
	done   chan struct{}
	// saved is the routing change count the file holds.
	saved uint64
	// failing is set after a failed write, so a path that keeps failing warns
	// once, not at every tick.
	failing bool
}

// StartClaudeSeatHolds reads the holds saved at path once, then writes them
// every claudeSeatHoldSaveInterval while they change, until StopClaudeSeatHolds.
// An empty path keeps the holds in memory only.
func (m *Manager) StartClaudeSeatHolds(ctx context.Context, path string) {
	ticker := time.NewTicker(claudeSeatHoldSaveInterval)
	m.startClaudeSeatHolds(ctx, path, ticker.C, ticker.Stop)
}

func (m *Manager) startClaudeSeatHolds(ctx context.Context, path string, ticks <-chan time.Time, stopTicks func()) {
	if m == nil || path == "" {
		stopTicks()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	store := &m.claudeSeats.holdFile
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.cancel != nil {
		stopTicks()
		return
	}
	if errLoad := m.loadClaudeSeatHolds(path); errLoad != nil {
		log.WithError(errLoad).Warn("claude routing: seat hold file not read")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	store.path, store.cancel, store.done = path, cancel, done
	go func() {
		defer close(done)
		defer stopTicks()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticks:
				m.saveClaudeSeatHoldsLogged(path)
			}
		}
	}()
}

// StopClaudeSeatHolds ends the periodic write and writes the holds once more if
// they changed, as the hub shuts down.
func (m *Manager) StopClaudeSeatHolds() {
	if m == nil {
		return
	}
	store := &m.claudeSeats.holdFile
	store.mu.Lock()
	path, cancel, done := store.path, store.cancel, store.done
	store.path, store.cancel, store.done = "", nil, nil
	store.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	m.saveClaudeSeatHoldsLogged(path)
}

func (m *Manager) saveClaudeSeatHoldsLogged(path string) {
	store := &m.claudeSeats.holdFile
	store.mu.Lock()
	defer store.mu.Unlock()
	errSave := m.saveClaudeSeatHoldsLocked(path)
	if errSave == nil {
		store.failing = false
		return
	}
	entry := log.WithError(errSave)
	if store.failing {
		entry.Debug("claude routing: seat hold file not saved")
		return
	}
	store.failing = true
	entry.Warn("claude routing: seat hold file not saved")
}

// saveClaudeSeatHoldsLocked writes the live holds to path when they changed
// since the file was last written. The caller holds the store's lock.
func (m *Manager) saveClaudeSeatHoldsLocked(path string) error {
	store := &m.claudeSeats.holdFile
	routing := &m.claudeSeats
	routing.mu.Lock()
	changes := routing.changes
	if changes == store.saved {
		routing.mu.Unlock()
		return nil
	}
	holds := routing.holdRecordsLocked(routing.clock())
	routing.mu.Unlock()
	for _, hold := range holds {
		sort.Strings(hold.Keys)
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].Client < holds[j].Client })
	data, errMarshal := json.Marshal(claudeSeatHoldFile{Version: claudeSeatHoldFileVersion, Holds: holds})
	if errMarshal != nil {
		return fmt.Errorf("marshal claude seat holds: %w", errMarshal)
	}
	if errWrite := writeClaudeSeatHoldFile(path, data); errWrite != nil {
		return errWrite
	}
	store.saved = changes
	return nil
}

// holdRecordsLocked lists the live holds, each with every alias key filed
// under it, unsorted so the routing lock is held only to copy.
func (r *claudeSeatRouting) holdRecordsLocked(now time.Time) []claudeSeatHoldRecord {
	index := make(map[*claudeSeatBinding]int)
	holds := make([]claudeSeatHoldRecord, 0)
	for key, binding := range r.bindings {
		if binding == nil || binding.authID == "" || !now.Before(binding.expiresAt) {
			continue
		}
		i, seen := index[binding]
		if !seen {
			i = len(holds)
			index[binding] = i
			holds = append(holds, claudeSeatHoldRecord{Client: binding.clientKey,
				AuthID: binding.authID, ExpiresAt: binding.expiresAt.UTC(), LateSubagent: binding.lateSubagent})
		}
		holds[i].Keys = append(holds[i].Keys, key)
	}
	return holds
}

// writeClaudeSeatHoldFile replaces path through a temporary file in its
// directory, so a reader finds the old file or the new one, never a part.
// os.CreateTemp makes the file mode 0600, which the rename keeps.
func writeClaudeSeatHoldFile(path string, data []byte) error {
	tmpFile, errCreate := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create claude seat hold temp file: %w", errCreate)
	}
	tmp := tmpFile.Name()
	_, errWrite := tmpFile.Write(data)
	if errClose := tmpFile.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write claude seat hold temp file: %w", errWrite)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace claude seat hold file: %w", errRename)
	}
	return nil
}

// loadClaudeSeatHolds restores the holds saved at path. It drops a file of
// another version, and expired holds, holds whose seat is gone or disabled, and
// holds whose keys are not client keys. The caller holds the store's lock.
func (m *Manager) loadClaudeSeatHolds(path string) error {
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("read claude seat hold file: %w", errRead)
	}
	var file claudeSeatHoldFile
	if errParse := json.Unmarshal(data, &file); errParse != nil {
		return fmt.Errorf("parse claude seat hold file: %w", errParse)
	}
	if file.Version != claudeSeatHoldFileVersion {
		log.WithFields(log.Fields{"provider": "claude", "version": file.Version,
			"want_version": claudeSeatHoldFileVersion}).Info("claude routing: seat hold file dropped")
		return nil
	}
	routing := &m.claudeSeats
	now := routing.clock()
	var restored []*claudeSeatBinding
	var restoredKeys [][]string
	expired, gone, disabled, invalid := 0, 0, 0, 0
	for _, record := range file.Holds {
		if !isClaudeClientKey(record.Client) || len(record.Keys) == 0 {
			invalid++
			continue
		}
		valid := true
		for _, key := range record.Keys {
			valid = valid && isClaudeClientKey(key)
		}
		if !valid {
			invalid++
			continue
		}
		if !now.Before(record.ExpiresAt) {
			expired++
			continue
		}
		auth, ok := m.GetByID(record.AuthID)
		if !ok {
			gone++
			continue
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			disabled++
			continue
		}
		restored = append(restored, &claudeSeatBinding{authID: auth.ID, authIndex: auth.EnsureIndex(),
			clientKey: record.Client, identities: make(map[string]bool), expiresAt: record.ExpiresAt,
			lateSubagent: record.LateSubagent})
		restoredKeys = append(restoredKeys, record.Keys)
	}
	routing.mu.Lock()
	if routing.bindings == nil {
		routing.bindings = make(map[string]*claudeSeatBinding)
		routing.placements = make(map[string]*claudeSeatPlacement)
		routing.counts = make(map[claudeSeatCountKey]uint64)
	}
	for i, binding := range restored {
		// The held seat is also the conversation's last seat, so a restored
		// hold that moves is a move, as it would be without the restart.
		placement := &claudeSeatPlacement{authID: binding.authID, authIndex: binding.authIndex}
		for _, key := range restoredKeys[i] {
			// A hold placed in this process is newer than the file's.
			if _, live := routing.bindings[key]; !live {
				routing.bindings[key] = binding
			}
			if _, live := routing.placements[key]; !live {
				routing.placements[key] = placement
			}
		}
	}
	routing.mu.Unlock()
	log.WithFields(log.Fields{"provider": "claude", "holds_restored": len(restored), "holds_expired": expired,
		"holds_gone_seat": gone, "holds_disabled_seat": disabled, "holds_invalid": invalid}).Info("claude routing: seat holds restored")
	return nil
}

// isClaudeClientKey reports whether key has claudeClientKey's form.
func isClaudeClientKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	_, errDecode := hex.DecodeString(key)
	return errDecode == nil
}
