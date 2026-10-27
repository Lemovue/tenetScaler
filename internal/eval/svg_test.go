package eval

import (
	"encoding/xml"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestNiceTicks(t *testing.T) {
	tests := []struct {
		lo, hi float64
		want   []float64
		step   float64
	}{
		{0, 10, []float64{0, 2, 4, 6, 8, 10}, 2},
		{0, 1, []float64{0, 0.2, 0.4, 0.6, 0.8, 1}, 0.2},
		{3, 97, []float64{0, 20, 40, 60, 80, 100}, 20},
		{-7, 3, []float64{-8, -6, -4, -2, 0, 2, 4}, 2},
		{5, 5, []float64{5, 5.2, 5.4, 5.6, 5.8, 6}, 0.2}, // empty range widens to [lo, lo+1]
		{0, 4500, []float64{0, 1000, 2000, 3000, 4000, 5000}, 1000},
	}
	for _, tc := range tests {
		got, step := niceTicks(tc.lo, tc.hi, 5)
		if len(got) != len(tc.want) || math.Abs(step-tc.step) > 1e-12 {
			t.Errorf("niceTicks(%v, %v) = %v step %v, want %v step %v", tc.lo, tc.hi, got, step, tc.want, tc.step)
			continue
		}
		for i := range got {
			if math.Abs(got[i]-tc.want[i]) > 1e-9 {
				t.Errorf("niceTicks(%v, %v) = %v, want %v", tc.lo, tc.hi, got, tc.want)
				break
			}
		}
	}
}

// TestNiceTicksLargeMagnitudes covers ranges where round tick indices are
// not exact float64 integers; stepping them used to never terminate.
func TestNiceTicksLargeMagnitudes(t *testing.T) {
	tests := []struct {
		name   string
		lo, hi float64
	}{
		{"range tiny next to its magnitude", 1e17, 1e17 + 16},
		{"negative", -1e17 - 16, -1e17},
		{"one Unix-nanosecond value: lo+1 rounds to lo", 1.7e18, 1.7e18},
		{"just past the exact-index limit", 1 << 53, 1<<53 + 2},
		{"overflowing span", -1e308, 1e308},
	}
	for _, tc := range tests {
		ticks, _ := niceTicks(tc.lo, tc.hi, 6)
		n := len(ticks)
		if n < 2 || n > 30 || !(ticks[0] <= tc.lo) || !(ticks[n-1] >= tc.hi) {
			t.Errorf("%s: niceTicks(%v, %v) = %v, want a few ticks covering the range", tc.name, tc.lo, tc.hi, ticks)
			continue
		}
		for i := 1; i < n; i++ {
			if !(ticks[i] > ticks[i-1]) || !finite(ticks[i]) {
				t.Errorf("%s: ticks %v are not finite and increasing", tc.name, ticks)
				break
			}
		}
	}
	for _, xs := range [][]float64{{1e17, 1e17 + 16}, {1.7e18}} {
		out := render(t, LineChart{Title: "ns", Series: []Series{{Name: "s", X: xs, Y: make([]float64, len(xs))}}}.WriteSVG)
		if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
			t.Errorf("line chart over x %v has non-finite coordinates:\n%s", xs, out)
		}
	}
}

func TestTickLabel(t *testing.T) {
	tests := []struct {
		v, step float64
		want    string
	}{
		{0.6000000000000001, 0.2, "0.6"},
		{1e-17, 0.2, "0"},
		{15, 5, "15"},
		{0.05, 0.05, "0.05"},
		{2500, 500, "2500"},
		{2000, 1000, "2K"},
		{-4000, 2000, "-4K"},
	}
	for _, tc := range tests {
		if got := tickLabel(tc.v, tc.step); got != tc.want {
			t.Errorf("tickLabel(%v, %v) = %q, want %q", tc.v, tc.step, got, tc.want)
		}
	}
}

