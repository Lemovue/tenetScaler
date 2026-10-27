package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
)

// maxBodyBytes bounds a completion request body.
const maxBodyBytes = 4 << 20

// completionRequest is the part of an OpenAI completions body the gateway
// reads. The body itself is forwarded unchanged.
type completionRequest struct {
	Prompt    json.RawMessage `json:"prompt"`
	MaxTokens *int            `json:"max_tokens"`
}

// parseCompletion extracts the prompt (a string, or an array of strings
// that is joined) and max_tokens (DefaultMaxTokens when absent or null).
func parseCompletion(body []byte) (prompt string, maxTokens int, err error) {
	var c completionRequest
	if err := json.Unmarshal(body, &c); err != nil {
		return "", 0, fmt.Errorf("decode body: %w", err)
	}
	if len(c.Prompt) == 0 || string(c.Prompt) == "null" {
		return "", 0, errors.New("prompt is required")
	}
	if err := json.Unmarshal(c.Prompt, &prompt); err != nil {
		var parts []string
		if json.Unmarshal(c.Prompt, &parts) != nil {
			return "", 0, errors.New("prompt must be a string or an array of strings")
		}
		prompt = strings.Join(parts, " ")
	}
	maxTokens = DefaultMaxTokens
	if c.MaxTokens != nil {
		maxTokens = *c.MaxTokens
	}
	if maxTokens < 1 {
		return "", 0, fmt.Errorf("max_tokens must be >= 1, got %d", maxTokens)
	}
	return prompt, maxTokens, nil
}

// tenantOf returns the request's tenant from TenantHeader or its alias.
func tenantOf(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get(TenantHeader)); t != "" {
		return t
	}
	return strings.TrimSpace(r.Header.Get(TenantHeaderAlias))
}

// handleCompletions serves POST /v1/completions: validate, account the
// arrival, admit or shed, wait for a slot, then proxy to the replica.
func (g *Gateway) handleCompletions(w http.ResponseWriter, r *http.Request) {
	arrival := g.now()
	if g.ctx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, apiError{Type: "unavailable", Message: errShutdown.Error()})
		return
	}
	tenant, maxPrio, ok := g.requestTenant(w, r)
	if !ok {
		return
	}
	spec, ok := g.tenantSpec(tenant)
	if !ok {
		unknownTenant(w, tenant)
		return
	}
	prio, err := model.ParsePriority(r.Header.Get(PriorityHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Message: err.Error(), Tenant: tenant})
		return
	}
	prio = min(prio, maxPrio) // a header may only lower the key's priority
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		status := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, apiError{Type: "invalid_request_error", Message: "read body: " + err.Error(), Tenant: tenant})
		return
	}
	prompt, maxTokens, err := parseCompletion(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Message: err.Error(), Tenant: tenant})
		return
	}

	in := g.inputTokens(r.Context(), prompt)
	weighted := model.WeightedTokens(float64(in), float64(maxTokens))
	g.metrics.arrival(tenant, prio, weighted)
	g.obs.arrival(tenant, arrival, weighted)
	req := &fairq.Request{
		ID: g.nextID.Add(1), Tenant: tenant, Priority: prio,
		InputTokens: in, MaxOutputTokens: maxTokens, Arrival: arrival,
	}
	if ttft := spec.SLO.TTFT(); ttft > 0 {
		req.Deadline = arrival.Add(ttft)
	}
	p, v, err := g.admit(req)
	switch {
	case err != nil: // removed since the lookup above
		unknownTenant(w, tenant)
		return
	case !v.Admitted:
		g.shed(w, tenant, v, weighted)
		return
	}
	g.kick()

	ep, err := g.waitDispatch(r.Context(), p)
	switch {
	case errors.Is(err, errClientGone):
		g.metrics.failed.WithLabelValues(tenant, FailCanceled).Inc()
		return
	case errors.Is(err, errShutdown):
		g.metrics.failed.WithLabelValues(tenant, FailShutdown).Inc()
		writeError(w, http.StatusServiceUnavailable, apiError{Type: "unavailable", Message: err.Error(), Tenant: tenant})
		return
	case errors.Is(err, errTenantRemoved):
		g.metrics.failed.WithLabelValues(tenant, FailTenantRemoved).Inc()
		msg := fmt.Sprintf("tenant %s was removed while its request was queued", tenant)
		writeError(w, http.StatusServiceUnavailable, apiError{Type: "unavailable", Message: msg, Tenant: tenant})
		return
	case err != nil:
		g.metrics.failed.WithLabelValues(tenant, FailDispatchTimeout).Inc()
		g.recordFailure(tenant, arrival, time.Time{})
		g.log.Warn("request timed out in the gateway queue", "tenant", tenant, "timeout", g.cfg.DispatchTimeout)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, apiError{Type: "unavailable", Message: err.Error(), Tenant: tenant})
		return
	}
	g.forward(w, r, p, ep, body, spec)
}

