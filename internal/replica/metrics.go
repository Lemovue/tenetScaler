package replica

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metric names exposed on GET /metrics. They match vLLM's names so that
// tooling written for vLLM (dashboards, KEDA/HPA queries) works unchanged.
const (
	MetricRunning          = "vllm:num_requests_running"
	MetricWaiting          = "vllm:num_requests_waiting"
	MetricPromptTokens     = "vllm:prompt_tokens_total"
	MetricGenerationTokens = "vllm:generation_tokens_total"
	MetricRequestSuccess   = "vllm:request_success_total"
	MetricTTFT             = "vllm:time_to_first_token_seconds"
	MetricE2ELatency       = "vllm:e2e_request_latency_seconds"
)

// Histogram bucket upper bounds in seconds (similar to vLLM's).
var (
	ttftBuckets = []float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5,
		0.75, 1, 2.5, 5, 7.5, 10, 20, 40, 80, 160, 640, 2560}
	e2eBuckets = []float64{0.3, 0.5, 0.8, 1, 1.5, 2, 2.5, 5, 10, 15, 20, 30, 40, 50, 60,
		120, 240, 480, 960, 1920, 7680}
)

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// metrics is one replica's own metric set, written in the Prometheus text
// exposition format (version 0.0.4). Every replica has its own instance,
// so replicas hosted in one process never share counters.
type metrics struct {
	labels string // rendered label pairs without braces

	mu               sync.Mutex
	promptTokens     uint64
	generationTokens uint64
	success          uint64
	ttft             histogram
	e2e              histogram
}

// histogram is a cumulative Prometheus histogram.
type histogram struct {
	bounds []float64
	counts []uint64 // counts[i] = observations <= bounds[i]
	sum    float64
	count  uint64
}

func newMetrics(model string) *metrics {
	return &metrics{
		labels: fmt.Sprintf(`model_name="%s"`, labelEscaper.Replace(model)),
		ttft:   newHistogram(ttftBuckets),
		e2e:    newHistogram(e2eBuckets),
	}
}

func newHistogram(bounds []float64) histogram {
	return histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

// observeFirstToken records a finished prefill: its prompt tokens and its
// time to first token measured from arrival (queueing included).
func (m *metrics) observeFirstToken(promptTokens int, ttft time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.promptTokens += uint64(promptTokens)
	m.ttft.observe(ttft.Seconds())
}

// addGenerated counts generated output tokens as they are produced.
func (m *metrics) addGenerated(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generationTokens += uint64(n)
}

// observeSuccess records a request that produced all its tokens.
func (m *metrics) observeSuccess(e2e time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.success++
	m.e2e.observe(e2e.Seconds())
}

// write renders all metrics; load supplies the running/waiting gauges.
func (m *metrics) write(w io.Writer, load Stats) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bw := bufio.NewWriter(w)
	m.sample(bw, MetricRunning, "gauge", "Number of requests in the simulated batch.", float64(load.Running))
	m.sample(bw, MetricWaiting, "gauge", "Number of requests waiting in the replica queue.", float64(load.Waiting))
	m.sample(bw, MetricPromptTokens, "counter", "Number of prefill tokens processed.", float64(m.promptTokens))
	m.sample(bw, MetricGenerationTokens, "counter", "Number of generation tokens processed.", float64(m.generationTokens))
	header(bw, MetricRequestSuccess, "counter", "Count of successfully processed requests.")
	fmt.Fprintf(bw, "%s{%s,finished_reason=\"length\"} %s\n", MetricRequestSuccess, m.labels, formatFloat(float64(m.success)))
	m.histogram(bw, MetricTTFT, "Histogram of time to first token in seconds.", &m.ttft)
	m.histogram(bw, MetricE2ELatency, "Histogram of end to end request latency in seconds.", &m.e2e)
	return bw.Flush()
}

func header(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s (SIMULATED engine)\n# TYPE %s %s\n", name, help, name, typ)
}

func (m *metrics) sample(w io.Writer, name, typ, help string, v float64) {
	header(w, name, typ, help)
	fmt.Fprintf(w, "%s{%s} %s\n", name, m.labels, formatFloat(v))
}

func (m *metrics) histogram(w io.Writer, name, help string, h *histogram) {
	header(w, name, "histogram", help)
	for i, b := range h.bounds {
		fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n", name, m.labels, formatFloat(b), h.counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, m.labels, h.count)
	fmt.Fprintf(w, "%s_sum{%s} %s\n", name, m.labels, formatFloat(h.sum))
	fmt.Fprintf(w, "%s_count{%s} %d\n", name, m.labels, h.count)
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
