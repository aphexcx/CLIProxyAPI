package management

import (
	"bufio"
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
	// claudeResetRetryWindow bounds the explicit retry of a claim whose outcome
	// is unknown; the hub never retries on its own.
	claudeResetRetryWindow = 10 * time.Minute

	// A spend writes two ledger lines: "sent", synced to disk before the claim
	// leaves (so a restart keeps the seat held), and "answered", the record of
	// the outcome with what is left of each kind before and after.
	claudeResetPhaseSent     = "sent"
	claudeResetPhaseAnswered = "answered"
	// claudeResetPhaseTornClosed follows a torn line (a write cut short, which
	// no claim followed) that a later append closed; only such a line is skipped.
	claudeResetPhaseTornClosed = "torn_line_closed"
)

var claudeResetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

type claudeResetSpendRequest struct {
	AuthIndex      string  `json:"auth_index"`
	Kind           *string `json:"kind"`
	GrantID        string  `json:"grant_id"`
	RetryRequestID string  `json:"retry_request_id"`
}

// claudeResetSpendRecord is one ledger line. It holds no token, email or
// organization: Account is an opaque digest that binds a hold to its account
// across restarts.
type claudeResetSpendRecord struct {
	Phase            string         `json:"phase"`
	Account          string         `json:"account,omitempty"`
	At               time.Time      `json:"at"`
	AuthIndex        string         `json:"auth_index"`
	Kind             string         `json:"kind"`
	GrantID          string         `json:"grant_id"`
	RequestID        string         `json:"request_id"`
	Retry            bool           `json:"retry"`
	Result           string         `json:"result"`
	Reason           string         `json:"reason,omitempty"`
	HTTPStatus       int            `json:"http_status,omitempty"`
	ResetsLeftBefore map[string]int `json:"resets_left_before"`
	ResetsLeftAfter  map[string]int `json:"resets_left_after"`
}

type claudeResetUnsettledClaim struct {
	kind, grantID, requestID, account string
	sentAt                            time.Time
}

// claudeResetAccount is the opaque account binding the ledger keeps in place
// of the organization id.
func claudeResetAccount(organization string) string {
	sum := sha256.Sum256([]byte("cp-g33 claude reset account\x00" + strings.ToLower(strings.TrimSpace(organization))))
	return hex.EncodeToString(sum[:12])
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

// claudeResetSpendLedger serializes spends per seat and per account, and
// remembers claims whose outcome is unknown. Holds are per hub process. Its
// zero value is ready to use.
type claudeResetSpendLedger struct {
	mu              sync.Mutex
	now             func() time.Time
	loaded          bool
	busy            map[string]bool
	accountInFlight map[string]string
	unsettled       map[string]*claudeResetUnsettledClaim
	last            map[string]claudeResetSpendRecord
}

func (l *claudeResetSpendLedger) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func claudeResetSettles(record claudeResetSpendRecord) bool {
	if record.Retry {
		// A refusal on a retry cannot prove the first send did not spend.
		return record.Result == claudeauth.LimitResetResultReset || record.Result == claudeauth.LimitResetResultAlreadyUsed
	}
	return record.Result != claudeauth.LimitResetResultUnknown
}

func claudeResetRecordValid(record claudeResetSpendRecord) bool {
	return (record.Phase == claudeResetPhaseSent || record.Phase == claudeResetPhaseAnswered) && record.AuthIndex != "" && record.RequestID != ""
}

// applyLocked folds one ledger line into the last spend and the holds.
func (l *claudeResetSpendLedger) applyLocked(record claudeResetSpendRecord) {
	l.last[record.AuthIndex] = record
	if record.Phase == claudeResetPhaseSent {
		if !record.Retry {
			l.unsettled[record.AuthIndex] = &claudeResetUnsettledClaim{kind: record.Kind, grantID: record.GrantID, requestID: record.RequestID, account: record.Account, sentAt: record.At}
		}
		return
	}
	if open := l.unsettled[record.AuthIndex]; open != nil && open.requestID == record.RequestID && claudeResetSettles(record) {
		delete(l.unsettled, record.AuthIndex)
	}
}

// loadLocked restores the last spend of each seat, and every claim still
// without a settling answer, from the ledger file. It fails closed: a ledger
// that cannot be opened or read, or a line that is not a valid record, is an
// error and nothing is cached, so every spend is refused until the ledger is
// readable. Only a torn line is skipped: the last line cut short, or one a
// later append closed with a torn_line_closed marker. No claim follows a torn
// line, because a claim leaves only after its sent line is synced.
func (l *claudeResetSpendLedger) loadLocked(dir string) error {
	if l.loaded {
		return nil
	}
	l.busy = make(map[string]bool)
	l.accountInFlight = make(map[string]string)
	l.unsettled = make(map[string]*claudeResetUnsettledClaim)
	l.last = make(map[string]claudeResetSpendRecord)
	if dir == "" {
		l.loaded = true
		return nil
	}
	file, errOpen := os.Open(filepath.Join(dir, claudeResetSpendLedgerName))
	if errOpen != nil {
		if os.IsNotExist(errOpen) {
			l.loaded = true
			return nil
		}
		return errOpen
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
			return errors.New("the reset spend ledger holds a line that is not a valid record")
		case parsed && claudeResetRecordValid(record):
			l.applyLocked(record)
		case complete:
			invalidPending = true
		case len(line) > 0:
			log.Warn("Claude reset spend ledger ends in a torn line; it is skipped")
		}
		if !complete {
			if invalidPending {
				return errors.New("the reset spend ledger holds a line that is not a valid record")
			}
			l.loaded = true
			return nil
		}
	}
}

