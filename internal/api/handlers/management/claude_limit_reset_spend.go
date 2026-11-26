package management

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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
	// claudeResetLedgerSentinelName records that the ledger was created and
	// the ledger's length after its last durable write. A ledger missing,
	// emptied or shorter than that length is refused, never read as fewer
	// spends.
	claudeResetLedgerSentinelName = "claude-limit-reset-spends.created"
	// claudeResetLedgerLockName is the file whose exclusive lock one hub
	// process holds for its whole life; a second process refuses every spend.
	claudeResetLedgerLockName = "claude-limit-reset-spends.lock"
	// claudeResetRetryWindow bounds the explicit retry of a claim whose outcome
	// is unknown; the hub never retries on its own.
	claudeResetRetryWindow = 10 * time.Minute
	// claudeResetDispatchLimit bounds the time from a sent line's stamp to its
	// claim's dispatch. A claim whose line took longer to become durable is
	// not sent, so its hold and retry window never start long before it
	// leaves, in the running hub or after a restart.
	claudeResetDispatchLimit = 5 * time.Second
	// claudeResetClockTolerance is how far the clock may read behind a stamp
	// the ledger holds before the hub stops trusting it to judge a window: a
	// sent line is never stamped ahead of the clock by more than this, and a
	// retry or dispatch judged on a clock behind its claim's stamp sends
	// nothing.
	claudeResetClockTolerance = time.Second
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
	// claudeResetResultNotSent records a claim the hub certainly did not send.
	claudeResetResultNotSent = "not_sent"
)

// Reason codes of a claim the hub did not send. Each is a code, never text.
const (
	claudeResetAbortTransport      = "transport_refused"
	claudeResetAbortCredential     = "credential_changed_at_dispatch"
	claudeResetAbortGrantExpired   = "grant_expired_at_dispatch"
	claudeResetAbortRetryWindow    = "retry_window_passed_at_dispatch"
	claudeResetAbortLedgerStall    = "ledger_write_stalled_at_dispatch"
	claudeResetAbortClockBehind    = "clock_behind_the_claim_at_dispatch"
	claudeResetAbortLedgerRefused  = "ledger_refused_at_dispatch"
	claudeResetLedgerErrorText     = "the outcome is not in the spend ledger: the ledger refuses every spend until the hub restarts, the seat stays held, and no retry of this claim is accepted"
	claudeResetBalanceErrorText    = "what is left after the claim is not in the spend ledger, which refuses every spend until the hub restarts"
	claudeResetCredentialRefusal   = "only an enabled OAuth Claude seat can spend a banked reset"
	claudeResetIdempotencyRequired = "exactly one Idempotency-Key header is required, with no comma in it: a fresh key for each new spend, the same key to deliver one again"
)

// Codes of a spend ledger the hub refuses. A refusal also names the ledger,
// the first line it could not accept, and the cure.
const (
	claudeResetRefusalUnreadable  = "ledger_unreadable"
	claudeResetRefusalInvalidLine = "ledger_invalid_line"
	claudeResetRefusalMissing     = "ledger_missing"
	claudeResetRefusalShortened   = "ledger_shortened"
	claudeResetRefusalHeld        = "ledger_held_by_another_process"
	claudeResetRefusalChanged     = "ledger_changed_under_running_hub"
	claudeResetRefusalWriteFailed = "ledger_write_failed"

	claudeResetCureRepair  = "check the usage page of every seat with a claim in the last ten minutes; then repair or remove that line (an incomplete last line is a write cut short, which no claim followed), or move the ledger and its sentinel aside together, which forgets every recorded Idempotency-Key and hold; then restart the hub"
	claudeResetCureRestore = "restore the ledger the hub wrote; or, after checking the usage page of every seat with a claim in the last ten minutes, move the sentinel aside as well, which forgets every recorded Idempotency-Key and hold; then restart the hub"
	claudeResetCureHeld    = "stop the other hub process that uses this log directory, or give each hub its own log directory; then restart this hub"
	claudeResetCureRestart = "check the ledger file and the disk, then restart the hub: a restart loads the ledger again and refuses it if it does not validate"
)

var (
	claudeResetOperationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	claudeResetAccountPattern   = regexp.MustCompile(`^[0-9a-f]{24}$`)
	// A key is printable ASCII with no comma, so two header fields can never
	// be read as one key joined by a comma.
	claudeResetIdempotencyKeyPattern = regexp.MustCompile(`^[\x21-\x2b\x2d-\x7e]{1,200}$`)
)

// errClaudeResetLedgerLocked is what the lock attempt returns when another
// process, or another open of the lock file, holds the ledger directory.
var errClaudeResetLedgerLocked = errors.New("another process holds the reset spend ledger lock")

// errClaudeResetAfterReadPending marks a seat's cached reset status stale
// while the grants read after a claim is still in flight.
var errClaudeResetAfterReadPending = errors.New("the grants read after the claim has not returned yet")

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

// claudeResetUnsettledClaim is a first claim whose outcome is unknown; it
// holds its seat and its account from its sent line's stamp for the retry
// window.
type claudeResetUnsettledClaim struct {
	kind, grantID, requestID, account string
	sentAt                            time.Time
}

// claudeResetLedgerRefusal is why a journal refuses every spend: what is
// wrong, where, and what an operator does about it.
type claudeResetLedgerRefusal struct {
	code, path, problem, cure string
	// line is the first line the hub could not accept, from 1; 0 when the
	// refusal is not about one line.
	line int
}

func (r *claudeResetLedgerRefusal) Error() string {
	if r.line > 0 {
		return fmt.Sprintf("%s: %s line %d: %s", r.code, r.path, r.line, r.problem)
	}
	return fmt.Sprintf("%s: %s: %s", r.code, r.path, r.problem)
}

