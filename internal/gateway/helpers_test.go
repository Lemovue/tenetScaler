package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
)

// fakeUpstream is an OpenAI-compatible test replica in the style of
// llama-server: one token of text per SSE chunk, then a final chunk with
// empty text, finish_reason "length" and usage, then [DONE]. Configure the
// fields before the first request.
type fakeUpstream struct {
	srv *httptest.Server
	// firstDelay is slept before the first token, tokenDelay between tokens.
	firstDelay, tokenDelay time.Duration
	// release, when non-nil, holds every request until it is closed.
	release chan struct{}
	// status, when non-zero, is answered instead of a completion.
	status int
	// noUsage drops the usage object from the final chunk.
	noUsage bool
	// script, when non-empty, is sent verbatim as the SSE body.
	script string
	// breakAfter, when positive, aborts the connection after that many
	// token chunks, as a crashing replica does.
	breakAfter int

	releaseOnce sync.Once
	mu          sync.Mutex
	active      int
	maxActive   int
	tenants     []string // first prompt word of each request, in arrival order
	auth        []string // Authorization header of each request
}

// newFakeUpstream starts a fake replica; configure may set its fields.
func newFakeUpstream(t *testing.T, configure func(*fakeUpstream)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	if configure != nil {
		configure(f)
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(func() {
		f.open()
		f.srv.Close()
	})
	return f
}

// open releases every held request (idempotent).
func (f *fakeUpstream) open() {
	if f.release != nil {
		f.releaseOnce.Do(func() { close(f.release) })
	}
}

func (f *fakeUpstream) URL() string { return f.srv.URL }

func (f *fakeUpstream) stats() (active, maxActive int, tenants []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, f.maxActive, append([]string(nil), f.tenants...)
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/completions" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Prompt    string `json:"prompt"`
		MaxTokens *int   `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.tenants = append(f.tenants, strings.Fields(req.Prompt + " ?")[0])
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if f.release != nil {
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
	}
	if f.status != 0 {
		http.Error(w, "fake replica failure", f.status)
		return
	}
	n := DefaultMaxTokens
	if req.MaxTokens != nil {
		n = *req.MaxTokens
	}
	usage := map[string]int{"prompt_tokens": 1, "completion_tokens": n, "total_tokens": n + 1}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"index": 0, "text": strings.Repeat("tok ", n), "finish_reason": "length"}},
			"usage":   usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Fake-Upstream", "yes")
	rc := http.NewResponseController(w)
	if f.script != "" {
		_, _ = io.WriteString(w, f.script)
		_ = rc.Flush()
		return
	}
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		_ = rc.Flush()
	}
	time.Sleep(f.firstDelay)
	for i := range n {
		if i > 0 {
			time.Sleep(f.tokenDelay)
		}
		if f.breakAfter > 0 && i == f.breakAfter {
			panic(http.ErrAbortHandler) // drops the connection mid-stream
		}
		send(map[string]any{"choices": []map[string]any{{"index": 0, "text": "tok", "finish_reason": nil}}})
	}
	final := map[string]any{"choices": []map[string]any{{"index": 0, "text": "", "finish_reason": "length"}}}
	if !f.noUsage {
		final["usage"] = usage
	}
	send(final)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// testClock is a manual clock for the gateway: it stands still until
// advanced.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: t0} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// testDiscovery is a Discovery whose endpoints a test can change.
type testDiscovery struct {
	mu  sync.Mutex
	eps []string
}

func (d *testDiscovery) Endpoints() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.eps...)
}

func (d *testDiscovery) set(eps ...string) {
	d.mu.Lock()
	d.eps = eps
	d.mu.Unlock()
}

// testTenants returns tenants with a 2 s TTFT / 100 ms TPOT SLO and one
// guaranteed replica each.
func testTenants(names ...string) []model.TenantSpec {
	out := make([]model.TenantSpec, len(names))
	for i, n := range names {
		out[i] = model.TenantSpec{
			Name: n, GuaranteedReplicas: 1, MaxReplicas: 2, Priority: 100,
			SLO: model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99},
		}
	}
	return out
}

// startGateway creates a gateway (tenants A and B and 2 slots unless set)
// behind an httptest server; both are shut down when the test ends.
func startGateway(t *testing.T, cfg Config) (*Gateway, string) {
	t.Helper()
	return startGatewayAt(t, cfg, time.Now)
}

// startGatewayAt is startGateway with the gateway's clock.
func startGatewayAt(t *testing.T, cfg Config, clock func() time.Time) (*Gateway, string) {
	t.Helper()
	if cfg.Tenants == nil {
		cfg.Tenants = testTenants("A", "B")
	}
	if cfg.SlotsPerReplica == 0 {
		cfg.SlotsPerReplica = 2
	}
	g, err := newGateway(cfg, clock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(func() {
		g.Close() // first: answers queued requests, so the server can drain
		srv.Close()
	})
	return g, srv.URL
}

// result is a completed client call.
type result struct {
	status int
	header http.Header
	body   string
	// tokens counts SSE chunks with non-empty text; done is true when the
	// stream ended with [DONE].
	tokens int
	done   bool
}

var testClient = &http.Client{Timeout: 30 * time.Second}

// completionBody builds a completions request body.
func completionBody(prompt string, maxTokens int, stream bool) string {
	b, _ := json.Marshal(map[string]any{"prompt": prompt, "max_tokens": maxTokens, "stream": stream})
	return string(b)
}

// call posts a completion for tenant with priority (both optional).
func call(ctx context.Context, t *testing.T, base, tenant, priority, body string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	if priority != "" {
		req.Header.Set(PriorityHeader, priority)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		return result{status: -1, body: err.Error()}
	}
	defer resp.Body.Close()
	res := result{status: resp.StatusCode, header: resp.Header}
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		sb.WriteString(line + "\n")
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			res.done = true
			continue
		}
		var c completionChunk
		if json.Unmarshal([]byte(data), &c) == nil && len(c.Choices) > 0 && c.Choices[0].Text != "" {
			res.tokens++
		}
	}
	res.body = sb.String()
	return res
}

// post is call with a background context.
func post(t *testing.T, base, tenant, priority, body string) result {
	t.Helper()
	return call(context.Background(), t, base, tenant, priority, body)
}

// slotsHeld returns the number of endpoints with a held dispatch slot.
func slotsHeld(g *Gateway) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.load)
}

// tenantState returns the scheduler state of one tenant from Stats.
func tenantState(t *testing.T, st Stats, tenant string) fairq.TenantState {
	t.Helper()
	for _, s := range st.Scheduler {
		if s.Tenant == tenant {
			return s
		}
	}
	t.Fatalf("no scheduler state for tenant %s in %+v", tenant, st.Scheduler)
	return fairq.TenantState{}
}

// waitFor polls cond every 5 ms until it holds or the timeout expires.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// scrape returns the gateway's /metrics text.
func scrape(t *testing.T, base string) string {
	t.Helper()
	resp, err := testClient.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// metricValue returns the value of one exact series, e.g.
// `tenantscaler_gateway_output_tokens_total{tenant="A"}`.
func metricValue(t *testing.T, base, series string) float64 {
	t.Helper()
	for line := range strings.Lines(scrape(t, base)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("series %s: %v", series, err)
			}
			return f
		}
	}
	t.Fatalf("series %s not found in /metrics", series)
	return 0
}

// getJSON decodes a GET response into v and returns the status.
func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := testClient.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

// errorBody decodes a JSON error response body.
func errorBody(t *testing.T, body string) apiError {
	t.Helper()
	var e struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("error body %q is not JSON: %v", body, err)
	}
	return e.Error
}