// unknownTenant answers a request of a tenant the gateway does not know.
func unknownTenant(w http.ResponseWriter, tenant string) {
	msg := fmt.Sprintf("unknown tenant %q: set the %s header to a configured tenant", tenant, TenantHeader)
	writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Message: msg, Tenant: tenant})
}

// shed answers a rejected request: 429 with a JSON error and Retry-After
// in whole seconds (at least 1).
func (g *Gateway) shed(w http.ResponseWriter, tenant string, v fairq.Verdict, weighted float64) {
	g.metrics.shed.WithLabelValues(tenant, v.Reason).Inc()
	g.obs.shed(tenant, g.now(), weighted)
	secs := max(int(math.Ceil(v.RetryAfter.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, apiError{
		Type: "rate_limited", Reason: v.Reason, Tenant: tenant,
		Message: fmt.Sprintf("tenant %s: request shed (%s); retry after %d s", tenant, v.Reason, secs),
	})
}

// exchange is the measured outcome of one proxied request.
type exchange struct {
	p    *pending
	spec model.TenantSpec
	// streamed is true for an SSE response, where per-token timing is
	// observable.
	streamed bool
	// chunks counts SSE chunks with a non-empty choices[0].text; usage is
	// the replica's usage.completion_tokens, or -1 when it sent none.
	chunks, usage         int
	firstToken, lastToken time.Time
}

// tokens returns the output token count: the replica's own usage report
// when present (real llama-server and the simulated replica both send it),
// else the counted token chunks.
func (x *exchange) tokens() int {
	if x.usage >= 0 {
		return x.usage
	}
	return x.chunks
}

// recordCompletion accounts a completed request: TTFT from gateway arrival
// to the first token chunk (for a non-streamed response: to the full
// response, an upper bound), TPOT as the mean gap after the first token,
// and the SLO outcome by model.SLO.Met.
func (g *Gateway) recordCompletion(x *exchange) {
	end := g.now()
	first := x.firstToken
	if first.IsZero() {
		first = end
	}
	tokens := x.tokens()
	ttft := first.Sub(x.p.req.Arrival)
	var tpot time.Duration
	if x.streamed && tokens > 1 && !x.firstToken.IsZero() {
		tpot = x.lastToken.Sub(x.firstToken) / time.Duration(tokens-1)
	}
	met := x.spec.SLO.Met(ttft, tpot, tokens)
	g.metrics.complete(x.p.tenant, ttft, tpot, tokens, met, x.streamed)
	g.obs.completed(x.p.tenant, end, ttft, met)
	g.log.Debug("request completed", "tenant", x.p.tenant, "id", x.p.req.ID, "endpoint", x.p.endpoint,
		"tokens", tokens, "ttftMs", ms(ttft), "tpotMs", ms(tpot), "sloMet", met)
}

// recordFailure accounts an admitted request that failed for a reason on
// the serving side (no replica slot within the dispatch timeout, or an
// upstream failure) as a completed request that missed its SLO, so SLO
// attainment and p99 TTFT include the worst requests instead of dropping
// them. Its TTFT is the time to its first relayed token when there was one,
// else the time until it failed: a lower bound, since no token came.
// Upstream 4xx answers (the request's own fault), client cancellations,
// shutdown and tenant removal are not SLO outcomes.
func (g *Gateway) recordFailure(tenant string, arrival, firstToken time.Time) {
	end := g.now()
	ttft := end.Sub(arrival)
	if !firstToken.IsZero() {
		ttft = firstToken.Sub(arrival)
	}
	g.metrics.missed(tenant, ttft)
	g.obs.failed(tenant, end, ttft)
}
