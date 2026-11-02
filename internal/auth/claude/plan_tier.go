package claude

import "strings"

// PlanTier returns the subscription tier label ("Max 20x", "Team premium", "Pro", ...)
// for this profile, or "" when the profile carries no tier facts. The mapping is
// claude-swap's plan_tier.tier_label (observed on 18 seats, 2026-09-10): the
// organization type decides Enterprise and Team first because a Team seat's
// rate-limit tier reuses the Max vocabulary, and Max is decided before Pro because
// a Max account may also report has_claude_pro. Unrecognized values are returned
// raw so a new tier stays visible instead of being mislabeled.
func (p *OAuthProfile) PlanTier() string {
	if p == nil {
		return ""
	}
	rateLimitTier := knownTierText(p.Organization.RateLimitTier)
	organizationType := knownTierText(p.Organization.OrganizationType)
	hasMax, _ := p.Account.HasClaudeMax.(bool)
	hasPro, _ := p.Account.HasClaudePro.(bool)

	tier := strings.ToLower(rateLimitTier)
	switch orgType := strings.ToLower(organizationType); {
	case orgType == "claude_enterprise":
		return "Enterprise"
	case orgType == "claude_team":
		if tier == "default_raven" {
			return "Team standard"
		}
		if strings.Contains(tier, "max") {
			return "Team premium"
		}
		if rateLimitTier != "" {
			return rateLimitTier
		}
		return "Team"
	case orgType == "claude_max" || hasMax || strings.HasPrefix(tier, "default_claude_max"):
		if strings.HasSuffix(tier, "20x") {
			return "Max 20x"
		}
		if strings.HasSuffix(tier, "5x") {
			return "Max 5x"
		}
		if rateLimitTier != "" {
			return rateLimitTier
		}
		return "Max"
	case hasPro || orgType == "claude_pro" || tier == "default_claude_pro" || strings.HasPrefix(tier, "default_claude_pro_"):
		return "Pro"
	case rateLimitTier != "":
		return rateLimitTier
	default:
		return organizationType
	}
}

// knownTierText trims a profile string, treating non-strings and "unknown" as absent.
func knownTierText(value any) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if strings.EqualFold(text, "unknown") {
		return ""
	}
	return text
}