// errClaudeResetHistoryUnavailable is what a read shows in place of a seat's
// last spend while the ledger cannot be read: unknown, not "never spent".
var errClaudeResetHistoryUnavailable = errors.New("the reset spend ledger cannot be read")

// lastSpend is the seat's last ledger line; nil with no error means no reset
// was ever spent through this hub.
func (l *claudeResetSpendLedger) lastSpend(dir, authIndex string) (*claudeResetSpendRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if errLoad := l.loadLocked(dir); errLoad != nil {
		return nil, errClaudeResetHistoryUnavailable
	}
	record, ok := l.last[authIndex]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

// openLocked returns the seat's unknown claim inside its retry window.
func (l *claudeResetSpendLedger) openLocked(authIndex string, now time.Time) *claudeResetUnsettledClaim {
	open := l.unsettled[authIndex]
	if open != nil && now.Sub(open.sentAt) >= claudeResetRetryWindow {
		delete(l.unsettled, authIndex)
		return nil
	}
	return open
}

// begin reserves the seat. A claim with an unknown outcome holds the seat for
// its retry window: only a retry naming its request id may proceed, and that
// retry is returned.
func (l *claudeResetSpendLedger) begin(dir, authIndex, retryRequestID string, now time.Time) (*claudeResetUnsettledClaim, int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if errLoad := l.loadLocked(dir); errLoad != nil {
		log.WithError(errLoad).Error("Claude reset spend ledger unreadable; spends refused")
		return nil, http.StatusServiceUnavailable, "the spend ledger cannot be read, so earlier claims cannot be ruled out"
	}
	if l.busy[authIndex] {
		return nil, http.StatusConflict, "a spend is already in flight for this seat"
	}
	open := l.openLocked(authIndex, now)
	switch {
	case retryRequestID == "" && open != nil:
		return nil, http.StatusConflict, "an earlier claim on this seat has an unknown outcome: retry it with its retry_request_id within ten minutes of sending, or check the account's usage page"
	case retryRequestID != "" && (open == nil || open.requestID != retryRequestID):
		return nil, http.StatusConflict, "no claim with an unknown outcome has this request id inside its ten-minute retry window"
	}
	l.busy[authIndex] = true
	if open == nil {
		return nil, 0, ""
	}
	retry := *open
	return &retry, 0, ""
}

// reserveAccount holds the account across seats, for a first claim and for a
// retry alike: two credential files of one account must not claim while
// either has a claim in flight or unknown. The reservation lasts until the
// request finishes, past the retry window if the claim is still in flight.
func (l *claudeResetSpendLedger) reserveAccount(authIndex, account string, now time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if holder, ok := l.accountInFlight[account]; ok && holder != authIndex {
		return "a spend is in flight on another seat of this account"
	}
	for seat := range l.unsettled {
		if open := l.openLocked(seat, now); seat != authIndex && open != nil && open.account == account {
			return "another seat of this account has a claim with an unknown outcome"
		}
	}
	l.accountInFlight[account] = authIndex
	return ""
}

func (l *claudeResetSpendLedger) finish(authIndex string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, authIndex)
	for account, holder := range l.accountInFlight {
		if holder == authIndex {
			delete(l.accountInFlight, account)
		}
	}
}

