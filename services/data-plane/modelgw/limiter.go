package modelgw

import (
	"local-agent-stack/pkg/a2a"
	"local-agent-stack/services/data-plane/config"
)

// TokenLimiter applies the token dimension of the dual-dimension limiter
// (contract §2.4 rule 3 / §0.3 RATE_LIMITED).
//
// Flow:
//  1. ReserveTokens(appId, estimate=max_tokens, rule) BEFORE the upstream call.
//     Over-limit -> RATE_LIMITED + Retry-After (the gateway returns 429).
//  2. After the response, SettleTokens(tokenID, actual=usage.total_tokens)
//     corrects the pre-deduction ("预扣 + 结算回填").
//
// It wraps the SAME a2a.MemoryLimiter instance used by RateLimitFilter, so
// request-count and token-count are two dimensions of one limiter.
type TokenLimiter struct {
	Limiter *a2a.MemoryLimiter
	Cfg     config.Provider
}

// NewTokenLimiter builds a TokenLimiter.
func NewTokenLimiter(l *a2a.MemoryLimiter, cfg config.Provider) *TokenLimiter {
	return &TokenLimiter{Limiter: l, Cfg: cfg}
}

// Reserve pre-deducts the estimated token amount. Returns (tokenID, decision).
func (t *TokenLimiter) Reserve(appID string, estimate int) (string, a2a.LimitDecision) {
	rule := t.Cfg.Current().RateRuleFor(appID)
	return t.Limiter.ReserveTokens(appID, estimate, rule)
}

// Settle corrects the reservation with the real usage.
func (t *TokenLimiter) Settle(tokenID string, actual int) {
	t.Limiter.SettleTokens(tokenID, actual)
}
