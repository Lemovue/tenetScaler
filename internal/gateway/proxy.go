package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
)

const (
	// maxAttempts bounds the tries of one request on different endpoints;
	// only refusals that prove the replica did no work are retried.
	maxAttempts = 3
	// maxResponseBytes bounds a buffered (non-streamed or error) response.
	maxResponseBytes = 16 << 20
)

// errClientWrite marks a failure to write the response to the client.
var errClientWrite = errors.New("write to client")

// upstreamIdleTimeout is how long an idle replica connection is kept. It
// is below llama-server's 5 s keep-alive timeout, so the gateway rarely
// writes to a connection the replica is closing.
const upstreamIdleTimeout = 2 * time.Second

// newUpstreamClient returns the HTTP client for replicas. It has no overall
// timeout (streams last as long as generation does; the request context
// bounds them), ignores proxy environment variables (replicas are direct
// pod or loopback addresses) and does not ask for compression, so SSE
// chunks pass through as they are produced.
func newUpstreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     upstreamIdleTimeout,
		DisableCompression:  true,
	}}
}

// forward proxies a dispatched request to its endpoint, relays the response
// and settles the request: Complete in the scheduler, slot release and
// accounting.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, p *pending, ep string, body []byte, spec model.TenantSpec) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()

	x := &exchange{p: p, spec: spec, usage: -1}
	resp, err := g.send(ctx, p, ep, body)
	if err != nil {
		g.finish(p, 0)
		g.abort(w, r, x, fmt.Errorf("upstream %s: %w", p.endpoint, err), false)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		g.finish(p, 0)
		if resp.StatusCode >= 500 {
			msg := strings.TrimSpace(string(data[:min(len(data), 512)]))
			g.abort(w, r, x, fmt.Errorf("upstream %s answered %s: %s", p.endpoint, resp.Status, msg), false)
			return
		}
		// A 4xx means the replica refused this request itself (e.g. the
		// prompt exceeds its context): pass its answer through. It is the
		// request's own fault, so it is not an SLO outcome.
		g.metrics.failed.WithLabelValues(p.tenant, FailUpstream).Inc()
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}

	if isEventStream(resp.Header) {
		x.streamed = true
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		err = g.relayStream(w, resp.Body, x)
	} else {
		var data []byte
		if data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes)); err != nil {
			g.finish(p, 0)
			g.abort(w, r, x, fmt.Errorf("upstream %s: read response: %w", p.endpoint, err), false)
			return
		}
		countBody(data, x)
		x.firstToken = g.now()
		x.lastToken = x.firstToken
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(data); werr != nil {
			err = fmt.Errorf("%w: %w", errClientWrite, werr)
		}
	}
	g.finish(p, x.tokens())
	if err != nil {
		g.abort(w, r, x, err, true)
		return
	}
	g.recordCompletion(x)
}

// abort accounts a dispatched request that did not complete and, unless
// the response has started or the client is gone, answers it: 502 for an
// upstream failure, 503 during shutdown. An upstream failure is also an SLO
// miss (recordFailure).
func (g *Gateway) abort(w http.ResponseWriter, r *http.Request, x *exchange, err error, started bool) {
	p := x.p
	reason := FailUpstream
	switch {
	case errors.Is(err, errClientWrite) || r.Context().Err() != nil:
		reason = FailCanceled
	case g.ctx.Err() != nil:
		reason = FailShutdown
	}
	g.metrics.failed.WithLabelValues(p.tenant, reason).Inc()
	if reason == FailCanceled {
		g.log.Debug("client went away", "tenant", p.tenant, "id", p.req.ID)
		return
	}
	g.log.Warn("request failed", "tenant", p.tenant, "id", p.req.ID, "reason", reason, "err", err)
	if reason == FailUpstream {
		g.recordFailure(p.tenant, p.req.Arrival, x.firstToken)
	}
	if started {
		return // the status line is sent; the truncated stream is the signal
	}
	if reason == FailShutdown {
		writeError(w, http.StatusServiceUnavailable, apiError{Type: "unavailable", Message: errShutdown.Error(), Tenant: p.tenant})
		return
	}
	writeError(w, http.StatusBadGateway, apiError{Type: "upstream_error", Message: err.Error(), Tenant: p.tenant})
}