func TestWrapLabel(t *testing.T) {
	tests := []struct {
		s         string
		max       int
		want      []string
		truncated bool
	}{
		{"S7 TenantScaler full", 12, []string{"S7", "TenantScaler", "full"}, false},
		{"a b c", 3, []string{"a b", "c"}, false},
		{"KEDA", 10, []string{"KEDA"}, false},
		{"  ", 5, nil, false},
		// A word that does not fit breaks after its hyphens.
		{"S4 Flow-Control-equivalent", 12, []string{"S4 Flow-", "Control-", "equivalent"}, false},
		{"a-b-c", 4, []string{"a-b-", "c"}, false},
		{"x-y z", 3, []string{"x-y", "z"}, false}, // fits whole: not split
		{"ünïcödé-wörds", 8, []string{"ünïcödé-", "wörds"}, false},
		// Still too long: shortened with an ellipsis.
		{"Supercalifragilistic", 8, []string{"Superca…"}, true},
		{"ok Supercalifragilistic", 8, []string{"ok", "Superca…"}, true},
		{"ab", 1, []string{"ab"}, false}, // width floor of two runes
	}
	for _, tc := range tests {
		got, truncated := wrapLabel(tc.s, tc.max)
		if !reflect.DeepEqual(got, tc.want) || truncated != tc.truncated {
			t.Errorf("wrapLabel(%q, %d) = %q, %v; want %q, %v", tc.s, tc.max, got, truncated, tc.want, tc.truncated)
		}
	}
}

func TestBarChartLongGroupLabels(t *testing.T) {
	// Seven groups on 720 px: 89 px bands hold 12 characters per line.
	groups := []string{"S1", "S2", "S3", "S4 Flow-Control-equivalent", "S5", "S6", "Averyveryveryverylongwordlabel"}
	values := []float64{1, 2, 3, 4, 5, 6, 7}
	out := render(t, BarChart{Groups: groups, Series: []BarSeries{{Name: "x", Values: values}}}.WriteSVG)
	for _, want := range []string{">S4 Flow-</text>", ">Control-</text>", ">equivalent</text>",
		"<g><title>Averyveryveryverylongwordlabel</title>", ">Averyveryve…</text>"} {
		if !strings.Contains(out, want) {
			t.Errorf("SVG lacks %q", want)
		}
	}
	if strings.Contains(out, "<title>S4 Flow-Control-equivalent</title>") {
		t.Error("a label that fits after wrapping should not get a tooltip")
	}
}

func TestBarPath(t *testing.T) {
	// Positive: 4 px rounded top at y=40, square at the baseline y=100.
	if got, want := barPath(10, 20, 100, 40), "M10.0 100.0 V44.0 Q10.0 40.0 14.0 40.0 H26.0 Q30.0 40.0 30.0 44.0 V100.0 Z"; got != want {
		t.Errorf("positive bar path\n%s\nwant\n%s", got, want)
	}
	// Negative and only 2 px tall: the radius shrinks to the height and the
	// rounded end points down.
	if got, want := barPath(10, 20, 100, 102), "M10.0 100.0 V100.0 Q10.0 102.0 12.0 102.0 H28.0 Q30.0 102.0 30.0 100.0 V100.0 Z"; got != want {
		t.Errorf("negative bar path\n%s\nwant\n%s", got, want)
	}
}

// render writes a chart and checks the output is well-formed XML.
func render(t *testing.T, write func(io.Writer) error) string {
	t.Helper()
	var b strings.Builder
	if err := write(&b); err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(strings.NewReader(b.String()))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("invalid SVG: %v\n%s", err, b.String())
		}
	}
	return b.String()
}

func TestLineChart(t *testing.T) {
	c := LineChart{
		Title: "Ready replicas <simulated> & more", XLabel: "time (s)", YLabel: "replicas",
		Series: []Series{
			{Name: "KEDA", X: []float64{0, 1, 2, 3}, Y: []float64{1, 2, 3, 4}},
			{Name: "TenantScaler", X: []float64{0, 1, 2, 3}, Y: []float64{2, math.NaN(), 5, 6}},
		},
	}
	out := render(t, c.WriteSVG)
	for _, want := range []string{
		"Ready replicas &lt;simulated&gt; &amp; more", "@media (prefers-color-scheme:dark)",
		">KEDA</text>", ">TenantScaler</text>", ">time (s)</text>", ">replicas</text>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SVG lacks %q", want)
		}
	}
	if n := strings.Count(out, `<path class="line k`); n != 2 {
		t.Errorf("%d series paths, want 2", n)
	}
	// The NaN point splits the second series into two subpaths.
	second := out[strings.Index(out, `<path class="line k2"`):]
	second = second[:strings.Index(second, "</path>")]
	if n := strings.Count(second, "M"); n != 2 {
		t.Errorf("second series has %d subpaths, want 2: %s", n, second)
	}
	if out != render(t, c.WriteSVG) {
		t.Error("rendering is not deterministic")
	}
	single := render(t, LineChart{Title: "one", Series: []Series{{Name: "only", X: []float64{0, 1}, Y: []float64{1, 1}}}}.WriteSVG)
	if strings.Contains(single, `<line class="line k1"`) {
		t.Error("a single series should have no legend")
	}
	empty := render(t, LineChart{Series: []Series{{Name: "none"}}}.WriteSVG)
	if !strings.Contains(empty, `<path class="line k1" d=""`) {
		t.Error("a series without points should render an empty path")
	}
}