// markSent syncs the sent line to disk, then holds the seat. When the line
// cannot be written the claim must not be sent.
func (l *claudeResetSpendLedger) markSent(dir string, record claudeResetSpendRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if errAppend := appendClaudeResetLine(dir, record); errAppend != nil {
		return errAppend
	}
	l.applyLocked(record)
	return nil
}

// forget drops the hold of a claim that was certainly not sent.
func (l *claudeResetSpendLedger) forget(authIndex, requestID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if open := l.unsettled[authIndex]; open != nil && open.requestID == requestID {
		delete(l.unsettled, authIndex)
	}
}

// answer records the outcome in the ledger and the hub log. A ledger failure
// is logged; the claim has already been answered.
func (l *claudeResetSpendLedger) answer(dir string, record claudeResetSpendRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyLocked(record)
	log.WithFields(log.Fields{
		"auth_index": record.AuthIndex, "kind": record.Kind, "grant_id": record.GrantID, "request_id": record.RequestID,
		"retry": record.Retry, "result": record.Result, "http_status": record.HTTPStatus,
		"resets_left_before": record.ResetsLeftBefore, "resets_left_after": record.ResetsLeftAfter,
	}).Info("Claude banked reset spend")
	if errAppend := appendClaudeResetLine(dir, record); errAppend != nil {
		log.WithError(errAppend).Error("Claude reset spend record not written")
	}
}

