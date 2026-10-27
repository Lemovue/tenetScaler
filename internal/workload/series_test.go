package workload

import (
	"math"
	"testing"
	"time"
)

// A series pattern follows its rate per bucket, is 0 after the series, and
// the generator's arrivals follow it.
func TestSeriesPattern(t *testing.T) {
	p := TenantPattern{Tenant: "B", Kind: KindSeries, Series: []float64{1, 0, 4}, SeriesBucketSeconds: 100, CV: 1,
		Mix: PriorityMix{Standard: 1}, In: LengthDist{Median: 10, Sigma: 0.1, Min: 1, Max: 20}, Out: LengthDist{Median: 5, Sigma: 0.1, Min: 1, Max: 10}}
	for at, want := range map[float64]float64{0: 1, 99: 1, 150: 0, 250: 4, 300: 0, -1: 0} {
		if got := RateAt(p, at); got != want {
			t.Errorf("RateAt(%v) = %v, want %v", at, got, want)
		}
	}
	sc := Scenario{Name: "series", Duration: 300 * time.Second, Tenants: []TenantPattern{p}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	var counts [3]int
	for _, s := range Generate(sc, 1) {
		counts[int(s.Offset.Seconds())/100]++
	}
	if counts[1] != 0 || math.Abs(float64(counts[0])-100) > 40 || math.Abs(float64(counts[2])-400) > 80 {
		t.Fatalf("arrivals per bucket %v, want about 100, 0, 400", counts)
	}
	for name, bad := range map[string]TenantPattern{
		"empty":    {Kind: KindSeries, SeriesBucketSeconds: 1},
		"bucket":   {Kind: KindSeries, Series: []float64{1}},
		"negative": {Kind: KindSeries, Series: []float64{-1}, SeriesBucketSeconds: 1},
	} {
		bad.Tenant, bad.CV, bad.Mix, bad.In, bad.Out = "B", 1, p.Mix, p.In, p.Out
		if err := (Scenario{Name: "bad", Duration: time.Second, Tenants: []TenantPattern{bad}}).Validate(); err == nil {
			t.Errorf("%s series accepted", name)
		}
	}
}
