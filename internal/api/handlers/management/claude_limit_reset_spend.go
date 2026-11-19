package management

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/resetclaim"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// claudeResetSpendLedgerName is the append-only record of every spend, in
	// the hub's log directory. Log cleanup removes only main.log rotations.
	claudeResetSpendLedgerName = "claude-limit-reset-spends.jsonl"
	// claudeResetLedgerSentinelName marks that the ledger was created. A
	// ledger missing beside it is refused, never read as "no spend ever".
	claudeResetLedgerSentinelName = "claude-limit-reset-spends.created"
	// claudeResetRetryWindow bounds the explicit retry of a claim whose outcome
	// is unknown; the hub never retries on its own.
	claudeResetRetryWindow = 10 * time.Minute
	// claudeResetIdempotencyHeader names the caller's key for one spend: the
	// same key delivered again returns the recorded outcome and sends nothing.
	claudeResetIdempotencyHeader = "Idempotency-Key"

	// A spend writes up to three ledger lines: "sent", synced to disk before
	// the claim leaves (so a restart keeps the seat held); "answered", the
	// outcome, written as soon as it is known; and "balance", what is left of
	// each kind after the claim, written once the after-read returns.
	claudeResetPhaseSent     = "sent"
	claudeResetPhaseAnswered = "answered"
	claudeResetPhaseBalance  = "balance"
	// claudeResetPhaseTornClosed follows a torn line (a write cut short, which
	// no claim followed) that a later append closed; only such a line is skipped.
	claudeResetPhaseTornClosed = "torn_line_closed"
	// claudeResetResultNotSent records a claim the hub certainly did not send.
	claudeResetResultNotSent = "not_sent"
)

// Reason codes of a claim the hub did not send. Each is a code, never text.
const (
	claudeResetAbortTransport      = "transport_refused"
	claudeResetAbortCredential     = "credential_changed_at_dispatch"
	claudeResetAbortGrantExpired   = "grant_expired_at_dispatch"
	claudeResetAbortRetryWindow    = "retry_window_passed_at_dispatch"
	claudeResetLedgerErrorText     = "the outcome is not in the spend ledger: the seat stays held and no retry of this claim is accepted"
	claudeResetBalanceErrorText    = "what is left after the claim is not in the spend ledger"
	claudeResetCredentialRefusal   = "only an enabled OAuth Claude seat can spend a banked reset"
	claudeResetIdempotencyRequired = "an Idempotency-Key header is required: a fresh key for each new spend, the same key to deliver one again"
)

