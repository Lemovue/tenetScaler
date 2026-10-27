package llama

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"tenantscaler/internal/replica"
)

// llama-server Prometheus metrics (enabled by --metrics).
const (
	metricProcessing = "llamacpp:requests_processing"
	metricDeferred   = "llamacpp:requests_deferred"
	// maxMetricsBytes bounds the /metrics body that is parsed.
	maxMetricsBytes = 1 << 20
)

// Stats returns Running = llamacpp:requests_processing and Waiting =
// llamacpp:requests_deferred from /metrics, cached for up to statsMaxAge.
// It is zero before the server is ready and after it stopped. A failed
// reading keeps the previous values and is logged at Warn, at most once per
// statsWarnEvery.
func (p *Process) Stats() replica.Stats {
	select {
	case <-p.exited:
		return replica.Stats{}
	default:
	}
	p.mu.Lock()
	ready := p.ready
	p.mu.Unlock()
	if !ready {
		return replica.Stats{}
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	now := p.statsClock()
	if now.Sub(p.statsAt) < statsMaxAge {
		return p.stats
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	s, err := p.fetchStats(ctx)
	p.statsAt = now
	if err != nil {
		if now.Sub(p.statsWarnAt) >= statsWarnEvery {
			p.statsWarnAt = now
			p.log.Warn("read llama-server metrics; reporting the last known load", "err", err)
		}
		return p.stats
	}
	p.stats = s
	return s
}

// fetchStats reads the request load from GET /metrics.
func (p *Process) fetchStats(ctx context.Context) (replica.Stats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/metrics", nil)
	if err != nil {
		return replica.Stats{}, err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return replica.Stats{}, fmt.Errorf("GET %s/metrics: %w", p.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return replica.Stats{}, fmt.Errorf("GET %s/metrics: %s", p.url, resp.Status)
	}
	return parseMetrics(io.LimitReader(resp.Body, maxMetricsBytes))
}

// parseMetrics extracts requests_processing (Running) and requests_deferred
// (Waiting) from the Prometheus text format. Both must be present.
func parseMetrics(r io.Reader) (replica.Stats, error) {
	var st replica.Stats
	found := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxMetricsBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, value, ok := metricSample(line)
		if !ok || (name != metricProcessing && name != metricDeferred) {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return replica.Stats{}, fmt.Errorf("metric %s: bad value %q", name, value)
		}
		n := int(math.Round(v))
		if name == metricProcessing {
			st.Running = n
		} else {
			st.Waiting = n
		}
		found[name] = true
	}
	if err := sc.Err(); err != nil {
		return replica.Stats{}, fmt.Errorf("read metrics: %w", err)
	}
	for _, name := range []string{metricProcessing, metricDeferred} {
		if !found[name] {
			return replica.Stats{}, fmt.Errorf("metric %s not found (is llama-server running with --metrics?)", name)
		}
	}
	return st, nil
}

// metricSample splits "name{labels} value [timestamp]" into name and value.
func metricSample(line string) (name, value string, ok bool) {
	end := strings.IndexAny(line, "{ \t")
	if end <= 0 {
		return "", "", false
	}
	name, rest := line[:end], line[end:]
	if rest[0] == '{' {
		rbrace := strings.LastIndexByte(rest, '}')
		if rbrace < 0 {
			return "", "", false
		}
		rest = rest[rbrace+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", false
	}
	return name, fields[0], true
}

// drain discards a small remainder of a response body so the connection
// can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
}
