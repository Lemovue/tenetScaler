package replica

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHistogramIsCumulative(t *testing.T) {
	h := newHistogram([]float64{0.1, 1})
	for _, v := range []float64{0.05, 0.5, 5} {
		h.observe(v)
	}
	if want := []uint64{1, 2}; !slices.Equal(h.counts, want) {
		t.Fatalf("bucket counts %v, want %v (cumulative)", h.counts, want)
	}
	if h.count != 3 || math.Abs(h.sum-5.55) > 1e-9 {
		t.Fatalf("count %d sum %v, want 3 and 5.55", h.count, h.sum)
	}
}

// TestMetricsWriteBuckets renders the real TTFT buckets after three
// observations whose values are exact in binary (2^-8 s, 2^-5 s, 3 s), so
// bucket lines, _sum and _count can be compared as text.
func TestMetricsWriteBuckets(t *testing.T) {
	m := newMetrics(DefaultModel)
	for _, d := range []time.Duration{3906250 * time.Nanosecond, 31250 * time.Microsecond, 3 * time.Second} {
		m.observeFirstToken(2, d)
	}
	var b strings.Builder
	if err := m.write(&b, Stats{Running: 1, Waiting: 4}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	bucket := func(le string, n int) string {
		return fmt.Sprintf("%s_bucket{model_name=%q,le=%q} %d\n", MetricTTFT, DefaultModel, le, n)
	}
	for _, want := range []string{
		bucket("0.001", 0),
		bucket("0.005", 1),
		bucket("0.02", 1),
		bucket("0.04", 2),
		bucket("2.5", 2),
		bucket("5", 3),
		bucket("2560", 3),
		bucket("+Inf", 3),
		fmt.Sprintf("%s_sum{model_name=%q} 3.03515625\n", MetricTTFT, DefaultModel),
		fmt.Sprintf("%s_count{model_name=%q} 3\n", MetricTTFT, DefaultModel),
		fmt.Sprintf("%s_bucket{model_name=%q,le=\"+Inf\"} 0\n", MetricE2ELatency, DefaultModel),
		metricLine(MetricPromptTokens, 6),
		metricLine(MetricRunning, 1),
		metricLine(MetricWaiting, 4),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	checkBucketsMonotonic(t, out, MetricTTFT, len(ttftBuckets)+1)
	checkBucketsMonotonic(t, out, MetricE2ELatency, len(e2eBuckets)+1)
}

// checkBucketsMonotonic asserts that name has want bucket lines with
// strictly increasing le bounds and non-decreasing counts, the last one
// being le="+Inf".
func checkBucketsMonotonic(t *testing.T, out, name string, want int) {
	t.Helper()
	prefix := name + "_bucket{"
	var les []string
	var counts []uint64
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		_, rest, _ := strings.Cut(line, `le="`)
		le, value, _ := strings.Cut(rest, `"} `)
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatalf("bad bucket line %q: %v", line, err)
		}
		les, counts = append(les, le), append(counts, n)
	}
	if len(les) != want || les[len(les)-1] != "+Inf" {
		t.Fatalf("%s: %d buckets ending %v, want %d ending +Inf", name, len(les), les, want)
	}
	for i := 1; i < len(les); i++ {
		prev, _ := strconv.ParseFloat(les[i-1], 64)
		cur, _ := strconv.ParseFloat(les[i], 64) // "+Inf" parses as +Inf
		if cur <= prev || counts[i] < counts[i-1] {
			t.Fatalf("%s: bucket %d (le=%s, %d) after (le=%s, %d) is not cumulative",
				name, i, les[i], counts[i], les[i-1], counts[i-1])
		}
	}
}