var (
	claudeResetOperationPattern      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	claudeResetAccountPattern        = regexp.MustCompile(`^[0-9a-f]{24}$`)
	claudeResetIdempotencyKeyPattern = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// claudeResetSpendRequest is the spend's body. A selector the caller sends
// must name something: grant_id and retry_request_id are never empty here.
type claudeResetSpendRequest struct {
	authIndex, kind, grantID, retryRequestID string
}

// claudeResetSpendRecord is one ledger line. It holds no token, email or
// organization: Account is an opaque digest that binds a hold to its account
// across restarts, and Operation is a digest of the caller's Idempotency-Key.
type claudeResetSpendRecord struct {
	Phase            string         `json:"phase"`
	Operation        string         `json:"operation"`
	Account          string         `json:"account,omitempty"`
	At               time.Time      `json:"at"`
	AuthIndex        string         `json:"auth_index"`
	Kind             string         `json:"kind,omitempty"`
	GrantID          string         `json:"grant_id,omitempty"`
	NamedGrant       string         `json:"named_grant"`
	RequestID        string         `json:"request_id"`
	Retry            bool           `json:"retry"`
	Result           string         `json:"result,omitempty"`
	Reason           string         `json:"reason,omitempty"`
	HTTPStatus       int            `json:"http_status,omitempty"`
	ResetsLeftBefore map[string]int `json:"resets_left_before"`
	ResetsLeftAfter  map[string]int `json:"resets_left_after"`
}

type claudeResetUnsettledClaim struct {
	kind, grantID, requestID, account string
	sentAt                            time.Time
	// unrecorded is a known outcome the ledger could not record: the hold
	// stays, and no retry may follow.
	unrecorded string
}

// claudeResetAccount is the opaque account binding the ledger keeps in place
// of the organization id.
func claudeResetAccount(organization string) string {
	sum := sha256.Sum256([]byte("cp-g33 claude reset account\x00" + strings.ToLower(strings.TrimSpace(organization))))
	return hex.EncodeToString(sum[:12])
}

// claudeResetOperation is the ledger's digest of the caller's Idempotency-Key.
func claudeResetOperation(key string) string {
	sum := sha256.Sum256([]byte("cp-g33 claude reset operation\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// claudeResetConfig is the spend's own copy of the configuration, taken
// under the handler's lock: management writes edit h.cfg in place under it.
func (h *Handler) claudeResetConfig() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.CloneForRuntime()
}

// claudeResetLedgerDir is the ledger's directory, read under the same lock.
func (h *Handler) claudeResetLedgerDir() string {
	if h.logDir != "" {
		return h.logDir
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return logging.ResolveLogDirectory(h.cfg)
}

// claudeResetSpendLedger is the handler's door to the spend journals: the
// clock spends are judged on. Its zero value is ready to use.
type claudeResetSpendLedger struct {
	now func() time.Time
}

func (l *claudeResetSpendLedger) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// claudeResetJournals holds the state of every spend ledger in this process,
// keyed by the journal's identity (its resolved directory), so every handler
// on one journal shares its holds and reservations.
var claudeResetJournals = struct {
	mu     sync.Mutex
	byPath map[string]*claudeResetJournal
}{byPath: make(map[string]*claudeResetJournal)}

// journal returns the state of the ledger in dir. A request takes its journal
// once, so a log directory changed mid-request cannot split one spend.
func (l *claudeResetSpendLedger) journal(dir string) (*claudeResetJournal, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("no log directory for the reset spend ledger")
	}
	resolved, errResolve := claudeResetJournalDir(dir)
	if errResolve != nil {
		return nil, errResolve
	}
	claudeResetJournals.mu.Lock()
	defer claudeResetJournals.mu.Unlock()
	journal := claudeResetJournals.byPath[resolved]
	if journal == nil {
		journal = &claudeResetJournal{dir: resolved}
		claudeResetJournals.byPath[resolved] = journal
	}
	return journal, nil
}

// claudeResetJournalDir resolves dir to one identity: the absolute path with
// the symlinks of its nearest existing ancestor resolved, so the identity
// does not change when the directory is created.
func claudeResetJournalDir(dir string) (string, error) {
	abs, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		return "", errAbs
	}
	existing, rest := abs, ""
	for {
		if real, errEval := filepath.EvalSymlinks(existing); errEval == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

// claudeResetJournal is one ledger's state: the seats and operations in
// flight, the reserved accounts, the claims whose outcome is unknown, and the
// last spend of each seat. Holds are per hub process and per journal.
type claudeResetJournal struct {
	mu     sync.Mutex
	dir    string
	loaded bool
	// durable is set once an append has synced the ledger's directory entries.
	durable         bool
	busy            map[string]bool
	keysInFlight    map[string]bool
	accountInFlight map[string]string
	unsettled       map[string]*claudeResetUnsettledClaim
	awaiting        map[string]claudeResetSpendRecord
	last            map[string]claudeResetSpendRecord
	operations      map[string]claudeResetSpendRecord
	// claims is the newest record of each claim, by request id.
	claims map[string]claudeResetSpendRecord
}

func claudeResetSettles(record claudeResetSpendRecord) bool {
	if record.Retry {
		// A refusal on a retry cannot prove the first send did not spend.
		return record.Result == claudeauth.LimitResetResultReset || record.Result == claudeauth.LimitResetResultAlreadyUsed
	}
	return record.Result != claudeauth.LimitResetResultUnknown
}

func claudeResetKnownResult(result string) bool {
	switch result {
	case claudeauth.LimitResetResultReset, claudeauth.LimitResetResultAlreadyUsed, claudeauth.LimitResetResultNotLimited,
		claudeauth.LimitResetResultCooldown, claudeauth.LimitResetResultIneligible, claudeauth.LimitResetResultUnavailable,
		claudeauth.LimitResetResultRateLimited, claudeauth.LimitResetResultAuthError, claudeauth.LimitResetResultUnknown,
		claudeResetResultNotSent:
		return true
	}
	return false
}

var errClaudeResetRecordInvalid = errors.New("the reset spend ledger holds an incomplete or invalid record")

// claudeResetRecordValid checks one record's own fields: every field its
// phase requires, a known result, and a reason that is a code.
func claudeResetRecordValid(record claudeResetSpendRecord) bool {
	if record.At.IsZero() || record.AuthIndex == "" || !claudeResetOperationPattern.MatchString(record.Operation) || !resetclaim.ValidRequestID(record.RequestID) {
		return false
	}
	if record.Phase == claudeResetPhaseBalance {
		return record.ResetsLeftAfter != nil
	}
	if record.Phase != claudeResetPhaseSent && record.Phase != claudeResetPhaseAnswered {
		return false
	}
	if !claudeResetAccountPattern.MatchString(record.Account) || !coreauth.ValidClaudeResetKind(record.Kind) || !resetclaim.ValidGrantID(record.GrantID) ||
		(record.NamedGrant != "" && record.NamedGrant != record.GrantID) || record.ResetsLeftBefore == nil ||
		(record.Reason != "" && !claudeauth.ValidLimitResetReasonCode(record.Reason)) {
		return false
	}
	if record.Phase == claudeResetPhaseSent {
		return record.Result == claudeauth.LimitResetResultUnknown
	}
	return claudeResetKnownResult(record.Result)
}

// checkLocked validates one record against the journal: its own fields and
// its transition. A sent first claim needs a seat with no claim held, a sent
// retry needs the held claim it names, an answer needs its sent line, and a
// balance needs its answer.
func (j *claudeResetJournal) checkLocked(record claudeResetSpendRecord) error {
	if !claudeResetRecordValid(record) {
		return errClaudeResetRecordInvalid
	}
	seat := record.AuthIndex
	switch record.Phase {
	case claudeResetPhaseSent:
		if _, used := j.operations[record.Operation]; used {
			return errors.New("the reset spend ledger records one operation twice")
		}
		open := j.openLocked(seat, record.At)
		if !record.Retry {
			if open != nil {
				return errors.New("the reset spend ledger records a claim sent while its seat was held")
			}
			return nil
		}
		if open == nil || open.requestID != record.RequestID || open.kind != record.Kind || open.grantID != record.GrantID || open.account != record.Account {
			return errors.New("the reset spend ledger records a retry of no held claim")
		}
	case claudeResetPhaseAnswered:
		waiting, ok := j.awaiting[seat]
		if !ok || waiting.Operation != record.Operation || waiting.RequestID != record.RequestID || waiting.Retry != record.Retry ||
			waiting.Kind != record.Kind || waiting.GrantID != record.GrantID || waiting.Account != record.Account {
			return errors.New("the reset spend ledger records an answer to no sent claim")
		}
	case claudeResetPhaseBalance:
		answered, ok := j.operations[record.Operation]
		if !ok || answered.Phase != claudeResetPhaseAnswered || answered.RequestID != record.RequestID || answered.AuthIndex != seat {
			return errors.New("the reset spend ledger records a balance of no answered claim")
		}
	}
	return nil
}

// foldLocked applies a checked record to the journal's state.
func (j *claudeResetJournal) foldLocked(record claudeResetSpendRecord) {
	seat := record.AuthIndex
	switch record.Phase {
	case claudeResetPhaseSent:
		j.awaiting[seat] = record
		if !record.Retry {
			j.unsettled[seat] = &claudeResetUnsettledClaim{kind: record.Kind, grantID: record.GrantID, requestID: record.RequestID, account: record.Account, sentAt: record.At}
		}
	case claudeResetPhaseAnswered:
		delete(j.awaiting, seat)
		if open := j.unsettled[seat]; open != nil && open.requestID == record.RequestID && claudeResetSettles(record) {
			delete(j.unsettled, seat)
		}
	case claudeResetPhaseBalance:
		answered := j.operations[record.Operation]
		answered.ResetsLeftAfter = record.ResetsLeftAfter
		j.operations[record.Operation] = answered
		if last, ok := j.last[seat]; ok && last.Operation == record.Operation {
			j.last[seat] = answered
		}
		return
	}
	j.last[seat] = record
	j.operations[record.Operation] = record
	j.claims[record.RequestID] = record
}

// latest is the newest record of the claim with requestID.
func (j *claudeResetJournal) latest(requestID string) claudeResetSpendRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.claims[requestID]
}

// loadLocked restores the journal from the ledger file. It fails closed: a
// ledger that cannot be opened or read, a ledger missing after one was
// created, or a line that is not a valid record in a valid order is an error
// and nothing is kept, so every spend is refused until the ledger is sound.
// Only a torn line is skipped: the last line cut short, or one a later append
// closed with a torn_line_closed marker. No claim follows a torn line,
// because a claim leaves only after its sent line is synced.
func (j *claudeResetJournal) loadLocked() error {
	if j.loaded {
		return nil
	}
	j.busy = make(map[string]bool)
	j.keysInFlight = make(map[string]bool)
	j.accountInFlight = make(map[string]string)
	j.unsettled = make(map[string]*claudeResetUnsettledClaim)
	j.awaiting = make(map[string]claudeResetSpendRecord)
	j.last = make(map[string]claudeResetSpendRecord)
	j.operations = make(map[string]claudeResetSpendRecord)
	j.claims = make(map[string]claudeResetSpendRecord)
	file, errOpen := os.Open(filepath.Join(j.dir, claudeResetSpendLedgerName))
	if errOpen != nil {
		if !os.IsNotExist(errOpen) {
			return errOpen
		}
		if _, errSentinel := os.Lstat(filepath.Join(j.dir, claudeResetLedgerSentinelName)); errSentinel == nil {
			return errors.New("the reset spend ledger is missing although one was created")
		} else if !os.IsNotExist(errSentinel) {
			return errSentinel
		}
		j.loaded = true
		return nil
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("failed to close Claude reset spend ledger: %v", errClose)
		}
	}()
	reader := bufio.NewReader(file)
	invalidPending := false
	for {
		line, errRead := reader.ReadBytes('\n')
		if errRead != nil && !errors.Is(errRead, io.EOF) {
			return errRead
		}
		complete := errRead == nil
		var record claudeResetSpendRecord
		parsed := json.Unmarshal(line, &record) == nil
		switch {
		case parsed && record.Phase == claudeResetPhaseTornClosed:
			// The line before it was cut short: skipped if invalid, kept if
			// the cut fell just after a complete record.
			if invalidPending {
				log.Warn("Claude reset spend ledger holds a closed torn line; it is skipped")
			}
			invalidPending = false
		case invalidPending:
			return errClaudeResetRecordInvalid
		case parsed && (complete || claudeResetRecordValid(record)):
			if errCheck := j.checkLocked(record); errCheck != nil {
				return errCheck
			}
			j.foldLocked(record)
		case complete:
			invalidPending = true
		case len(line) > 0:
			log.Warn("Claude reset spend ledger ends in a torn line; it is skipped")
		}
		if !complete {
			if invalidPending {
				return errClaudeResetRecordInvalid
			}
			j.loaded = true
			return nil
		}
	}
}

// errClaudeResetHistoryUnavailable is what a read shows in place of a seat's
// last spend while the ledger cannot be read: unknown, not "never spent".
var errClaudeResetHistoryUnavailable = errors.New("the reset spend ledger cannot be read")

// claudeResetHistory is the last spend of every seat, read once per request.
type claudeResetHistory struct {
	last map[string]claudeResetSpendRecord
	err  error
}

// seat is the seat's last spend; nil with no error means no reset was ever
// spent through this hub.
func (history *claudeResetHistory) seat(authIndex string) (*claudeResetSpendRecord, error) {
	if history.err != nil {
		return nil, history.err
	}
	record, ok := history.last[authIndex]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

// claudeResetHistory takes one snapshot of the ledger for a listing request.
// A ledger that cannot be read is read again by the next request.
func (h *Handler) claudeResetHistory() *claudeResetHistory {
	journal, errJournal := h.claudeResetSpends.journal(h.claudeResetLedgerDir())
	if errJournal != nil {
		return &claudeResetHistory{err: errClaudeResetHistoryUnavailable}
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if errLoad := journal.loadLocked(); errLoad != nil {
		return &claudeResetHistory{err: errClaudeResetHistoryUnavailable}
	}
	last := make(map[string]claudeResetSpendRecord, len(journal.last))
	for seat, record := range journal.last {
		last[seat] = record
	}
	return &claudeResetHistory{last: last}
}

// openLocked returns the seat's unknown claim inside its retry window at now.
func (j *claudeResetJournal) openLocked(authIndex string, now time.Time) *claudeResetUnsettledClaim {
	open := j.unsettled[authIndex]
	if open != nil && now.Sub(open.sentAt) >= claudeResetRetryWindow {
		delete(j.unsettled, authIndex)
		return nil
	}
	return open
}

// claudeResetSameOperation reports whether a request delivered under a used
// key asks for the operation the key recorded.
func claudeResetSameOperation(recorded claudeResetSpendRecord, request claudeResetSpendRequest, kind string) bool {
	retryRequestID := ""
	if recorded.Retry {
		retryRequestID = recorded.RequestID
	}
	return recorded.AuthIndex == request.authIndex && recorded.Kind == kind && recorded.NamedGrant == request.grantID && retryRequestID == request.retryRequestID
}

// begin reserves the seat and the operation. A key already recorded returns
// its record and reserves nothing. A claim with an unknown outcome holds the
// seat for its retry window: only a retry naming its request id may proceed,
// and that retry is returned.
func (j *claudeResetJournal) begin(operation string, request claudeResetSpendRequest, kind string, now time.Time) (*claudeResetSpendRecord, *claudeResetUnsettledClaim, int, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if errLoad := j.loadLocked(); errLoad != nil {
		log.WithError(errLoad).Error("Claude reset spend ledger unreadable; spends refused")
		return nil, nil, http.StatusServiceUnavailable, "the spend ledger cannot be read, so earlier claims cannot be ruled out"
	}
	if j.keysInFlight[operation] {
		return nil, nil, http.StatusConflict, "a spend with this Idempotency-Key is in flight"
	}
	if recorded, ok := j.operations[operation]; ok {
		if !claudeResetSameOperation(recorded, request, kind) {
			return nil, nil, http.StatusConflict, "this Idempotency-Key was used for another spend"
		}
		return &recorded, nil, 0, ""
	}
	seat := request.authIndex
	if j.busy[seat] {
		return nil, nil, http.StatusConflict, "a spend is already in flight for this seat"
	}
	open := j.openLocked(seat, now)
	switch {
	case request.retryRequestID == "" && open != nil:
		return nil, nil, http.StatusConflict, "an earlier claim on this seat has an unknown outcome: retry it with its retry_request_id within ten minutes of sending, or check the account's usage page"
	case request.retryRequestID != "" && (open == nil || open.requestID != request.retryRequestID):
		return nil, nil, http.StatusConflict, "no claim with an unknown outcome has this request id inside its ten-minute retry window"
	case open != nil && open.unrecorded != "":
		return nil, nil, http.StatusConflict, "the provider answered this claim (" + open.unrecorded + ") but the spend ledger could not record it"
	}
	j.busy[seat], j.keysInFlight[operation] = true, true
	if open == nil {
		return nil, nil, 0, ""
	}
	retry := *open
	return nil, &retry, 0, ""
}

// reserveAccount holds the account across seats, for a first claim and for a
// retry alike: two credential files of one account must not claim while
// either has a claim in flight or unknown. It is taken before the grants read
// that authorizes the claim, and lasts until the request finishes.
func (j *claudeResetJournal) reserveAccount(authIndex, account string, now time.Time) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if holder, ok := j.accountInFlight[account]; ok && holder != authIndex {
		return "a spend is in flight on another seat of this account"
	}
	for seat := range j.unsettled {
		if open := j.openLocked(seat, now); seat != authIndex && open != nil && open.account == account {
			return "another seat of this account has a claim with an unknown outcome"
		}
	}
	j.accountInFlight[account] = authIndex
	return ""
}

func (j *claudeResetJournal) finish(authIndex, operation string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.busy, authIndex)
	delete(j.keysInFlight, operation)
	for account, holder := range j.accountInFlight {
		if holder == authIndex {
			delete(j.accountInFlight, account)
		}
	}
}

// markSent makes the sent line durable, then holds the seat. When the line
// cannot be written the claim must not be sent.
func (j *claudeResetJournal) markSent(record claudeResetSpendRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if errCheck := j.checkLocked(record); errCheck != nil {
		return errCheck
	}
	if errAppend := j.appendLocked(record); errAppend != nil {
		return errAppend
	}
	j.foldLocked(record)
	return nil
}

// answer records a known outcome at once. When the line cannot be written
// the hold stays and no retry of the claim is accepted; a delivery of the same
// key again still answers with the known outcome.
func (j *claudeResetJournal) answer(record claudeResetSpendRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	log.WithFields(log.Fields{
		"auth_index": record.AuthIndex, "kind": record.Kind, "grant_id": record.GrantID, "request_id": record.RequestID,
		"retry": record.Retry, "result": record.Result, "reason": record.Reason, "http_status": record.HTTPStatus,
		"resets_left_before": record.ResetsLeftBefore,
	}).Info("Claude banked reset spend")
	errWrite := j.checkLocked(record)
	if errWrite == nil {
		errWrite = j.appendLocked(record)
	}
	if errWrite != nil {
		log.WithError(errWrite).Error("Claude reset spend outcome not written; the seat stays held")
		if open := j.unsettled[record.AuthIndex]; open != nil && open.requestID == record.RequestID {
			open.unrecorded = record.Result
		}
		j.operations[record.Operation] = record
		return errWrite
	}
	j.foldLocked(record)
	return nil
}

// recordBalance writes what is left of each kind after an answered claim.
func (j *claudeResetJournal) recordBalance(record claudeResetSpendRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if errCheck := j.checkLocked(record); errCheck != nil {
		return errCheck
	}
	log.WithFields(log.Fields{
		"auth_index": record.AuthIndex, "request_id": record.RequestID, "resets_left_after": record.ResetsLeftAfter,
	}).Info("Claude banked reset spend balance")
	if errAppend := j.appendLocked(record); errAppend != nil {
		log.WithError(errAppend).Error("Claude reset spend balance not written")
		return errAppend
	}
	j.foldLocked(record)
	return nil
}

// syncClaudeResetDirectories makes every directory entry from dir up to the
// root durable, as the refresh journal does before an exchange.
func syncClaudeResetDirectories(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	for {
		handle, errOpen := os.Open(dir)
		if errOpen != nil {
			return errOpen
		}
		errSync := handle.Sync()
		if errClose := handle.Close(); errSync == nil {
			errSync = errClose
		}
		if errSync != nil {
			return errSync
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// claudeResetSyncDirectories is the directory sync appends use; a test
// replaces it to make the sync fail.
var claudeResetSyncDirectories = syncClaudeResetDirectories

// ensureClaudeResetSentinel creates the sentinel that records that the
// ledger in dir exists, its content synced. Its directory entry is made
// durable by the append's directory sync.
func ensureClaudeResetSentinel(dir string) error {
	path := filepath.Join(dir, claudeResetLedgerSentinelName)
	if _, errStat := os.Lstat(path); errStat == nil || !os.IsNotExist(errStat) {
		return errStat
	}
	sentinel, errCreate := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errCreate != nil {
		return errCreate
	}
	errSync := sentinel.Sync()
	if errClose := sentinel.Close(); errSync == nil {
		errSync = errClose
	}
	return errSync
}

// appendLocked appends one record and syncs it. A ledger file it creates is
// made durable before the sentinel beside it exists. Until one append has
// synced the ledger's directory entries (the ledger, the sentinel and every
// directory above them), each append syncs them after its write, so no
// record is reported written before they are durable. A failed append is cut
// back off the file, so memory and disk agree on what was written.
func (j *claudeResetJournal) appendLocked(record claudeResetSpendRecord) error {
	line, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(j.dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	path := filepath.Join(j.dir, claudeResetSpendLedgerName)
	_, errStat := os.Lstat(path)
	created := os.IsNotExist(errStat)
	file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if errOpen != nil {
		return errOpen
	}
	var errWrite error
	if created {
		j.durable = false
		errWrite = claudeResetSyncDirectories(j.dir)
	}
	if errWrite == nil {
		errWrite = ensureClaudeResetSentinel(j.dir)
	}
	var info os.FileInfo
	if errWrite == nil {
		info, errWrite = file.Stat()
	}
	// A torn last line (a write cut short) is closed first, so this record
	// starts on its own line and is never read as part of the torn one.
	if errWrite == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, errWrite = file.ReadAt(last, info.Size()-1); errWrite == nil && last[0] != '\n' {
			marker, _ := json.Marshal(claudeResetSpendRecord{Phase: claudeResetPhaseTornClosed, At: record.At})
			line = append(append(append([]byte{'\n'}, marker...), '\n'), line...)
		}
	}
	wrote := false
	if errWrite == nil {
		wrote = true
		_, errWrite = file.Write(append(line, '\n'))
	}
	if errWrite == nil {
		errWrite = file.Sync()
	}
	synced := false
	if errWrite == nil && !j.durable {
		errWrite = claudeResetSyncDirectories(j.dir)
		synced = errWrite == nil
	}
	if errWrite != nil && wrote {
		if errTruncate := file.Truncate(info.Size()); errTruncate == nil {
			_ = file.Sync()
		} else {
			log.WithError(errTruncate).Error("Claude reset spend ledger: a failed record could not be cut back")
		}
	}
	if errClose := file.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil && synced {
		j.durable = true
	}
	return errWrite
}

func claudeResetsLeftByKind(kinds map[string]coreauth.ClaudeResetKindSummary) map[string]int {
	left := make(map[string]int, len(kinds))
	for kind, summary := range kinds {
		left[kind] = summary.ResetsLeft
	}
	return left
}

func (h *Handler) claudeResetAuth(cfg *config.Config, auth *coreauth.Auth) *claudeauth.ClaudeAuth {
	if h.claudeResetClient != nil {
		return h.claudeResetClient(cfg, auth.ProxyURL)
	}
	return claudeauth.NewClaudeAuthWithProxyURL(cfg, auth.ProxyURL)
}

func readClaudeResetStatus(ctx context.Context, client *claudeauth.ClaudeAuth, token, userAgent string) (*coreauth.ClaudeLimitResets, json.RawMessage, int, error) {
	body, errRead := client.FetchLimitResetSpendStatus(ctx, token, userAgent)
	if errRead != nil {
		status := 0
		var statusError interface{ StatusCode() int }
		if errors.As(errRead, &statusError) {
			status = statusError.StatusCode()
		}
		return nil, nil, status, errRead
	}
	parsed, errParse := coreauth.ParseClaudeLimitResets(body)
	return parsed, body, 0, errParse
}

// sendClaudeResetClaim is the handler's only step that spends. It is reached
// only from SpendClaudeLimitReset (TestBankedResetClaimHasOneCallerChain).
func sendClaudeResetClaim(ctx context.Context, client *claudeauth.ClaudeAuth, token, userAgent string, claim resetclaim.Claim) (claudeauth.LimitResetClaimResult, error) {
	return client.ClaimLimitReset(ctx, token, userAgent, claim)
}

// parseClaudeResetSpendRequest reads one JSON object with no duplicate and no
// unknown key. A selector that is sent must name something: an empty or null
// grant_id or retry_request_id is refused, never read as omitted.
func parseClaudeResetSpendRequest(body io.Reader) (claudeResetSpendRequest, string) {
	const invalid = "invalid body: send one JSON object with auth_index and kind"
	const limit = 1 << 16
	var request claudeResetSpendRequest
	encoded, errRead := io.ReadAll(io.LimitReader(body, limit+1))
	if errRead != nil {
		return request, invalid
	}
	if len(encoded) > limit {
		return request, "invalid body: larger than 64 KiB"
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if open, errOpen := decoder.Token(); errOpen != nil || open != json.Delim('{') {
		return request, invalid
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, errKey := decoder.Token()
		key, isKey := token.(string)
		if errKey != nil || !isKey {
			return request, invalid
		}
		if seen[key] {
			return request, "invalid body: the key " + key + " is sent twice"
		}
		seen[key] = true
		var raw json.RawMessage
		if errValue := decoder.Decode(&raw); errValue != nil {
			return request, invalid
		}
		var target *string
		switch key {
		case "auth_index":
			target = &request.authIndex
		case "kind":
			target = &request.kind
		case "grant_id":
			target = &request.grantID
		case "retry_request_id":
			target = &request.retryRequestID
		default:
			return request, "invalid body: unknown key " + key
		}
		if string(raw) == "null" {
			if key == "grant_id" || key == "retry_request_id" {
				return request, "invalid body: " + key + " is empty; omit it or name one"
			}
			continue
		}
		if json.Unmarshal(raw, target) != nil {
			return request, "invalid body: " + key + " must be a string"
		}
		*target = strings.TrimSpace(*target)
		if *target == "" && (key == "grant_id" || key == "retry_request_id") {
			return request, "invalid body: " + key + " is empty; omit it or name one"
		}
	}
	if closing, errClose := decoder.Token(); errClose != nil || closing != json.Delim('}') {
		return request, invalid
	}
	if _, errTrailing := decoder.Token(); !errors.Is(errTrailing, io.EOF) {
		return request, "invalid body: one JSON object only"
	}
	return request, ""
}

// claudeResetAuthorizeGrant is the one rule a first claim and a retry alike
// must pass on the fresh read: the read lists the kind, the seat is eligible
// and out of its reset cooldown, and the grant named (or, for a first claim
// naming none, the one selected) is in the read, of the kind and spendable.
func claudeResetAuthorizeGrant(status *coreauth.ClaudeLimitResets, kinds map[string]coreauth.ClaudeResetKindSummary, kind, grantID string, retry bool, now time.Time) (coreauth.ClaudeResetGrant, int, string) {
	if _, listed := kinds[kind]; !listed {
		return coreauth.ClaudeResetGrant{}, http.StatusBadRequest, "the seat's grants list no reset of this kind"
	}
	if !status.Eligible {
		return coreauth.ClaudeResetGrant{}, http.StatusConflict, "the seat is not eligible for a reset"
	}
	if status.CooldownUntil != nil && status.CooldownUntil.After(now) {
		return coreauth.ClaudeResetGrant{}, http.StatusConflict, "the seat's reset cooldown is active"
	}
	if grantID == "" {
		grant, ok := coreauth.SelectClaudeResetGrant(status.Grants, kind, now)
		if !ok {
			return coreauth.ClaudeResetGrant{}, http.StatusConflict, "no grant of this kind can be spent now"
		}
		return grant, 0, ""
	}
	for _, grant := range status.Grants {
		if grant.ID != grantID {
			continue
		}
		switch {
		case grant.Kind != kind && retry:
			return coreauth.ClaudeResetGrant{}, http.StatusConflict, "the retried grant changed kind"
		case grant.Kind != kind:
			return coreauth.ClaudeResetGrant{}, http.StatusBadRequest, "the named grant is of kind " + grant.Kind + ", not " + kind
		case !coreauth.ClaudeResetGrantSpendable(grant, now):
			return coreauth.ClaudeResetGrant{}, http.StatusConflict, "the named grant cannot be spent now"
		}
		return grant, 0, ""
	}
	return coreauth.ClaudeResetGrant{}, http.StatusConflict, "the named grant is not in the seat's fresh grants read"
}

// claudeResetDispatchAbort judges the claim again after its sent line is
// durable, on a fresh clock: the registry must still hold the same enabled
// OAuth credential, the grant must not have expired, and a retry must still
// be inside its window. It returns the reason code of a claim not to send.
func (h *Handler) claudeResetDispatchAbort(auth *coreauth.Auth, grant coreauth.ClaudeResetGrant, retry *claudeResetUnsettledClaim, at time.Time) string {
	switch {
	case !h.authManager.ClaudeResetCredentialCurrent(auth):
		return claudeResetAbortCredential
	case !coreauth.ClaudeResetGrantSpendable(grant, at):
		return claudeResetAbortGrantExpired
	case retry != nil && at.Sub(retry.sentAt) >= claudeResetRetryWindow:
		return claudeResetAbortRetryWindow
	}
	return ""
}

// claudeResetReplay answers a key delivered again with what the ledger holds,
// and with the newest record of the same claim when a retry followed it.
func claudeResetReplay(record, latest claudeResetSpendRecord) gin.H {
	answer := gin.H{
		"replayed": true, "auth_index": record.AuthIndex, "kind": record.Kind, "grant_id": record.GrantID, "request_id": record.RequestID,
		"retry": record.Retry, "result": record.Result, "at": record.At, "resets_left_before": record.ResetsLeftBefore, "resets_left_after": record.ResetsLeftAfter,
	}
	if record.Reason != "" {
		answer["reason"] = record.Reason
	}
	if record.Phase == claudeResetPhaseSent {
		answer["recorded"] = "the claim was sent and no answer was recorded"
	}
	settled := claudeResetSettles(record)
	if latest.Operation != "" && latest.Operation != record.Operation {
		answer["latest"] = gin.H{"retry": latest.Retry, "phase": latest.Phase, "result": latest.Result, "at": latest.At}
		settled = settled || (latest.Phase == claudeResetPhaseAnswered && claudeResetSettles(latest))
	}
	if !record.Retry && !settled {
		answer["retry_until"] = record.At.Add(claudeResetRetryWindow)
	}
	return answer
}

// SpendClaudeLimitReset spends one banked reset of the kind the caller names,
// on one Claude seat. A request that names no kind, carries no
// Idempotency-Key, or names a kind the seat's fresh grants read does not
// list, is refused and spends nothing. Only an enabled OAuth seat spends. The
// organization is reserved before the fresh grants read that authorizes the
// claim, and the claim is judged again after its sent line is durable.
// Nothing in the hub calls this but the management route; it never retries on
// its own.
func (h *Handler) SpendClaudeLimitReset(c *gin.Context) {
	request, invalid := parseClaudeResetSpendRequest(c.Request.Body)
	if invalid != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": invalid, "kinds_allowed": coreauth.ClaudeResetKindNames()})
		return
	}
	kind := strings.ToLower(request.kind)
	if kind == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind is required: name the kind of reset to spend", "kinds_allowed": coreauth.ClaudeResetKindNames()})
		return
	}
	if !coreauth.ValidClaudeResetKind(kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown reset kind", "kinds_allowed": coreauth.ClaudeResetKindNames()})
		return
	}
	if request.grantID != "" && !resetclaim.ValidGrantID(request.grantID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "malformed grant_id"})
		return
	}
	if request.retryRequestID != "" && !resetclaim.ValidRequestID(request.retryRequestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "malformed retry_request_id"})
		return
	}
	if request.authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	key := c.GetHeader(claudeResetIdempotencyHeader)
	if !claudeResetIdempotencyKeyPattern.MatchString(key) {
		c.JSON(http.StatusBadRequest, gin.H{"error": claudeResetIdempotencyRequired})
		return
	}

	// One journal serves the whole spend. A key delivered again is answered
	// from the ledger first, whatever has become of the seat since.
	ledger := &h.claudeResetSpends
	journal, errJournal := ledger.journal(h.claudeResetLedgerDir())
	if errJournal != nil {
		log.WithError(errJournal).Error("Claude reset spend ledger has no directory; spends refused")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the spend ledger has no directory; nothing spent"})
		return
	}
	operation := claudeResetOperation(key)
	replay, retry, heldStatus, held := journal.begin(operation, request, kind, ledger.clock())
	if held != "" {
		c.JSON(heldStatus, gin.H{"error": held + "; nothing spent"})
		return
	}
	c.Header("Cache-Control", "no-store")
	if replay != nil {
		c.JSON(http.StatusOK, claudeResetReplay(*replay, journal.latest(replay.RequestID)))
		return
	}
	defer journal.finish(request.authIndex, operation)
	auth := h.authByIndex(request.authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		c.JSON(http.StatusNotFound, gin.H{"error": "Claude seat not found"})
		return
	}
	token, spends := coreauth.ClaudeResetSpendToken(auth)
	if !spends {
		c.JSON(http.StatusConflict, gin.H{"error": claudeResetCredentialRefusal + "; nothing spent"})
		return
	}
	// One private copy of the configuration serves the whole spend.
	cfg := h.claudeResetConfig()
	grantID := request.grantID
	if retry != nil {
		if kind != retry.kind || (grantID != "" && grantID != retry.grantID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a retry must name the kind and grant of the claim it retries", "kind": retry.kind, "grant_id": retry.grantID})
			return
		}
		grantID = retry.grantID
	}
	ctx, client := c.Request.Context(), h.claudeResetAuth(cfg, auth)
	userAgent := claudeauth.LimitResetUserAgent(helps.DefaultClaudeVersion(cfg))

	// The organization is resolved, and its account reserved, before the
	// grants read that authorizes the claim.
	profile, errProfile := client.FetchLimitResetSpendProfile(ctx, token, userAgent)
	if errProfile != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "organization read failed; nothing spent"})
		return
	}
	organization := profile.Organization.UUID
	account := claudeResetAccount(organization)
	if retry != nil && retry.account != account {
		c.JSON(http.StatusConflict, gin.H{"error": "the seat's account changed since the unknown claim; nothing spent"})
		return
	}
	if heldAccount := journal.reserveAccount(request.authIndex, account, ledger.clock()); heldAccount != "" {
		c.JSON(http.StatusConflict, gin.H{"error": heldAccount + "; nothing spent"})
		return
	}

	// The last grants read is this fresh one, under the reservation; the quota
	// cache never decides a spend. Grants are judged when the read returns.
	before, _, readStatus, errRead := readClaudeResetStatus(ctx, client, token, userAgent)
	if errRead != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "fresh grants read failed; nothing spent", "upstream_status": readStatus})
		return
	}
	now := ledger.clock()
	kinds := coreauth.SummarizeClaudeResetKinds(before.Grants, now)
	if retry != nil && now.Sub(retry.sentAt) >= claudeResetRetryWindow {
		c.JSON(http.StatusConflict, gin.H{"error": "the ten-minute retry window has passed; nothing spent"})
		return
	}
	grant, refusalStatus, refusal := claudeResetAuthorizeGrant(before, kinds, kind, grantID, retry != nil, now)
	if refusal != "" {
		ofKind := make([]coreauth.ClaudeResetGrant, 0)
		for _, listed := range before.Grants {
			if listed.Kind == kind {
				ofKind = append(ofKind, listed)
			}
		}
		c.JSON(refusalStatus, gin.H{"error": refusal + "; nothing spent", "kind": kind, "kinds": kinds, "grants_of_kind": ofKind, "eligible": before.Eligible, "ineligible_reason": before.IneligibleReason, "cooldown_until": before.CooldownUntil})
		return
	}
	requestID := uuid.NewString()
	if retry != nil {
		requestID = retry.requestID
	}
	record := claudeResetSpendRecord{
		Phase: claudeResetPhaseSent, Operation: operation, Account: account, At: now, AuthIndex: request.authIndex, Kind: kind, GrantID: grant.ID,
		NamedGrant: request.grantID, RequestID: requestID, Retry: retry != nil, Result: claudeauth.LimitResetResultUnknown, ResetsLeftBefore: claudeResetsLeftByKind(kinds),
	}
	if errLedger := journal.markSent(record); errLedger != nil {
		log.WithError(errLedger).Error("Claude reset spend ledger not writable; claim not sent")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the spend ledger is not writable; nothing spent"})
		return
	}
	notSent := func(status int, reason, message string) {
		record.Phase, record.Result, record.Reason = claudeResetPhaseAnswered, claudeResetResultNotSent, reason
		answer := gin.H{"error": message + "; nothing spent", "reason": reason, "request_id": requestID}
		if errAnswer := journal.answer(record); errAnswer != nil {
			answer["ledger_error"] = claudeResetLedgerErrorText
		}
		c.JSON(status, answer)
	}
	if abort := h.claudeResetDispatchAbort(auth, grant, retry, ledger.clock()); abort != "" {
		notSent(http.StatusConflict, abort, "the claim was judged again after its record was written and not sent")
		return
	}
	claim := resetclaim.Claim{OrganizationUUID: organization, GrantID: grant.ID, RequestID: requestID}
	outcome, errClaim := sendClaudeResetClaim(ctx, client, token, userAgent, claim)
	if errClaim != nil {
		notSent(http.StatusBadGateway, claudeResetAbortTransport, "claim not sent")
		return
	}

	// A known outcome is recorded at once, before and apart from the balance read.
	record.Phase, record.Result, record.Reason, record.HTTPStatus = claudeResetPhaseAnswered, outcome.Result, outcome.Reason, outcome.HTTPStatus
	errAnswer := journal.answer(record)
	answer := gin.H{
		"auth_index": request.authIndex, "kind": kind, "grant_id": grant.ID, "request_id": requestID, "retry": retry != nil,
		"result": outcome.Result, "provider": outcome, "kinds_before": kinds, "kinds_after": nil,
	}
	if errAnswer != nil {
		answer["ledger_error"] = claudeResetLedgerErrorText
	}
	if !claudeResetSettles(record) {
		firstSent := now
		if retry != nil {
			firstSent = retry.sentAt
		}
		answer["retry_until"] = firstSent.Add(claudeResetRetryWindow)
	}
	if outcome.Result == claudeauth.LimitResetResultReset || outcome.Result == claudeauth.LimitResetResultAlreadyUsed {
		// The spend never edits the hub's routing state: which of the seat's
		// cooldowns the provider's refill lifted is not known here, and a
		// blanket clear would also lift limits this kind did not refill,
		// independent failures, or a replacement credential's cooldown. A
		// usage poll of this same credential is asked for; until the operator
		// calls the routing reset, the hub's own cooldown can keep the seat out
		// of rotation.
		answer["routing"] = gin.H{
			"cooldown_changed":     false,
			"usage_poll_queued":    h.authManager.RequestClaudeResetFollowUpPoll(auth),
			"cooldown_reset_route": "/v8/management/routing/cooldown/reset",
			"note":                 "a confirmed reset does not return the seat to rotation: the hub's own cooldown can hold it until the routing reset is called",
		}
	}
	afterStarted := time.Now()
	after, afterBody, _, errAfter := readClaudeResetStatus(ctx, client, token, userAgent)
	// The reset cache shows this read, or marks its older one stale; other
	// seats listing the grant turn stale when the claim spent or may have.
	spent := outcome.Result == claudeauth.LimitResetResultReset || outcome.Result == claudeauth.LimitResetResultAlreadyUsed || outcome.Result == claudeauth.LimitResetResultUnknown
	h.authManager.PublishClaudeResetSpendRead(auth, grant.ID, spent, afterStarted, afterBody, errAfter)
	if errAfter != nil {
		answer["after_read_error"] = "the grants read after the claim failed"
		c.JSON(http.StatusOK, answer)
		return
	}
	kindsAfter := coreauth.SummarizeClaudeResetKinds(after.Grants, ledger.clock())
	answer["kinds_after"] = kindsAfter
	if errAnswer == nil {
		balance := claudeResetSpendRecord{
			Phase: claudeResetPhaseBalance, Operation: operation, At: ledger.clock(), AuthIndex: request.authIndex, RequestID: requestID,
			ResetsLeftAfter: claudeResetsLeftByKind(kindsAfter),
		}
		if errBalance := journal.recordBalance(balance); errBalance != nil {
			answer["balance_error"] = claudeResetBalanceErrorText
		}
	}
	c.JSON(http.StatusOK, answer)
}
