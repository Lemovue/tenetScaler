package eval

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	nan := math.NaN()
	zeroTo39 := make([]float64, 40)
	for i := range zeroTo39 {
		zeroTo39[i] = float64(i)
	}
	tests := []struct {
		name   string
		values []float64
		want   Stat
	}{
		// Sum of squared deviations 32, variance 32/7, std 2.138090;
		// CI95 = t(7) 2.364624 * 2.138090 / sqrt(8) = 1.787488.
		{"textbook sample", []float64{2, 4, 4, 4, 5, 5, 7, 9}, Stat{Mean: 5, Std: 2.138090, CI95: 1.787488, N: 8}},
		// std sqrt(2); CI95 = t(1) 12.706205 * sqrt(2) / sqrt(2).
		{"NaN skipped", []float64{1, nan, 3}, Stat{Mean: 2, Std: 1.414214, CI95: 12.706205, N: 2}},
		{"one value", []float64{7}, Stat{Mean: 7, Std: nan, CI95: nan, N: 1}},
		{"empty", nil, Stat{Mean: nan, Std: nan, CI95: nan}},
		{"only NaN", []float64{nan, nan}, Stat{Mean: nan, Std: nan, CI95: nan}},
		{"identical values", []float64{3, 3, 3}, Stat{Mean: 3, Std: 0, CI95: 0, N: 3}},
		// 0..39: variance 5330/39, std 11.690452; df 39 > 30 so CI95 = 1.96 * std / sqrt(40) = 3.622909.
		{"large sample uses 1.96", zeroTo39, Stat{Mean: 19.5, Std: 11.690452, CI95: 3.622909, N: 40}},
	}
	close6 := func(a, b float64) bool {
		if math.IsNaN(a) || math.IsNaN(b) {
			return math.IsNaN(a) && math.IsNaN(b)
		}
		return math.Abs(a-b) < 1e-6
	}
	for _, tc := range tests {
		got := Summarize(tc.values)
		if got.N != tc.want.N || !close6(got.Mean, tc.want.Mean) || !close6(got.Std, tc.want.Std) || !close6(got.CI95, tc.want.CI95) {
			t.Errorf("%s: Summarize = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestStatJSON(t *testing.T) {
	nan := math.NaN()
	tests := []struct {
		st   Stat
		want string
	}{
		{Stat{Mean: 5, Std: 2.5, CI95: 1.25, N: 8}, `{"Mean":5,"Std":2.5,"CI95":1.25,"N":8}`},
		{Stat{Mean: 7, Std: nan, CI95: nan, N: 1}, `{"Mean":7,"Std":null,"CI95":null,"N":1}`},
		{Stat{Mean: math.Inf(1), Std: nan, CI95: nan}, `{"Mean":null,"Std":null,"CI95":null,"N":0}`},
	}
	for _, tc := range tests {
		data, err := json.Marshal(tc.st)
		if err != nil || string(data) != tc.want {
			t.Errorf("Marshal(%+v) = %s, %v; want %s", tc.st, data, err, tc.want)
			continue
		}
		var back Stat
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		// Infinity is not representable either; it comes back as NaN.
		wantBack := tc.st
		if math.IsInf(wantBack.Mean, 0) {
			wantBack.Mean = nan
		}
		if back.N != wantBack.N || !near(back.Mean, wantBack.Mean) || !near(back.Std, wantBack.Std) || !near(back.CI95, wantBack.CI95) {
			t.Errorf("round trip of %+v gave %+v", tc.st, back)
		}
	}
	// Inside a larger value (a slice of summaries) the encoder uses it too.
	data, err := json.Marshal([]SetupSummary{{Setup: "S7", Runs: 1, Stats: []Stat{Summarize([]float64{3})}}})
	if err != nil || !strings.Contains(string(data), `"Stats":[{"Mean":3,"Std":null,"CI95":null,"N":1}]`) {
		t.Errorf("Marshal(summaries) = %s, %v", data, err)
	}
	var missing Stat
	if err := json.Unmarshal([]byte(`{"N":2}`), &missing); err != nil || !math.IsNaN(missing.Mean) || missing.N != 2 {
		t.Errorf("missing values decoded to %+v, %v; want NaN mean and N 2", missing, err)
	}
	if err := json.Unmarshal([]byte(`{"Mean":"x"}`), &missing); err == nil {
		t.Error("a string mean should be rejected")
	}
}

func TestTCritical95(t *testing.T) {
	tests := map[int]float64{1: 12.706205, 2: 4.302653, 5: 2.570582, 9: 2.262157, 10: 2.228139,
		20: 2.085963, 29: 2.045230, 30: 2.042272, 31: 1.96, 1000: 1.96}
	for df, want := range tests {
		if got := TCritical95(df); got != want {
			t.Errorf("TCritical95(%d) = %v, want %v", df, got, want)
		}
	}
	for _, df := range []int{0, -3} {
		if got := TCritical95(df); !math.IsNaN(got) {
			t.Errorf("TCritical95(%d) = %v, want NaN", df, got)
		}
	}
	for df := 2; df <= 31; df++ {
		if TCritical95(df) >= TCritical95(df-1) {
			t.Errorf("t table not decreasing at df %d", df)
		}
	}
}

func TestJain(t *testing.T) {
	nan := math.NaN()
	tests := []struct {
		name string
		xs   []float64
		want float64
	}{
		{"equal", []float64{1, 1, 1}, 1},
		{"one takes all of four", []float64{1, 0, 0, 0}, 0.25},
		{"two values", []float64{1, 3}, 16.0 / 20}, // (1+3)^2 / (2 * (1+9))
		{"thirds", []float64{2.0 / 3, 1, 1.0 / 3}, 6.0 / 7},
		{"NaN skipped", []float64{nan, 1, 1}, 1},
		{"empty", nil, nan},
		{"all zero", []float64{0, 0}, nan},
		{"only NaN", []float64{nan}, nan},
	}
	for _, tc := range tests {
		if got := Jain(tc.xs); !near(got, tc.want) {
			t.Errorf("%s: Jain = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPercentile(t *testing.T) {
	nan := math.NaN()
	seq := func(n int) []float64 {
		xs := make([]float64, n)
		for i := range xs {
			xs[i] = float64(n - i) // descending: Percentile must sort
		}
		return xs
	}
	classic := []float64{15, 20, 35, 40, 50}
	tests := []struct {
		name string
		xs   []float64
		p    float64
		want float64
	}{
		// The nearest-rank textbook example: rank = ceil(p/100 * 5).
		{"p0 is the minimum", classic, 0, 15},
		{"p5", classic, 5, 15},
		{"p30", classic, 30, 20},
		{"p40 exact rank", classic, 40, 20},
		{"p50", classic, 50, 35},
		{"p100 is the maximum", classic, 100, 50},
		{"p99 of 1..100", seq(100), 99, 99},
		{"p99 of 1..1000", seq(1000), 99, 990},
		{"p99.9 of 1..1000 (float snap)", seq(1000), 99.9, 999},
		{"p99 of fewer than 100 is the max", seq(7), 99, 7},
		{"NaN skipped", []float64{nan, 3, 1, 2}, 50, 2},
		{"single value", []float64{4.2}, 99, 4.2},
		{"empty", nil, 50, nan},
		{"only NaN", []float64{nan}, 50, nan},
		{"p below range", classic, -1, nan},
		{"p above range", classic, 100.5, nan},
		{"p NaN", classic, nan, nan},
	}
	for _, tc := range tests {
		if got := Percentile(tc.xs, tc.p); !near(got, tc.want) {
			t.Errorf("%s: Percentile(p=%v) = %v, want %v", tc.name, tc.p, got, tc.want)
		}
	}
	in := []float64{3, 1, 2}
	Percentile(in, 50)
	if !slices.Equal(in, []float64{3, 1, 2}) {
		t.Errorf("Percentile reordered its input: %v", in)
	}
}