// send posts the body to ep. When the replica refuses the connection or
// answers 503 (shutting down or not loaded), nothing was generated, so the
// request moves to another free slot (up to maxAttempts tries).
func (g *Gateway) send(ctx context.Context, p *pending, ep string, body []byte) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := g.post(ctx, ep, body)
		if attempt == maxAttempts || ctx.Err() != nil || !refused(resp, err) {
			return resp, err
		}
		next, ok := g.reroute(p, ep)
		if !ok {
			return resp, err
		}
		cause := fmt.Sprint(err)
		if resp != nil {
			cause = resp.Status
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
			resp.Body.Close()
		}
		g.log.Warn("replica refused a request; retrying on another replica",
			"tenant", p.tenant, "id", p.req.ID, "failed", ep, "next", next, "cause", cause)
		ep = next
	}
}

// refused reports whether the replica provably did no work: the connection
// was refused (dial error) or it answered 503.
func refused(resp *http.Response, err error) bool {
	if err != nil {
		var op *net.OpError
		return errors.As(err, &op) && op.Op == "dial"
	}
	return resp.StatusCode == http.StatusServiceUnavailable
}

// post sends the unchanged body to {ep}/v1/completions.
func (g *Gateway) post(ctx context.Context, ep string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep+"/v1/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	// An empty idempotency key (not sent) lets the transport resend the
	// request on a new connection when a reused one turns out to be closed
	// by the replica before any response: the replica never ran it.
	req.Header["X-Idempotency-Key"] = nil
	if g.cfg.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.cfg.UpstreamAPIKey)
	}
	return g.client.Do(req)
}

// relayStream copies an SSE response line by line, flushing at every event
// boundary, and measures token chunks on the way.
func (g *Gateway) relayStream(w http.ResponseWriter, body io.Reader, x *exchange) error {
	rc := http.NewResponseController(w)
	flush := func() error {
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return fmt.Errorf("%w: %w", errClientWrite, err)
		}
		return nil
	}
	br := bufio.NewReaderSize(body, 16<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, err := w.Write(line); err != nil {
				return fmt.Errorf("%w: %w", errClientWrite, err)
			}
			if g.observeLine(line, x) {
				g.onTokens(x.p, x.chunks)
			}
			if len(bytes.TrimSpace(line)) == 0 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF {
			return flush()
		}
		if rerr != nil {
			return fmt.Errorf("upstream %s: read stream: %w", x.p.endpoint, rerr)
		}
	}
}

// completionChunk is the part of a completion response or SSE chunk the
// gateway reads.
type completionChunk struct {
	Choices []struct {
		Text string `json:"text"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// observeLine inspects one SSE line. It records usage when present and
// reports whether the line was a token chunk (non-empty choices[0].text),
// updating the first and last token times.
func (g *Gateway) observeLine(line []byte, x *exchange) bool {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' { // e.g. "[DONE]"
		return false
	}
	var c completionChunk
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	if c.Usage != nil {
		x.usage = c.Usage.CompletionTokens
	}
	if len(c.Choices) == 0 || c.Choices[0].Text == "" {
		return false
	}
	now := g.now()
	if x.chunks == 0 {
		x.firstToken = now
	}
	x.lastToken = now
	x.chunks++
	return true
}

// countBody reads the output tokens of a non-streamed response: usage when
// present, else the words of choices[0].text.
func countBody(data []byte, x *exchange) {
	var c completionChunk
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if c.Usage != nil {
		x.usage = c.Usage.CompletionTokens
		return
	}
	if len(c.Choices) > 0 && strings.TrimSpace(c.Choices[0].Text) != "" {
		x.chunks = engine.CountTokens(c.Choices[0].Text)
	}
}

// isEventStream reports whether the response is server-sent events.
func isEventStream(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// hopHeaders are not relayed from the replica to the client.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length",
}

// copyHeaders relays end-to-end response headers, such as Content-Type and
// X-TenantScaler-Simulated.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if !slices.Contains(hopHeaders, http.CanonicalHeaderKey(k)) {
			dst[k] = slices.Clone(vs)
		}
	}
}