// answer is what a spend or a replay refused by the ledger says.
func (r *claudeResetLedgerRefusal) answer() gin.H {
	answer := gin.H{
		"error":          "the spend ledger cannot be read or trusted, so earlier claims cannot be ruled out (" + r.problem + "); this request sent nothing",
		"ledger_refusal": r.code, "ledger": r.path, "cure": r.cure,
	}
	if r.line > 0 {
		answer["ledger_line"] = r.line
	}
	return answer
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

// claudeResetJournal is one ledger's state. From the journal's first open the
// process holds the ledger directory's lock for its whole life, and it trusts
// the ledger file only while the file is provably the one it loaded and
// wrote: the same file, the length of its last durable write, ending in the
// record it last wrote. Any doubt refuses every spend until a restart loads a
// ledger that validates.
type claudeResetJournal struct {
	mu  sync.Mutex
	dir string
	// opened is set once the journal took the lock and loaded the ledger.
	opened bool
	// refused, once set, refuses every spend, replay and listing until the
	// hub restarts.
	refused *claudeResetLedgerRefusal
	// lock, file and sentinel stay open for the life of the process; file and
	// sentinel are nil until they exist.
	lock, file, sentinel *os.File
	// size is the ledger's length after this process's last durable write,
	// tail the last line it holds, and lastAt the newest stamp.
	size   int64
	tail   []byte
	lastAt time.Time
	// syncDirs are the directories whose entries the first append makes
	// durable: the ledger's own and every ancestor it can open.
	syncDirs        []string
	synced          bool
	busy            map[string]bool
	keysInFlight    map[string]bool
	accountInFlight map[string]string
	unsettled       map[string]*claudeResetUnsettledClaim
	awaiting        map[string]claudeResetSpendRecord
	last            map[string]claudeResetSpendRecord
	operations      map[string]claudeResetSpendRecord
	// claims is the newest record of each claim and firstSends its first sent
	// line, by request id; balanced marks the operations with a balance.
	claims     map[string]claudeResetSpendRecord
	firstSends map[string]claudeResetSpendRecord
	balanced   map[string]bool
}

func (j *claudeResetJournal) ledgerPath() string {
	return filepath.Join(j.dir, claudeResetSpendLedgerName)
}

func (j *claudeResetJournal) sentinelPath() string {
	return filepath.Join(j.dir, claudeResetLedgerSentinelName)
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

var errClaudeResetRecordInvalid = errors.New("the line is an incomplete or invalid record")

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

// checkLocked validates one record against the journal, by the rules a live
// spend is admitted by, whether the record is about to be written or is read
// back on a start: its own fields, a stamp not before the record above it, an
// operation and a first request id never used before, no other seat of its
// account holding a claim with an unknown outcome, a retry only of a held
// claim whose answer is recorded, an answer only to its sent line, and one
// balance per answer.
func (j *claudeResetJournal) checkLocked(record claudeResetSpendRecord) error {
	if !claudeResetRecordValid(record) {
		return errClaudeResetRecordInvalid
	}
	if record.At.Before(j.lastAt) {
		return errors.New("the record is stamped before the record above it")
	}
	seat := record.AuthIndex
	switch record.Phase {
	case claudeResetPhaseSent:
		if _, used := j.operations[record.Operation]; used {
			return errors.New("the record repeats an operation already recorded")
		}
		for other := range j.unsettled {
			if held := j.openLocked(other, record.At); other != seat && held != nil && held.account == record.Account {
				return errors.New("the claim was sent while another seat of its account held a claim with an unknown outcome")
			}
		}
		open := j.openLocked(seat, record.At)
		if !record.Retry {
			if _, used := j.claims[record.RequestID]; used {
				return errors.New("the record sends again a request id already sent")
			}
			if open != nil {
				return errors.New("the claim was sent while its seat was held")
			}
			return nil
		}
		if open == nil || open.requestID != record.RequestID || open.kind != record.Kind || open.grantID != record.GrantID || open.account != record.Account {
			return errors.New("the record retries no held claim")
		}
		if latest := j.claims[record.RequestID]; latest.Phase != claudeResetPhaseAnswered {
			return errors.New("the record retries a claim whose answer is not recorded")
		}
	case claudeResetPhaseAnswered:
		waiting, ok := j.awaiting[seat]
		if !ok || waiting.Operation != record.Operation || waiting.RequestID != record.RequestID || waiting.Retry != record.Retry ||
			waiting.Kind != record.Kind || waiting.GrantID != record.GrantID || waiting.NamedGrant != record.NamedGrant || waiting.Account != record.Account {
			return errors.New("the record answers no sent claim")
		}
	case claudeResetPhaseBalance:
		answered, ok := j.operations[record.Operation]
		if !ok || answered.Phase != claudeResetPhaseAnswered || answered.RequestID != record.RequestID || answered.AuthIndex != seat {
			return errors.New("the record is a balance of no answered claim")
		}
		if j.balanced[record.Operation] {
			return errors.New("the record is a second balance of one answer")
		}
	}
	return nil
}

// foldLocked applies a checked record to the journal's state.
func (j *claudeResetJournal) foldLocked(record claudeResetSpendRecord) {
	j.lastAt = record.At
	seat := record.AuthIndex
	switch record.Phase {
	case claudeResetPhaseSent:
		j.awaiting[seat] = record
		if !record.Retry {
			j.unsettled[seat] = &claudeResetUnsettledClaim{kind: record.Kind, grantID: record.GrantID, requestID: record.RequestID, account: record.Account, sentAt: record.At}
			j.firstSends[record.RequestID] = record
		}
	case claudeResetPhaseAnswered:
		delete(j.awaiting, seat)
		if open := j.unsettled[seat]; open != nil && open.requestID == record.RequestID && claudeResetSettles(record) {
			delete(j.unsettled, seat)
		}
	case claudeResetPhaseBalance:
		j.balanced[record.Operation] = true
		answered := j.operations[record.Operation]
		answered.ResetsLeftAfter = record.ResetsLeftAfter
		j.operations[record.Operation] = answered
		if last, ok := j.last[seat]; ok && last.Operation == record.Operation {
			j.last[seat] = answered
		}
		if claim, ok := j.claims[record.RequestID]; ok && claim.Operation == record.Operation {
			j.claims[record.RequestID] = answered
		}
		return
	}
	j.last[seat] = record
	j.operations[record.Operation] = record
	j.claims[record.RequestID] = record
}

// refuseLocked makes the journal refuse every spend from now until a
// restart, and logs why once.
func (j *claudeResetJournal) refuseLocked(code string, line int, problem, cure string) *claudeResetLedgerRefusal {
	if j.refused == nil {
		j.refused = &claudeResetLedgerRefusal{code: code, path: j.ledgerPath(), line: line, problem: problem, cure: cure}
		log.WithField("ledger_refusal", code).Errorf("Claude reset spend ledger refused; every spend is refused until a restart loads a ledger that validates: %s", j.refused.Error())
	}
	return j.refused
}

// claudeResetMakeDirs creates dir and its missing parents, and returns the
// directories whose entries the first append must make durable: dir and
// every ancestor.
func claudeResetMakeDirs(dir string) ([]string, error) {
	// Every ancestor, not only those created here: logging or an earlier
	// process may have created one without syncing its parent.
	syncDirs := []string{dir}
	for current := dir; ; {
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		syncDirs = append(syncDirs, parent)
		current = parent
	}
	return syncDirs, os.MkdirAll(dir, 0o700)
}

// startLocked opens the journal once: it creates the ledger directory, takes
// the directory's lock for the life of the process, then loads the ledger.
// It never runs without the lock: a lock it cannot take refuses every spend.
func (j *claudeResetJournal) startLocked() *claudeResetLedgerRefusal {
	if j.refused != nil || j.opened {
		return j.refused
	}
	syncDirs, errMkdir := claudeResetMakeDirs(j.dir)
	if errMkdir != nil {
		return j.refuseLocked(claudeResetRefusalUnreadable, 0, "its directory cannot be created: "+errMkdir.Error(), claudeResetCureRestart)
	}
	j.syncDirs = syncDirs
	lock, errOpen := os.OpenFile(filepath.Join(j.dir, claudeResetLedgerLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if errOpen != nil {
		return j.refuseLocked(claudeResetRefusalUnreadable, 0, "its directory's lock file cannot be opened: "+errOpen.Error(), claudeResetCureRestart)
	}
	if errLock := lockClaudeResetLedgerFile(lock); errLock != nil {
		closeClaudeResetFile(lock)
		if errors.Is(errLock, errClaudeResetLedgerLocked) {
			return j.refuseLocked(claudeResetRefusalHeld, 0, "another process holds its directory's lock", claudeResetCureHeld)
		}
		return j.refuseLocked(claudeResetRefusalUnreadable, 0, "its directory's lock cannot be taken: "+errLock.Error(), claudeResetCureRestart)
	}
	j.lock = lock
	if refusal := j.loadLocked(); refusal != nil {
		return refusal
	}
	j.opened = true
	return nil
}

func closeClaudeResetFile(f *os.File) {
	if errClose := f.Close(); errClose != nil {
		log.WithError(errClose).Warn("Claude reset spend ledger: a file could not be closed")
	}
}

// claudeResetSentinelWidth is the fixed width of the length the sentinel
// records, so each update rewrites it in place at one size.
const claudeResetSentinelWidth = 20

func claudeResetSentinelContent(length int64) []byte {
	return []byte(fmt.Sprintf("%0*d\n", claudeResetSentinelWidth, length))
}

// readClaudeResetSentinel reads the ledger length the sentinel records.
func readClaudeResetSentinel(path string) (length int64, exists bool, err error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, false, nil
		}
		return 0, true, errRead
	}
	if len(raw) != claudeResetSentinelWidth+1 || raw[claudeResetSentinelWidth] != '\n' {
		return 0, true, errors.New("it does not record a length")
	}
	for _, digit := range raw[:claudeResetSentinelWidth] {
		if digit < '0' || digit > '9' {
			return 0, true, errors.New("it does not record a length")
		}
	}
	length, errParse := strconv.ParseInt(string(raw[:claudeResetSentinelWidth]), 10, 64)
	if errParse != nil {
		return 0, true, errors.New("it does not record a length")
	}
	return length, true, nil
}

// loadLocked restores the journal from the ledger file. It fails closed: a
// ledger that cannot be opened or read, a ledger missing, emptied or shorter
// than the length its sentinel records, or a line that is not a valid record
// in a valid order refuses every spend. A last line with no newline is a
// write cut short, refused whatever it parses as, the same way on every
// start: the hub never appends to a ledger it refused.
func (j *claudeResetJournal) loadLocked() *claudeResetLedgerRefusal {
	j.busy = make(map[string]bool)
	j.keysInFlight = make(map[string]bool)
	j.accountInFlight = make(map[string]string)
	j.unsettled = make(map[string]*claudeResetUnsettledClaim)
	j.awaiting = make(map[string]claudeResetSpendRecord)
	j.last = make(map[string]claudeResetSpendRecord)
	j.operations = make(map[string]claudeResetSpendRecord)
	j.claims = make(map[string]claudeResetSpendRecord)
	j.firstSends = make(map[string]claudeResetSpendRecord)
	j.balanced = make(map[string]bool)
	recorded, sentinelExists, errSentinel := readClaudeResetSentinel(j.sentinelPath())
	if errSentinel != nil {
		return j.refuseLocked(claudeResetRefusalUnreadable, 0, "its sentinel cannot be read: "+errSentinel.Error(), claudeResetCureRestore)
	}
	// Not O_APPEND: a record is written at the ledger's known length, and a
	// failed one can be cut back off on every platform.
	file, errOpen := os.OpenFile(j.ledgerPath(), os.O_RDWR, 0)
	if errOpen != nil {
		if !os.IsNotExist(errOpen) {
			return j.refuseLocked(claudeResetRefusalUnreadable, 0, "it cannot be opened: "+errOpen.Error(), claudeResetCureRestart)
		}
		if sentinelExists {
			return j.refuseLocked(claudeResetRefusalMissing, 0, "it is missing although its sentinel records that one was created", claudeResetCureRestore)
		}
		return nil
	}
	if refusal := j.readLocked(file, recorded, sentinelExists); refusal != nil {
		closeClaudeResetFile(file)
		return refusal
	}
	j.file = file
	if sentinelExists {
		sentinel, errSentinelOpen := os.OpenFile(j.sentinelPath(), os.O_RDWR, 0)
		if errSentinelOpen != nil {
			return j.refuseLocked(claudeResetRefusalUnreadable, 0, "its sentinel cannot be opened: "+errSentinelOpen.Error(), claudeResetCureRestart)
		}
		j.sentinel = sentinel
	}
	return nil
}

// readLocked checks and folds every line of the ledger file.
func (j *claudeResetJournal) readLocked(file *os.File, recorded int64, sentinelExists bool) *claudeResetLedgerRefusal {
	if info, errStat := file.Stat(); errStat != nil || !info.Mode().IsRegular() {
		return j.refuseLocked(claudeResetRefusalUnreadable, 0, "it is not a regular file that can be read", claudeResetCureRestart)
	}
	reader := bufio.NewReader(file)
	var size int64
	for number := 1; ; number++ {
		line, errRead := reader.ReadBytes('\n')
		if errRead != nil && !errors.Is(errRead, io.EOF) {
			return j.refuseLocked(claudeResetRefusalUnreadable, number, "it cannot be read: "+errRead.Error(), claudeResetCureRestart)
		}
		if len(line) == 0 {
			break
		}
		if line[len(line)-1] != '\n' {
			return j.refuseLocked(claudeResetRefusalInvalidLine, number, "its last line has no newline: a write cut short, whatever it parses as", claudeResetCureRepair)
		}
		var record claudeResetSpendRecord
		if json.Unmarshal(line, &record) != nil {
			return j.refuseLocked(claudeResetRefusalInvalidLine, number, "the line is not a record", claudeResetCureRepair)
		}
		if errCheck := j.checkLocked(record); errCheck != nil {
			return j.refuseLocked(claudeResetRefusalInvalidLine, number, errCheck.Error(), claudeResetCureRepair)
		}
		j.foldLocked(record)
		size += int64(len(line))
		j.tail = line
	}
	if sentinelExists && size < recorded {
		return j.refuseLocked(claudeResetRefusalShortened, 0, fmt.Sprintf("it holds %d bytes, fewer than the %d its sentinel records: it was cut, emptied or replaced by an older copy", size, recorded), claudeResetCureRestore)
	}
	j.size = size
	return nil
}

// verifyLocked opens the journal, then proves the ledger is still the file
// this process loaded and wrote, before every reservation, append, replay
// answer and listing: the same file, of the length of its last durable write,
// with its last record in place, beside the sentinel it wrote. Any doubt
// refuses every spend until a restart; nothing is ever created where a ledger
// or a sentinel already was.
func (j *claudeResetJournal) verifyLocked() *claudeResetLedgerRefusal {
	if refusal := j.startLocked(); refusal != nil {
		return refusal
	}
	changed := func(problem string) *claudeResetLedgerRefusal {
		return j.refuseLocked(claudeResetRefusalChanged, 0, problem, claudeResetCureRestart)
	}
	// The lock holds only while its file is the one at the lock path: another
	// process could lock a new file put there.
	lockOnDisk, errLockStat := os.Lstat(filepath.Join(j.dir, claudeResetLedgerLockName))
	if j.lock == nil || errLockStat != nil {
		return changed("the ledger directory's lock file was removed or is not held")
	}
	if lockHeld, errLockHeld := j.lock.Stat(); errLockHeld != nil || !os.SameFile(lockOnDisk, lockHeld) {
		return changed("the ledger directory's lock file was replaced")
	}
	onDisk, errStat := os.Lstat(j.ledgerPath())
	if j.file == nil {
		if !os.IsNotExist(errStat) {
			return changed("a ledger appeared that this process did not write")
		}
		if _, errSentinel := os.Lstat(j.sentinelPath()); !os.IsNotExist(errSentinel) {
			return changed("a sentinel appeared that this process did not write")
		}
		return nil
	}
	held, errHeld := j.file.Stat()
	switch {
	case errStat != nil:
		return changed("the ledger was removed or cannot be reached")
	case errHeld != nil || !os.SameFile(onDisk, held):
		return changed("the ledger was replaced by another file")
	case held.Size() != j.size:
		return changed(fmt.Sprintf("the ledger holds %d bytes, not the %d this process last wrote", held.Size(), j.size))
	}
	if len(j.tail) > 0 {
		last := make([]byte, len(j.tail))
		if _, errRead := j.file.ReadAt(last, j.size-int64(len(j.tail))); errRead != nil || !bytes.Equal(last, j.tail) {
			return changed("the ledger's last record is not the one this process wrote")
		}
	}
	if j.sentinel != nil {
		sentinelOnDisk, errSentinel := os.Lstat(j.sentinelPath())
		sentinelHeld, errSentinelHeld := j.sentinel.Stat()
		if errSentinel != nil || errSentinelHeld != nil || !os.SameFile(sentinelOnDisk, sentinelHeld) {
			return changed("the ledger's sentinel was removed or replaced")
		}
	}
	return nil
}

// errClaudeResetClockBehind refuses one spend, not the journal: the clock
// reads earlier than the ledger's newest stamp, so a hold or retry window
// stamped now would run longer than the time it means.
var errClaudeResetClockBehind = errors.New("the hub's clock reads earlier than the newest record in the spend ledger; was it stepped back?")

// stampLocked is the time of a record about to be written: the clock without
// its monotonic reading, so a stamp means the same in memory and on disk. The
// ledger's times only go forward: a line after a clock step back is stamped
// with the newest stamp, except a sent line, which starts a hold and a retry
// window and so is never stamped ahead of the clock by more than the
// tolerance; that spend is refused instead.
func (j *claudeResetJournal) stampLocked(record claudeResetSpendRecord, clock func() time.Time) (time.Time, error) {
	at := clock().Round(0)
	if !at.Before(j.lastAt) {
		return at, nil
	}
	if record.Phase == claudeResetPhaseSent && j.lastAt.Sub(at) > claudeResetClockTolerance {
		return time.Time{}, errClaudeResetClockBehind
	}
	return j.lastAt, nil
}

// appendLocked stamps one record, checks it and makes it durable: the line
// synced, on the first append the directory entries synced, then the length
// the sentinel records. Any failure, including one that leaves the write
// uncertain, cuts the line back off where the disk allows and refuses every
// spend until a restart, so memory never runs ahead of the disk.
func (j *claudeResetJournal) appendLocked(record *claudeResetSpendRecord, clock func() time.Time) error {
	if refusal := j.verifyLocked(); refusal != nil {
		return refusal
	}
	at, errClock := j.stampLocked(*record, clock)
	if errClock != nil {
		return errClock
	}
	record.At = at
	if errCheck := j.checkLocked(*record); errCheck != nil {
		return errCheck
	}
	line, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return errMarshal
	}
	line = append(line, '\n')
	length := j.size + int64(len(line))
	if errWrite := j.writeLocked(line, length); errWrite != nil {
		j.rollBackLocked()
		return j.refuseLocked(claudeResetRefusalWriteFailed, 0, "a record could not be made durable: "+errWrite.Error(), claudeResetCureRestart)
	}
	j.size, j.tail, j.synced = length, line, true
	j.foldLocked(*record)
	return nil
}

// writeLocked writes one line and makes it durable. The ledger is created
// only where none was loaded and none exists now.
func (j *claudeResetJournal) writeLocked(line []byte, length int64) error {
	if j.file == nil {
		file, errCreate := os.OpenFile(j.ledgerPath(), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errCreate != nil {
			return errCreate
		}
		j.file = file
	}
	if _, errWrite := j.file.WriteAt(line, j.size); errWrite != nil {
		return errWrite
	}
	if errSync := claudeResetSyncFile(j.file); errSync != nil {
		return errSync
	}
	if !j.synced {
		if errSync := claudeResetSyncDirectories(j.syncDirs); errSync != nil {
			return errSync
		}
	}
	return j.recordLengthLocked(length)
}

// recordLengthLocked writes the ledger's new length into the sentinel. The
// sentinel is created once the ledger's first record is durable.
func (j *claudeResetJournal) recordLengthLocked(length int64) error {
	created := false
	if j.sentinel == nil {
		sentinel, errCreate := os.OpenFile(j.sentinelPath(), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errCreate != nil {
			return errCreate
		}
		j.sentinel, created = sentinel, true
	}
	if _, errWrite := j.sentinel.WriteAt(claudeResetSentinelContent(length), 0); errWrite != nil {
		return errWrite
	}
	if errSync := claudeResetSyncFile(j.sentinel); errSync != nil {
		return errSync
	}
	if created {
		return claudeResetSyncDirectories([]string{j.dir})
	}
	return nil
}

// rollBackLocked cuts a failed record back off the ledger and restores the
// sentinel's length, as far as the disk allows; the journal refuses anyway.
func (j *claudeResetJournal) rollBackLocked() {
	if j.file != nil {
		if errTruncate := j.file.Truncate(j.size); errTruncate != nil {
			log.WithError(errTruncate).Error("Claude reset spend ledger: a failed record could not be cut back")
		} else if errSync := j.file.Sync(); errSync != nil {
			log.WithError(errSync).Error("Claude reset spend ledger: a cut-back record could not be synced")
		}
	}
	if j.sentinel != nil {
		if _, errWrite := j.sentinel.WriteAt(claudeResetSentinelContent(j.size), 0); errWrite != nil {
			log.WithError(errWrite).Error("Claude reset spend ledger: the sentinel's length could not be restored")
		} else if errSync := j.sentinel.Sync(); errSync != nil {
			log.WithError(errSync).Error("Claude reset spend ledger: the sentinel's restored length could not be synced")
		}
	}
}

// syncClaudeResetDirectories makes the entries of each directory durable, as
// the refresh journal does before an exchange.
func syncClaudeResetDirectories(dirs []string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	for i, dir := range dirs {
		handle, errOpen := os.Open(dir)
		if errOpen != nil {
			// An ancestor the service may traverse but not read cannot be
			// synced by it; only the ledger's own directory must open.
			if i > 0 && os.IsPermission(errOpen) {
				continue
			}
			return errOpen
		}
		errSync := handle.Sync()
		if errClose := handle.Close(); errSync == nil {
			errSync = errClose
		}
		if errSync != nil {
			return errSync
		}
	}
	return nil
}

// claudeResetSyncDirectories and claudeResetSyncFile are the syncs appends
// use; a test replaces them to make a sync fail.
var (
	claudeResetSyncDirectories = syncClaudeResetDirectories
	claudeResetSyncFile        = func(f *os.File) error { return f.Sync() }
)

// errClaudeResetHistoryUnavailable is what a read shows in place of a seat's
// last spend while the ledger cannot be read: unknown, not "never spent".
var errClaudeResetHistoryUnavailable = errors.New("the reset spend ledger cannot be read")

// claudeResetHistory is the last spend of every seat, read at most once per
// listing request, and only once an entry asks for it.
type claudeResetHistory struct {
	h    *Handler
	once sync.Once
	last map[string]claudeResetSpendRecord
	err  error
}

// claudeResetHistory returns a listing request's snapshot of the ledger; it
// reads nothing until a seat is asked for.
func (h *Handler) claudeResetHistory() *claudeResetHistory {
	return &claudeResetHistory{h: h}
}

// seat is the seat's last spend; nil with no error means no reset was ever
// spent through this hub.
func (history *claudeResetHistory) seat(authIndex string) (*claudeResetSpendRecord, error) {
	history.once.Do(func() { history.read() })
	if history.err != nil {
		return nil, history.err
	}
	record, ok := history.last[authIndex]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

// read takes the snapshot. A listing opens a journal only in a directory
// that exists: none could hold a ledger otherwise.
func (history *claudeResetHistory) read() {
	if history.h == nil {
		history.err = errClaudeResetHistoryUnavailable
		return
	}
	journal, errJournal := history.h.claudeResetSpends.journal(history.h.claudeResetLedgerDir())
	if errJournal != nil {
		history.err = errClaudeResetHistoryUnavailable
		return
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if !journal.opened && journal.refused == nil {
		if _, errStat := os.Stat(journal.dir); os.IsNotExist(errStat) {
			history.last = map[string]claudeResetSpendRecord{}
			return
		}
	}
	if refusal := journal.verifyLocked(); refusal != nil {
		history.err = errClaudeResetHistoryUnavailable
		return
	}
	history.last = make(map[string]claudeResetSpendRecord, len(journal.last))
	for seat, record := range journal.last {
		history.last[seat] = record
	}
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

// claudeResetStart is begin's verdict: an answer to give in place of a spend
// (status non-zero), or the go-ahead with the unknown claim a retry continues.
type claudeResetStart struct {
	status int
	answer gin.H
	retry  *claudeResetUnsettledClaim
}

func claudeResetRefuse(status int, message string) claudeResetStart {
	return claudeResetStart{status: status, answer: gin.H{"error": message}}
}

// begin reserves the seat and the operation. A key whose outcome the ledger
// holds is answered from it and reserves nothing, even while its spend is
// still reading the balance. A claim with an unknown outcome holds the seat
// for its retry window: only a retry naming its request id may proceed, and
// only when the claim's answer is recorded.
func (j *claudeResetJournal) begin(operation string, request claudeResetSpendRequest, kind string, now time.Time) claudeResetStart {
	j.mu.Lock()
	defer j.mu.Unlock()
	if refusal := j.verifyLocked(); refusal != nil {
		return claudeResetStart{status: http.StatusServiceUnavailable, answer: refusal.answer()}
	}
	recorded, known := j.operations[operation]
	if j.keysInFlight[operation] && (!known || recorded.Phase == claudeResetPhaseSent) {
		return claudeResetRefuse(http.StatusConflict, "a spend with this Idempotency-Key is in flight and its outcome is not recorded yet: deliver it again once it has answered")
	}
	if known {
		if !claudeResetSameOperation(recorded, request, kind) {
			return claudeResetRefuse(http.StatusConflict, "this Idempotency-Key was used for another spend; nothing spent")
		}
		status, answer := j.replayLocked(recorded)
		return claudeResetStart{status: status, answer: answer}
	}
	seat := request.authIndex
	if j.busy[seat] {
		return claudeResetRefuse(http.StatusConflict, "a spend is already in flight for this seat; nothing spent")
	}
	open := j.openLocked(seat, now)
	switch {
	case request.retryRequestID == "" && open != nil:
		return claudeResetRefuse(http.StatusConflict, "an earlier claim on this seat has an unknown outcome: retry it with its retry_request_id within ten minutes of sending, or check the account's usage page; nothing spent")
	case request.retryRequestID != "" && (open == nil || open.requestID != request.retryRequestID):
		return claudeResetRefuse(http.StatusConflict, "no claim with an unknown outcome has this request id inside its ten-minute retry window; nothing spent")
	case open != nil && j.claims[open.requestID].Phase != claudeResetPhaseAnswered:
		return claudeResetRefuse(http.StatusConflict, "the claim was sent and its answer is not in the spend ledger, so the provider may have answered it: no retry is accepted; check the account's usage page; nothing spent")
	case open != nil && open.sentAt.Sub(now) > claudeResetClockTolerance:
		return claudeResetRefuse(http.StatusConflict, "the hub's clock reads earlier than the claim's record, so its retry window cannot be judged: no retry is accepted until the clock passes it; nothing spent")
	}
	j.busy[seat], j.keysInFlight[operation] = true, true
	if open == nil {
		return claudeResetStart{}
	}
	retry := *open
	return claudeResetStart{retry: &retry}
}

// claudeResetNotSentStatus is the status a claim not sent was answered with.
func claudeResetNotSentStatus(reason string) int {
	switch reason {
	case claudeResetAbortTransport:
		return http.StatusBadGateway
	case claudeResetAbortLedgerRefused:
		return http.StatusServiceUnavailable
	}
	return http.StatusConflict
}

// dispatch judges a claim right before it leaves: under the journal's lock it
// checks the ledger again, then runs check on a fresh clock, so no wait for
// the ledger falls between the judgment and the send. A journal that has
// started refusing, or a ledger that is no longer the file it wrote, sends
// nothing. It returns the reason code of a claim not to send.
func (j *claudeResetJournal) dispatch(check func() string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if refusal := j.verifyLocked(); refusal != nil {
		return claudeResetAbortLedgerRefused
	}
	return check()
}

// replayLocked answers a key delivered again with what the ledger holds: the
// recorded outcome with the status and retry deadline its first answer
// carried, and the newest record of the same claim when a retry followed it.
func (j *claudeResetJournal) replayLocked(record claudeResetSpendRecord) (int, gin.H) {
	answer := gin.H{
		"replayed": true, "auth_index": record.AuthIndex, "kind": record.Kind, "grant_id": record.GrantID, "request_id": record.RequestID,
		"retry": record.Retry, "result": record.Result, "at": record.At, "resets_left_before": record.ResetsLeftBefore, "resets_left_after": record.ResetsLeftAfter,
	}
	if record.Reason != "" {
		answer["reason"] = record.Reason
	}
	status := http.StatusOK
	switch {
	case record.Phase == claudeResetPhaseSent:
		answer["recorded"] = "the claim was sent and no answer was recorded: no retry of it is accepted"
		return status, answer
	case record.Result == claudeResetResultNotSent:
		// The first answer's status, and still the newest record of the
		// claim, which a later retry may have settled.
		answer["error"] = "the claim was not sent; nothing spent"
		status = claudeResetNotSentStatus(record.Reason)
	}
	settled := claudeResetSettles(record)
	if latest := j.claims[record.RequestID]; latest.Operation != "" && latest.Operation != record.Operation {
		answer["latest"] = gin.H{"retry": latest.Retry, "phase": latest.Phase, "result": latest.Result, "at": latest.At}
		settled = settled || (latest.Phase == claudeResetPhaseAnswered && claudeResetSettles(latest))
	}
	if first, ok := j.firstSends[record.RequestID]; ok && !settled && record.Result != claudeResetResultNotSent {
		answer["retry_until"] = first.At.Add(claudeResetRetryWindow)
	}
	return status, answer
}

// reserveAccount holds the account across seats, for a first claim and for a
// retry alike: two credential files of one account must not claim while
// either has a claim in flight or unknown. It is taken before the grants read
// that authorizes the claim, and lasts until the request finishes.
func (j *claudeResetJournal) reserveAccount(authIndex, account string, now time.Time) (int, gin.H) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if refusal := j.verifyLocked(); refusal != nil {
		return http.StatusServiceUnavailable, refusal.answer()
	}
	if holder, ok := j.accountInFlight[account]; ok && holder != authIndex {
		return http.StatusConflict, gin.H{"error": "a spend is in flight on another seat of this account; nothing spent"}
	}
	for seat := range j.unsettled {
		if open := j.openLocked(seat, now); seat != authIndex && open != nil && open.account == account {
			return http.StatusConflict, gin.H{"error": "another seat of this account has a claim with an unknown outcome; nothing spent"}
		}
	}
	j.accountInFlight[account] = authIndex
	return 0, nil
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

// markSent makes the sent line durable, which holds the seat from its stamp.
// When the line cannot be written the claim must not be sent.
func (j *claudeResetJournal) markSent(record *claudeResetSpendRecord, clock func() time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(record, clock)
}

// answer records a known outcome at once. When the line cannot be written
// the journal refuses every spend from then on, the hold stays, and after a
// restart the claim, whose answer is not recorded, cannot be retried.
func (j *claudeResetJournal) answer(record *claudeResetSpendRecord, clock func() time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	log.WithFields(log.Fields{
		"auth_index": record.AuthIndex, "kind": record.Kind, "grant_id": record.GrantID, "request_id": record.RequestID,
		"retry": record.Retry, "result": record.Result, "reason": record.Reason, "http_status": record.HTTPStatus,
		"resets_left_before": record.ResetsLeftBefore,
	}).Info("Claude banked reset spend")
	if errWrite := j.appendLocked(record, clock); errWrite != nil {
		log.WithError(errWrite).Error("Claude reset spend outcome not written; the seat stays held")
		return errWrite
	}
	return nil
}

// recordBalance writes what is left of each kind after an answered claim.
func (j *claudeResetJournal) recordBalance(record *claudeResetSpendRecord, clock func() time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	log.WithFields(log.Fields{
		"auth_index": record.AuthIndex, "request_id": record.RequestID, "resets_left_after": record.ResetsLeftAfter,
	}).Info("Claude banked reset spend balance")
	if errAppend := j.appendLocked(record, clock); errAppend != nil {
		log.WithError(errAppend).Error("Claude reset spend balance not written")
		return errAppend
	}
	return nil
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
	body, errRead := client.FetchLimitResetStatus(ctx, token, userAgent)
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
// durable, on a fresh clock, first claim and retry alike: the registry must
// still hold the same enabled OAuth credential, the grant must not have
// expired, the claim's retry window (from its first send's stamp) must not
// have passed, and the sent line must have become durable promptly. It
// returns the reason code of a claim not to send.
func (h *Handler) claudeResetDispatchAbort(auth *coreauth.Auth, grant coreauth.ClaudeResetGrant, sentAt, firstSentAt, at time.Time) string {
	switch {
	case !h.authManager.ClaudeResetCredentialCurrent(auth):
		return claudeResetAbortCredential
	case !coreauth.ClaudeResetGrantSpendable(grant, at):
		return claudeResetAbortGrantExpired
	case at.Sub(firstSentAt) >= claudeResetRetryWindow:
		return claudeResetAbortRetryWindow
	case at.Sub(sentAt) >= claudeResetDispatchLimit:
		return claudeResetAbortLedgerStall
	case sentAt.Sub(at) > claudeResetClockTolerance || firstSentAt.Sub(at) > claudeResetClockTolerance:
		return claudeResetAbortClockBehind
	}
	return ""
}

// SpendClaudeLimitReset spends one banked reset of the kind the caller names,
// on one Claude seat. A request that names no kind, carries no single
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
	keys := c.Request.Header.Values(claudeResetIdempotencyHeader)
	if len(keys) != 1 || !claudeResetIdempotencyKeyPattern.MatchString(keys[0]) {
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
	c.Header("Cache-Control", "no-store")
	operation := claudeResetOperation(keys[0])
	start := journal.begin(operation, request, kind, ledger.clock())
	if start.status != 0 {
		c.JSON(start.status, start.answer)
		return
	}
	retry := start.retry
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
	if heldStatus, held := journal.reserveAccount(request.authIndex, account, ledger.clock()); heldStatus != 0 {
		c.JSON(heldStatus, held)
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
		Phase: claudeResetPhaseSent, Operation: operation, Account: account, AuthIndex: request.authIndex, Kind: kind, GrantID: grant.ID,
		NamedGrant: request.grantID, RequestID: requestID, Retry: retry != nil, Result: claudeauth.LimitResetResultUnknown, ResetsLeftBefore: claudeResetsLeftByKind(kinds),
	}
	if errLedger := journal.markSent(&record, ledger.clock); errLedger != nil {
		log.WithError(errLedger).Error("Claude reset spend ledger did not take the sent line; claim not sent")
		answer := gin.H{"error": "the spend ledger did not take the claim's record (" + errLedger.Error() + "); nothing spent"}
		var ledgerRefusal *claudeResetLedgerRefusal
		if errors.As(errLedger, &ledgerRefusal) {
			answer = ledgerRefusal.answer()
		}
		c.JSON(http.StatusServiceUnavailable, answer)
		return
	}
	// The hold and the retry window run from the sent line's stamp; a retry's
	// window runs from its first send's.
	sentAt, firstSentAt := record.At, record.At
	if retry != nil {
		firstSentAt = retry.sentAt
	}
	notSent := func(reason, message string) {
		record.Phase, record.Result, record.Reason = claudeResetPhaseAnswered, claudeResetResultNotSent, reason
		answer := gin.H{"error": message + "; nothing spent", "reason": reason, "request_id": requestID}
		if errAnswer := journal.answer(&record, ledger.clock); errAnswer != nil {
			answer["ledger_error"] = claudeResetLedgerErrorText
		}
		c.JSON(claudeResetNotSentStatus(reason), answer)
	}
	abort := journal.dispatch(func() string {
		return h.claudeResetDispatchAbort(auth, grant, sentAt, firstSentAt, ledger.clock())
	})
	switch abort {
	case "":
	case claudeResetAbortLedgerRefused:
		notSent(abort, "the spend ledger stopped being trusted before the claim left, so it was not sent")
		return
	default:
		notSent(abort, "the claim was judged again after its record was written and not sent")
		return
	}
	claim := resetclaim.Claim{OrganizationUUID: organization, GrantID: grant.ID, RequestID: requestID}
	outcome, errClaim := sendClaudeResetClaim(ctx, client, token, userAgent, claim)
	if errClaim != nil {
		notSent(claudeResetAbortTransport, "claim not sent")
		return
	}

	// A known outcome is on disk before it is answered, and apart from the
	// balance read.
	record.Phase, record.Result, record.Reason, record.HTTPStatus = claudeResetPhaseAnswered, outcome.Result, outcome.Reason, outcome.HTTPStatus
	errAnswer := journal.answer(&record, ledger.clock)
	answer := gin.H{
		"auth_index": request.authIndex, "kind": kind, "grant_id": grant.ID, "request_id": requestID, "retry": retry != nil,
		"result": outcome.Result, "provider": outcome, "kinds_before": kinds, "kinds_after": nil,
	}
	switch {
	case errAnswer != nil:
		answer["ledger_error"] = claudeResetLedgerErrorText
	case !claudeResetSettles(record):
		answer["retry_until"] = firstSentAt.Add(claudeResetRetryWindow)
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
	// The reset cache shows the claim's seat, and every other seat listing the
	// grant when the claim spent or may have, as awaiting a fresh read from
	// the moment the outcome is known; the after-read then replaces it.
	spent := outcome.Result == claudeauth.LimitResetResultReset || outcome.Result == claudeauth.LimitResetResultAlreadyUsed || outcome.Result == claudeauth.LimitResetResultUnknown
	h.authManager.PublishClaudeResetSpendRead(auth, grant.ID, spent, time.Now(), nil, errClaudeResetAfterReadPending)
	afterStarted := time.Now()
	after, afterBody, _, errAfter := readClaudeResetStatus(ctx, client, token, userAgent)
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
			Phase: claudeResetPhaseBalance, Operation: operation, AuthIndex: request.authIndex, RequestID: requestID,
			ResetsLeftAfter: claudeResetsLeftByKind(kindsAfter),
		}
		if errBalance := journal.recordBalance(&balance, ledger.clock); errBalance != nil {
			answer["balance_error"] = claudeResetBalanceErrorText
		}
	}
	c.JSON(http.StatusOK, answer)
}
