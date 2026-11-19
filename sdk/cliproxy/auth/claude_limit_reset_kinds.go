package auth

import (
	"sort"
	"time"
)

// The kinds of a banked Claude reset grant, and the one table that maps a
// grant to its kind. The provider's own client (Claude Code 2.1.328) calls a
// grant "fable" when gives_back_weekly is set, otherwise names it by its type
// (session, weekly or full). Where that client would derive an untyped grant's
// kind from the windows it clears, the hub does not guess: such a grant, and
// any type outside the three, is unknown and can never be spent through the hub.
const (
	ClaudeResetKindSession = "session"
	ClaudeResetKindWeekly  = "weekly"
	ClaudeResetKindFull    = "full"
	ClaudeResetKindFable   = "fable"
	ClaudeResetKindUnknown = "unknown"
)

// ClaudeResetGrantKind returns the grant's kind by the table above.
func ClaudeResetGrantKind(grant ClaudeResetGrant) string {
	switch grant.Type {
	case "", ClaudeResetKindSession, ClaudeResetKindWeekly, ClaudeResetKindFull:
	default:
		return ClaudeResetKindUnknown
	}
	if grant.GivesBackWeekly != nil && *grant.GivesBackWeekly {
		return ClaudeResetKindFable
	}
	if grant.Type == "" {
		return ClaudeResetKindUnknown
	}
	return grant.Type
}

// ValidClaudeResetKind reports whether an operator may name kind in a spend.
func ValidClaudeResetKind(kind string) bool {
	switch kind {
	case ClaudeResetKindSession, ClaudeResetKindWeekly, ClaudeResetKindFull, ClaudeResetKindFable:
		return true
	}
	return false
}

// ClaudeResetKindNames lists the kinds an operator may name, sorted.
func ClaudeResetKindNames() []string {
	return []string{ClaudeResetKindFable, ClaudeResetKindFull, ClaudeResetKindSession, ClaudeResetKindWeekly}
}

// ClaudeResetGrantSpendable reports whether a grant of a nameable kind can be
// claimed at now, by the grant's own fields. Seat-level eligibility and
// cooldown are the caller's to check.
func ClaudeResetGrantSpendable(grant ClaudeResetGrant, now time.Time) bool {
	if !ValidClaudeResetKind(ClaudeResetGrantKind(grant)) || grant.ResetsLeft <= 0 || !grant.UsableNow || grant.Paused {
		return false
	}
	if grant.StartsAt != nil && grant.StartsAt.After(now) {
		return false
	}
	return grant.EndsAt == nil || grant.EndsAt.After(now)
}

// SelectClaudeResetGrant returns the spendable grant of kind that expires
// first. A grant with no expiry sorts last; equal expiries sort by id.
func SelectClaudeResetGrant(grants []ClaudeResetGrant, kind string, now time.Time) (ClaudeResetGrant, bool) {
	candidates := make([]ClaudeResetGrant, 0, len(grants))
	for _, grant := range grants {
		if ClaudeResetGrantKind(grant) == kind && ClaudeResetGrantSpendable(grant, now) {
			candidates = append(candidates, grant)
		}
	}
	if len(candidates) == 0 {
		return ClaudeResetGrant{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.EndsAt == nil) != (b.EndsAt == nil) {
			return b.EndsAt == nil
		}
		if a.EndsAt != nil && !a.EndsAt.Equal(*b.EndsAt) {
			return a.EndsAt.Before(*b.EndsAt)
		}
		return a.ID < b.ID
	})
	return candidates[0], true
}

// ClaudeResetKindSummary is what remains of one kind on a seat.
type ClaudeResetKindSummary struct {
	// ResetsLeft counts resets on unexpired grants of the kind.
	ResetsLeft int `json:"resets_left"`
	// UsableResets counts resets on grants that can be claimed now.
	UsableResets int `json:"usable_resets"`
	// SoonestExpiry is the expiry of the grant a spend of the kind would use.
	SoonestExpiry *time.Time `json:"soonest_expiry"`
}

// SummarizeClaudeResetKinds lists every kind present in grants, including
// unknown, so an operator sees grants the hub will not spend.
func SummarizeClaudeResetKinds(grants []ClaudeResetGrant, now time.Time) map[string]ClaudeResetKindSummary {
	summaries := make(map[string]ClaudeResetKindSummary)
	for _, grant := range grants {
		kind := ClaudeResetGrantKind(grant)
		summary := summaries[kind]
		if grant.EndsAt == nil || grant.EndsAt.After(now) {
			summary.ResetsLeft += grant.ResetsLeft
		}
		if ClaudeResetGrantSpendable(grant, now) {
			summary.UsableResets += grant.ResetsLeft
		}
		summaries[kind] = summary
	}
	for kind, summary := range summaries {
		if grant, ok := SelectClaudeResetGrant(grants, kind, now); ok && grant.EndsAt != nil {
			expiry := *grant.EndsAt
			summary.SoonestExpiry = &expiry
			summaries[kind] = summary
		}
	}
	return summaries
}
