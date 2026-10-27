package replica

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func doPost(ctx context.Context, base, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func postCompletion(t *testing.T, ctx context.Context, base, body string) *http.Response {
	t.Helper()
	resp, err := doPost(ctx, base, body)
	if err != nil {
		t.Fatalf("POST /v1/completions: %v", err)
	}
	return resp
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}

// metricLine renders a sample line with only the model_name label, as
// /metrics writes it.
func metricLine(name string, value any) string {
	return fmt.Sprintf("%s{model_name=%q} %v\n", name, DefaultModel, value)
}

func TestStreamingSSE(t *testing.T) {
	_, srv := newReady(t, fastParams(2))
	resp := postCompletion(t, context.Background(), srv.URL,
		`{"model":"m1","prompt":"one two three","max_tokens":5,"stream":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get(SimulatedHeader); got != "true" {
		t.Fatalf("%s = %q, want true", SimulatedHeader, got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			t.Fatalf("non-data SSE line %q", line)
		}
		events = append(events, data)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 || events[5] != "[DONE]" {
		t.Fatalf("got %d events ending %q, want 5 chunks + [DONE]", len(events), events[len(events)-1])
	}
	for i, ev := range events[:5] {
		var c CompletionResponse
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if c.Object != "text_completion" || c.Model != "m1" || c.ID == "" || len(c.Choices) != 1 || c.Choices[0].Text != "tok " {
			t.Fatalf("chunk %d malformed: %s", i, ev)
		}
		last := i == 4
		if !last && (!strings.Contains(ev, `"finish_reason":null`) || c.Usage != nil) {
			t.Fatalf("intermediate chunk %d must have finish_reason null and no usage: %s", i, ev)
		}
		if last {
			if c.Choices[0].FinishReason == nil || *c.Choices[0].FinishReason != "length" {
				t.Fatalf("last chunk finish_reason: %s", ev)
			}
			want := Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}
			if c.Usage == nil || *c.Usage != want {
				t.Fatalf("usage %+v, want %+v", c.Usage, want)
			}
		}
	}
}

func TestNonStreamJSON(t *testing.T) {
	_, srv := newReady(t, fastParams(2))
	tests := []struct {
		name string
		body string
		want int
	}{
		{"explicit max_tokens", `{"prompt":"a b","max_tokens":7}`, 7},
		{"default max_tokens", `{"prompt":"a b"}`, DefaultMaxTokens},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := postCompletion(t, context.Background(), srv.URL, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get(SimulatedHeader) != "true" {
				t.Fatalf("status %d, simulated header %q", resp.StatusCode, resp.Header.Get(SimulatedHeader))
			}
			var c CompletionResponse
			if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
				t.Fatal(err)
			}
			if c.Model != DefaultModel || len(c.Choices) != 1 {
				t.Fatalf("response %+v", c)
			}
			if got := len(strings.Fields(c.Choices[0].Text)); got != tc.want {
				t.Fatalf("text has %d tokens, want %d", got, tc.want)
			}
			if fr := c.Choices[0].FinishReason; fr == nil || *fr != "length" {
				t.Fatalf("finish_reason %v, want length", fr)
			}
			want := Usage{PromptTokens: 2, CompletionTokens: tc.want, TotalTokens: 2 + tc.want}
			if c.Usage == nil || *c.Usage != want {
				t.Fatalf("usage %+v, want %+v", c.Usage, want)
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	_, srv := newReady(t, fastParams(1))
	tests := []struct{ name, body string }{
		{"invalid json", `{"prompt":`},
		{"zero max_tokens", `{"prompt":"x","max_tokens":0}`},
		{"negative max_tokens", `{"prompt":"x","max_tokens":-4}`},
		{"huge max_tokens", fmt.Sprintf(`{"prompt":"x","max_tokens":%d}`, MaxMaxTokens+1)},
	}
	for _, tc := range tests {
		resp := postCompletion(t, context.Background(), srv.URL, tc.body)
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		err := json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || err != nil || e.Error.Message == "" {
			t.Errorf("%s: status %d, decode err %v, message %q", tc.name, resp.StatusCode, err, e.Error.Message)
		}
	}
	if code := getStatus(t, srv.URL+"/v1/completions"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/completions: %d, want 405", code)
	}
}

func TestModelsAndLive(t *testing.T) {
	_, srv := newReady(t, fastParams(1))
	if body := getBody(t, srv.URL+"/v1/models"); !strings.Contains(body, DefaultModel) {
		t.Fatalf("/v1/models = %s", body)
	}
	if code := getStatus(t, srv.URL+"/health/live"); code != http.StatusOK {
		t.Fatalf("/health/live = %d", code)
	}
	if code := getStatus(t, srv.URL+"/health/ready"); code != http.StatusOK {
		t.Fatalf("/health/ready = %d", code)
	}
}

// TestConcurrencyLimit sends MaxNumSeqs+2 requests at once: two must wait
// in the replica queue, visibly in Stats and /metrics, and all complete.
func TestConcurrencyLimit(t *testing.T) {
	p := fastParams(2)
	p.InterTokenLatency = 5 * time.Millisecond
	r, srv := newReady(t, p)
	const n, tokens = 4, 30
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := doPost(context.Background(), srv.URL, fmt.Sprintf(`{"prompt":"hi there","max_tokens":%d}`, tokens))
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	waitFor(t, time.Second, "2 running + 2 waiting", func() bool { return r.Stats() == Stats{Running: 2, Waiting: 2} })
	m := getBody(t, srv.URL+"/metrics")
	for _, want := range []string{metricLine(MetricRunning, 2), metricLine(MetricWaiting, 2)} {
		if !strings.Contains(m, want) {
			t.Fatalf("/metrics lacks %q:\n%s", want, m)
		}
	}
	for i := 0; i < n; i++ {
		if code := <-codes; code != http.StatusOK {
			t.Fatalf("request finished with %d", code)
		}
	}
	waitFor(t, time.Second, "idle replica", func() bool { return r.Stats() == Stats{} })
	m = getBody(t, srv.URL+"/metrics")
	for _, want := range []string{
		metricLine(MetricRunning, 0),
		metricLine(MetricWaiting, 0),
		metricLine(MetricPromptTokens, 2*n),
		metricLine(MetricGenerationTokens, tokens*n),
		fmt.Sprintf("%s{model_name=%q,finished_reason=\"length\"} %d\n", MetricRequestSuccess, DefaultModel, n),
		fmt.Sprintf("%s_count{model_name=%q} %d\n", MetricTTFT, DefaultModel, n),
		fmt.Sprintf("%s_bucket{model_name=%q,le=\"+Inf\"} %d\n", MetricE2ELatency, DefaultModel, n),
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

// TestLoadSlowsRequests checks end to end that the HTTP path sleeps the
// load-dependent latency: one slot is held directly, so the request runs
// with 2 of 2 slots busy and every delay is 5x longer (200ms instead of
// 40ms at load 1). Only that lower bound is asserted; it cannot flake on a
// slow machine.
func TestLoadSlowsRequests(t *testing.T) {
	p := fastParams(2)
	p.PrefillOverhead = 4 * time.Millisecond
	p.InterTokenLatency = 4 * time.Millisecond
	p.TimeFactorUnderLoad = 5
	r, srv := newReady(t, p)
	if err := r.admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.release()

	const tokens = 10
	start := time.Now()
	resp := postCompletion(t, context.Background(), srv.URL, fmt.Sprintf(`{"prompt":"x","max_tokens":%d}`, tokens))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	took := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	loaded := p.PrefillTime(1, 2) + (tokens-1)*p.TokenInterval(2)
	if took < loaded {
		t.Fatalf("request took %v, want >= %v (LoadFactor(2) = %v)", took, loaded, p.LoadFactor(2))
	}
}

var sampleRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*\{[^{}]*\} [0-9.e+-]+$`)

func TestMetricsFormat(t *testing.T) {
	_, srv := newReady(t, fastParams(1))
	resp := postCompletion(t, context.Background(), srv.URL, `{"prompt":"x","max_tokens":2}`)
	resp.Body.Close()
	m := getBody(t, srv.URL+"/metrics")
	types := map[string]string{
		MetricRunning: "gauge", MetricWaiting: "gauge",
		MetricPromptTokens: "counter", MetricGenerationTokens: "counter", MetricRequestSuccess: "counter",
		MetricTTFT: "histogram", MetricE2ELatency: "histogram",
	}
	for name, typ := range types {
		if !strings.Contains(m, fmt.Sprintf("# TYPE %s %s\n", name, typ)) {
			t.Errorf("missing TYPE line for %s %s", name, typ)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(m), "\n") {
		if !strings.HasPrefix(line, "# ") && !sampleRE.MatchString(line) {
			t.Errorf("malformed sample line %q", line)
		}
	}
}

// TestClientDisconnectAborts cancels a long streaming request after its
// first chunk; the replica must free the slot without counting a success.
func TestClientDisconnectAborts(t *testing.T) {
	p := fastParams(1)
	p.InterTokenLatency = 2 * time.Millisecond
	r, srv := newReady(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := postCompletion(t, ctx, srv.URL, `{"prompt":"x","max_tokens":5000,"stream":true}`)
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line %q, err %v", line, err)
	}
	cancel()
	resp.Body.Close()
	waitFor(t, 2*time.Second, "slot release after disconnect", func() bool { return r.Stats() == Stats{} })
	m := getBody(t, srv.URL+"/metrics")
	if want := fmt.Sprintf("%s{model_name=%q,finished_reason=\"length\"} 0\n", MetricRequestSuccess, DefaultModel); !strings.Contains(m, want) {
		t.Fatalf("aborted request counted as success:\n%s", m)
	}
	if strings.Contains(m, metricLine(MetricGenerationTokens, 5000)) {
		t.Fatal("aborted request generated all tokens")
	}
}
