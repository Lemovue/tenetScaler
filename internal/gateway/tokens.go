package gateway

import (
	"context"
	"time"

	"tenantscaler/internal/engine"
)

// tokenizeTimeout bounds one call of Config.TokenCounter.
const tokenizeTimeout = 2 * time.Second

// inputTokens counts the prompt's tokens with Config.TokenCounter on a
// ready endpoint (the replica's own tokenizer, decision Q17), rotating over
// the endpoints. Without a counter it counts whitespace words, the
// simulated engine's tokenizer; when the counter fails or no endpoint is
// ready it falls back to words and counts the fallback, so that reports can
// show how many prompts were not counted by the real tokenizer.
func (g *Gateway) inputTokens(ctx context.Context, prompt string) int {
	if g.cfg.TokenCounter == nil {
		return engine.CountTokens(prompt)
	}
	g.mu.Lock()
	eps := g.endpoints
	g.mu.Unlock()
	if len(eps) > 0 {
		ep := eps[int(g.tokRR.Add(1)%uint64(len(eps)))]
		tctx, cancel := context.WithTimeout(ctx, tokenizeTimeout)
		n, err := g.cfg.TokenCounter(tctx, ep, prompt)
		cancel()
		if err == nil && n > 0 {
			return n
		}
		if g.tokFallbacks.Add(1) == 1 {
			g.log.Warn("tokenizer unavailable: counting prompt words instead", "endpoint", ep, "err", err)
		}
		return engine.CountTokens(prompt)
	}
	g.tokFallbacks.Add(1)
	return engine.CountTokens(prompt)
}

// TokenizerFallbacks is how many prompts were counted in words because
// Config.TokenCounter was set but unavailable.
func (g *Gateway) TokenizerFallbacks() int64 { return g.tokFallbacks.Load() }