func appendClaudeResetLine(dir string, record claudeResetSpendRecord) error {
	if dir == "" {
		return errors.New("no log directory for the reset spend ledger")
	}
	line, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	file, errOpen := os.OpenFile(filepath.Join(dir, claudeResetSpendLedgerName), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if errOpen != nil {
		return errOpen
	}
	// A torn last line (a write cut short) is closed first, so this record
	// starts on its own line and is never read as part of the torn one.
	info, errWrite := file.Stat()
	if errWrite == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, errWrite = file.ReadAt(last, info.Size()-1); errWrite == nil && last[0] != '\n' {
			marker, _ := json.Marshal(claudeResetSpendRecord{Phase: claudeResetPhaseTornClosed, At: record.At})
			line = append(append(append([]byte{'\n'}, marker...), '\n'), line...)
		}
	}
	if errWrite == nil {
		_, errWrite = file.Write(append(line, '\n'))
	}
	if errWrite == nil {
		errWrite = file.Sync()
	}
	if errClose := file.Close(); errWrite == nil {
		errWrite = errClose
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

func readClaudeResetStatus(ctx context.Context, client *claudeauth.ClaudeAuth, token, userAgent string) (*coreauth.ClaudeLimitResets, int, error) {
	body, errRead := client.FetchLimitResetSpendStatus(ctx, token, userAgent)
	if errRead != nil {
		status := 0
		var statusError interface{ StatusCode() int }
		if errors.As(errRead, &statusError) {
			status = statusError.StatusCode()
		}
		return nil, status, errRead
	}
	parsed, errParse := coreauth.ParseClaudeLimitResets(body)
	return parsed, 0, errParse
}

// sendClaudeResetClaim is the handler's only step that spends. It is reached
// only from SpendClaudeLimitReset (TestBankedResetClaimHasOneCallerChain).
func sendClaudeResetClaim(ctx context.Context, client *claudeauth.ClaudeAuth, token, userAgent string, claim resetclaim.Claim) (claudeauth.LimitResetClaimResult, error) {
	return client.ClaimLimitReset(ctx, token, userAgent, claim)
}

// SpendClaudeLimitReset spends one banked reset of the kind the caller names,
// on one Claude seat. A request that names no kind, or a kind the seat's fresh
// grants read does not list, is refused and spends nothing. The grants are
// re-read fresh before every claim, never from the quota cache. Nothing in the
// hub calls this but the management route; it never retries on its own.
func (h *Handler) SpendClaudeLimitReset(c *gin.Context) {
	var body claudeResetSpendRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&body); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: send auth_index and kind", "kinds_allowed": coreauth.ClaudeResetKindNames()})
		return
	}
	if errTrailing := decoder.Decode(&struct{}{}); !errors.Is(errTrailing, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: one JSON object only"})
		return
	}
	kind := ""
	if body.Kind != nil {
		kind = strings.ToLower(strings.TrimSpace(*body.Kind))
	}
	if kind == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind is required: name the kind of reset to spend", "kinds_allowed": coreauth.ClaudeResetKindNames()})
		return
	}
	if !coreauth.ValidClaudeResetKind(kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown reset kind", "kinds_allowed": coreauth.ClaudeResetKindNames()})
		return
	}
	grantID, retryRequestID := strings.TrimSpace(body.GrantID), strings.TrimSpace(body.RetryRequestID)
	if grantID != "" && !claudeResetGrantIDPattern.MatchString(grantID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "malformed grant_id"})
		return
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		c.JSON(http.StatusNotFound, gin.H{"error": "Claude seat not found"})
		return
	}

	// One private copy of the configuration serves the whole spend.
	cfg := h.claudeResetConfig()
	ledger, dir := &h.claudeResetSpends, h.claudeResetLedgerDir()
	retry, heldStatus, held := ledger.begin(dir, authIndex, retryRequestID, ledger.clock())
	if held != "" {
		c.JSON(heldStatus, gin.H{"error": held + "; nothing spent"})
		return
	}
	defer ledger.finish(authIndex)
	if retry != nil {
		if kind != retry.kind || (grantID != "" && grantID != retry.grantID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a retry must name the kind and grant of the claim it retries", "kind": retry.kind, "grant_id": retry.grantID})
			return
		}
		grantID = retry.grantID
	}
	token := tokenValueForAuth(auth)
	if token == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "seat has no access token; nothing spent"})
		return
	}
	ctx, client := c.Request.Context(), h.claudeResetAuth(cfg, auth)
	userAgent := claudeauth.LimitResetUserAgent(helps.DefaultClaudeVersion(cfg))

	// The last grants read is this fresh one; the quota cache never decides a
	// spend. Grants are judged at the time the read returns.
	before, readStatus, errRead := readClaudeResetStatus(ctx, client, token, userAgent)
	if errRead != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "fresh grants read failed; nothing spent", "upstream_status": readStatus})
		return
	}
	now := ledger.clock()
	kinds := coreauth.SummarizeClaudeResetKinds(before.Grants, now)
	ofKind := make([]coreauth.ClaudeResetGrant, 0)
	for _, grant := range before.Grants {
		if grant.Kind == kind {
			ofKind = append(ofKind, grant)
		}
	}
	refuse := func(status int, message string) {
		c.JSON(status, gin.H{"error": message + "; nothing spent", "kind": kind, "kinds": kinds, "grants_of_kind": ofKind, "eligible": before.Eligible, "ineligible_reason": before.IneligibleReason, "cooldown_until": before.CooldownUntil})
	}
	if retry == nil {
		if _, listed := kinds[kind]; !listed {
			refuse(http.StatusBadRequest, "the seat's grants list no reset of this kind")
			return
		}
		if !before.Eligible {
			refuse(http.StatusConflict, "the seat is not eligible for a reset")
			return
		}
		if before.CooldownUntil != nil && before.CooldownUntil.After(now) {
			refuse(http.StatusConflict, "the seat's reset cooldown is active")
			return
		}
		if grantID != "" {
			var named *coreauth.ClaudeResetGrant
			for i := range before.Grants {
				if before.Grants[i].ID == grantID {
					named = &before.Grants[i]
				}
			}
			switch {
			case named == nil:
				refuse(http.StatusConflict, "the named grant is not in the seat's fresh grants read")
				return
			case named.Kind != kind:
				refuse(http.StatusBadRequest, "the named grant is of kind "+named.Kind+", not "+kind)
				return
			case !coreauth.ClaudeResetGrantSpendable(*named, now):
				refuse(http.StatusConflict, "the named grant cannot be spent now")
				return
			}
		} else {
			grant, ok := coreauth.SelectClaudeResetGrant(before.Grants, kind, now)
			if !ok {
				refuse(http.StatusConflict, "no grant of this kind can be spent now")
				return
			}
			grantID = grant.ID
		}
	} else {
		for _, grant := range before.Grants {
			if grant.ID == grantID && grant.Kind != kind {
				refuse(http.StatusConflict, "the retried grant changed kind")
				return
			}
		}
	}
	profile, errProfile := client.FetchLimitResetSpendProfile(ctx, token, userAgent)
	if errProfile != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "organization read failed; nothing spent"})
		return
	}
	organization := profile.Organization.UUID
	account := claudeResetAccount(organization)
	requestID, sentAt := uuid.NewString(), ledger.clock()
	if retry != nil {
		if retry.account != "" && retry.account != account {
			c.JSON(http.StatusConflict, gin.H{"error": "the seat's account changed since the unknown claim; nothing spent"})
			return
		}
		if sentAt.Sub(retry.sentAt) >= claudeResetRetryWindow {
			c.JSON(http.StatusConflict, gin.H{"error": "the ten-minute retry window has passed; nothing spent"})
			return
		}
		requestID = retry.requestID
	} else {
		// Judged again at the time of the claim: the profile read takes time.
		if before.CooldownUntil != nil && before.CooldownUntil.After(sentAt) {
			refuse(http.StatusConflict, "the seat's reset cooldown is active")
			return
		}
		for _, grant := range before.Grants {
			if grant.ID == grantID && !coreauth.ClaudeResetGrantSpendable(grant, sentAt) {
				refuse(http.StatusConflict, "the grant can no longer be spent")
				return
			}
		}
	}
	if heldAccount := ledger.reserveAccount(authIndex, account, sentAt); heldAccount != "" {
		c.JSON(http.StatusConflict, gin.H{"error": heldAccount + "; nothing spent"})
		return
	}
	record := claudeResetSpendRecord{
		Phase: claudeResetPhaseSent, Account: account, At: sentAt, AuthIndex: authIndex, Kind: kind, GrantID: grantID, RequestID: requestID,
		Retry: retry != nil, Result: claudeauth.LimitResetResultUnknown, ResetsLeftBefore: claudeResetsLeftByKind(kinds),
	}
	if errLedger := ledger.markSent(dir, record); errLedger != nil {
		log.WithError(errLedger).Error("Claude reset spend ledger not writable; claim not sent")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the spend ledger is not writable; nothing spent"})
		return
	}
	claim := resetclaim.Claim{OrganizationUUID: organization, GrantID: grantID, RequestID: requestID}
	outcome, errClaim := sendClaudeResetClaim(ctx, client, token, userAgent, claim)
	if errClaim != nil {
		if retry == nil {
			ledger.forget(authIndex, requestID)
		}
		record.Phase, record.Result, record.Reason = claudeResetPhaseAnswered, "not_sent", "the claim was not sent"
		ledger.answer(dir, record)
		c.JSON(http.StatusBadGateway, gin.H{"error": "claim not sent; nothing spent"})
		return
	}

	record.Phase, record.Result, record.Reason, record.HTTPStatus = claudeResetPhaseAnswered, outcome.Result, outcome.Reason, outcome.HTTPStatus
	settled := claudeResetSettles(record)
	answer := gin.H{
		"auth_index": authIndex, "kind": kind, "grant_id": grantID, "request_id": requestID, "retry": retry != nil,
		"result": outcome.Result, "provider": outcome, "kinds_before": kinds, "kinds_after": nil,
	}
	if !settled {
		firstSent := sentAt
		if retry != nil {
			firstSent = retry.sentAt
		}
		answer["retry_until"] = firstSent.Add(claudeResetRetryWindow)
	}
	after, _, errAfter := readClaudeResetStatus(ctx, client, token, userAgent)
	if errAfter == nil {
		kindsAfter := coreauth.SummarizeClaudeResetKinds(after.Grants, ledger.clock())
		record.ResetsLeftAfter = claudeResetsLeftByKind(kindsAfter)
		answer["kinds_after"] = kindsAfter
	} else {
		answer["after_read_error"] = "the grants read after the claim failed"
	}
	ledger.answer(dir, record)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, answer)
}