func TestBarChart(t *testing.T) {
	c := BarChart{
		Title: "Victim SLO violations (simulated)", YLabel: "%",
		Groups: []string{"S2 KEDA", "S4 Flow Control", "S7 TenantScaler"},
		Series: []BarSeries{
			{Name: "A", Values: []float64{30, 10, 2}, Errors: []float64{1, math.NaN(), 2}},
			{Name: "B", Values: []float64{25, math.NaN(), 1}, Errors: []float64{0.5, 1, 1}},
		},
	}
	out := render(t, c.WriteSVG)
	if n := strings.Count(out, `<path class="f`); n != 5 {
		t.Errorf("%d bars, want 5 (one value is NaN)", n)
	}
	// Error bars (3 lines each) for A/S2, A/S7, B/S2, B/S7.
	if n := strings.Count(out, `<line class="err"`); n != 12 {
		t.Errorf("%d error-bar lines, want 12", n)
	}
	for _, want := range []string{"<title>A, S2 KEDA: 30.000 ± 1.000</title>", "<title>A, S4 Flow Control: 10.000</title>",
		`class="f1"`, `class="f2"`, ">S7 TenantScaler</text>"} {
		if !strings.Contains(out, want) {
			t.Errorf("SVG lacks %q", want)
		}
	}
}

func TestBarChartGeometry(t *testing.T) {
	out := render(t, BarChart{Groups: []string{"only"}, Series: []BarSeries{{Name: "x", Values: []float64{10}}}}.WriteSVG)
	// 720x360, no title or legend: plot top 28, bottom 360-36 = 324, left 56,
	// right 696; y ticks 0..10. One 24 px bar centred in the 640 px band
	// starts at x = 56 + (640-24)/2 = 364 and reaches y = 28.
	want := `d="M364.0 324.0 V32.0 Q364.0 28.0 368.0 28.0 H384.0 Q388.0 28.0 388.0 32.0 V324.0 Z"`
	if !strings.Contains(out, want) {
		t.Errorf("bar geometry: want %s in\n%s", want, out)
	}
}

func TestChartErrors(t *testing.T) {
	nine := make([]Series, MaxSeries+1)
	nineBars := make([]BarSeries, MaxSeries+1)
	for i := range nineBars {
		nineBars[i] = BarSeries{Values: []float64{1}}
	}
	tests := []struct {
		name  string
		write func(io.Writer) error
		want  string
	}{
		{"line without series", LineChart{Title: "t"}.WriteSVG, "has no series"},
		{"too many lines", LineChart{Series: nine}.WriteSVG, "at most 8"},
		{"mismatched line", LineChart{Series: []Series{{Name: "s", X: []float64{1, 2}, Y: []float64{1}}}}.WriteSVG, "2 x values and 1 y values"},
		{"tiny line chart", LineChart{Width: 60, Height: 60, Series: []Series{{Name: "s"}}}.WriteSVG, "too small"},
		{"bars without groups", BarChart{Series: []BarSeries{{Name: "s"}}}.WriteSVG, "no groups"},
		{"bars without series", BarChart{Groups: []string{"g"}}.WriteSVG, "has no series"},
		{"too many bars", BarChart{Groups: []string{"g"}, Series: nineBars}.WriteSVG, "at most 8"},
		{"wrong value count", BarChart{Groups: []string{"g", "h"}, Series: []BarSeries{{Name: "s", Values: []float64{1}}}}.WriteSVG, "needs 2 values"},
		{"wrong error count", BarChart{Groups: []string{"g"}, Series: []BarSeries{{Name: "s", Values: []float64{1}, Errors: []float64{1, 2}}}}.WriteSVG, "needs 1 values"},
		{"negative error", BarChart{Groups: []string{"g"}, Series: []BarSeries{{Name: "s", Values: []float64{1}, Errors: []float64{-1}}}}.WriteSVG, "negative error"},
	}
	for _, tc := range tests {
		err := tc.write(&strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", tc.name, err, tc.want)
		}
	}
	ok := LineChart{Series: []Series{{Name: "s", X: []float64{0}, Y: []float64{1}}}}
	if err := ok.WriteSVG(failingWriter{}); err == nil {
		t.Error("WriteSVG ignored a write error")
	}
}
