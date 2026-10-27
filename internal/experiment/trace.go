package experiment

import (
	"bufio"
	"cmp"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/estimate"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/forecast"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// TraceCitation credits the real trace (CC-BY; BRIEF-v2 Q18).
const TraceCitation = "Azure LLM inference trace 2024 (code, one week), github.com/Azure/AzurePublicDataset, " +
	"release dataset-llm-2024; Stojkovic et al., \"DynamoLLM: Designing LLM Inference Clusters for Performance " +
	"and Energy Efficiency\", HPCA 2025 (CC-BY)"

// azureTimeLayout is the trace's TIMESTAMP format.
const azureTimeLayout = "2006-01-02 15:04:05.999999-07:00"

// RateSeries is a request trace bucketed into rates.
type RateSeries struct {
	Start  time.Time
	Bucket time.Duration
	// Weighted is weighted tokens/s (context + 2 x generated, the
	// controller's demand unit) and Requests requests/s, per bucket.
	Weighted, Requests []float64
	// Rows is the number of requests; Early counts rows timestamped before
	// the first row, which are put in bucket 0.
	Rows, Early int
}

// LoadAzureTrace reads an Azure LLM inference trace CSV (TIMESTAMP,
// ContextTokens, GeneratedTokens) and buckets it from its first row on.
func LoadAzureTrace(r io.Reader, bucket time.Duration) (RateSeries, error) {
	if bucket <= 0 {
		return RateSeries{}, errors.New("trace: bucket must be > 0")
	}
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.ReuseRecord = true
	head, err := cr.Read()
	if err != nil {
		return RateSeries{}, fmt.Errorf("trace: header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	ti, ok1 := col["TIMESTAMP"]
	ci, ok2 := col["ContextTokens"]
	gi, ok3 := col["GeneratedTokens"]
	if !ok1 || !ok2 || !ok3 {
		return RateSeries{}, fmt.Errorf("trace: want columns TIMESTAMP, ContextTokens, GeneratedTokens, got %v", head)
	}
	s := RateSeries{Bucket: bucket}
	var tokens, reqs []float64
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return RateSeries{}, fmt.Errorf("trace: line %d: %w", line, err)
		}
		t, err := time.Parse(azureTimeLayout, rec[ti])
		if err != nil {
			return RateSeries{}, fmt.Errorf("trace: line %d: %w", line, err)
		}
		in, err1 := strconv.ParseFloat(rec[ci], 64)
		out, err2 := strconv.ParseFloat(rec[gi], 64)
		if err1 != nil || err2 != nil || in < 0 || out < 0 {
			return RateSeries{}, fmt.Errorf("trace: line %d: bad token counts %q, %q", line, rec[ci], rec[gi])
		}
		if s.Rows == 0 {
			s.Start = t
		}
		s.Rows++
		// Duration division truncates toward zero, so a row less than one
		// bucket early would land in bucket 0 without being counted: compare
		// the times instead.
		i := 0
		if t.Before(s.Start) {
			s.Early++
		} else {
			i = int(t.Sub(s.Start) / bucket)
		}
		for len(tokens) <= i {
			tokens, reqs = append(tokens, 0), append(reqs, 0)
		}
		tokens[i] += model.WeightedTokens(in, out)
		reqs[i]++
	}
	if s.Rows == 0 {
		return RateSeries{}, errors.New("trace: no rows")
	}
	secs := bucket.Seconds()
	for i := range tokens {
		s.Weighted = append(s.Weighted, tokens[i]/secs)
		s.Requests = append(s.Requests, reqs[i]/secs)
	}
	return s, nil
}

// TracePlan configures the real-trace forecaster evaluation.
type TracePlan struct {
	Series RateSeries
	// Season is the forecasters' season in buckets (a day: 288 at 5 min).
	Season int
	// HeldOutSeasons are the last whole seasons scored, one by one, each
	// after observing every earlier bucket (rolling one-step forecasts).
	HeldOutSeasons int
	// Config is the real configuration; the S7 vs S7F simulation drives
	// tenant B with the rescaled series, one trace bucket per control cycle.
	Config   config.Config
	Runs     int
	BaseSeed int64
	// ColdStart of the simulated replicas.
	ColdStart time.Duration
}

// DayAccuracy is one forecaster's error on one held-out season.
type DayAccuracy struct {
	Method  string
	Season  int // index of the held-out season in the series
	Metrics forecast.Metrics
}

// TraceEffect is one setup's outcome for tenant B driven by the trace.
type TraceEffect struct {
	Setup string
	// BViolationPct is the share of B's non-shed requests in the scored
	// part that missed the SLO; BP99TTFT is their p99 TTFT (s).
	BViolationPct, BP99TTFT, ReplicaMinutes eval.Stat
	// ReactionSeconds is the mean, over B's step-ups in replica need, of
	// the time from the step until the desired pool rose above its level
	// 30 s before the step (negative: pre-warmed); Unreacted counts steps
	// without such a rise within 60 s.
	ReactionSeconds, Unreacted eval.Stat
}

// TraceReport is the outcome of EvaluateTrace.
type TraceReport struct {
	Rows, Early, Buckets int
	BucketSeconds        float64
	Season               int
	Accuracy             []DayAccuracy
	MAPE                 map[string]eval.Stat // per method over the held-out seasons
	// Scale multiplies the trace's request rate into tenant B's rate.
	Scale            float64
	SimulatedSeasons int
	Effects          []TraceEffect
	Steps            int // B's step-ups per run (the same trace every seed)
}

// traceMethods are the forecasters compared on the real trace: Holt-Winters
// with the a-priori hyperparameters of S7F, seasonal-naive and naive.
func traceMethods(season int) []method {
	return []method{
		func() forecast.Forecaster {
			return forecast.NewHoltWinters(sim.ForecastAlpha, sim.ForecastBeta, sim.ForecastGamma, season)
		},
		func() forecast.Forecaster { return forecast.NewSeasonalNaive(season) },
		func() forecast.Forecaster { return forecast.NewNaive() },
	}
}

// EvaluateTrace scores the forecasters on the held-out seasons of the
// weighted-token series (no leakage: forecast.Evaluate predicts each bucket
// before observing it), then simulates S7 and S7F with tenant B driven by
// the trace's request rate.
func EvaluateTrace(p TracePlan) (TraceReport, error) {
	s := p.Series
	n := len(s.Weighted)
	if p.Season < 2 || p.HeldOutSeasons < 1 || n < (2+p.HeldOutSeasons)*p.Season {
		return TraceReport{}, fmt.Errorf("trace: %d buckets cannot hold 2 training seasons plus %d held-out seasons of %d", n, p.HeldOutSeasons, p.Season)
	}
	rep := TraceReport{Rows: s.Rows, Early: s.Early, Buckets: n, BucketSeconds: s.Bucket.Seconds(), Season: p.Season,
		MAPE: map[string]eval.Stat{}}
	whole := n / p.Season // complete seasons
	per := map[string][]float64{}
	for d := whole - p.HeldOutSeasons; d < whole; d++ {
		start, end := d*p.Season, (d+1)*p.Season
		for _, mk := range traceMethods(p.Season) {
			f := mk()
			m := forecast.Evaluate(s.Weighted[:end], start, f)
			rep.Accuracy = append(rep.Accuracy, DayAccuracy{Method: f.Name(), Season: d, Metrics: m})
			per[f.Name()] = append(per[f.Name()], m.MAPE)
		}
	}
	for name, v := range per {
		rep.MAPE[name] = eval.Summarize(v)
	}
	effects, err := traceEffect(p, &rep)
	if err != nil {
		return rep, err
	}
	rep.Effects = effects
	return rep, nil
}

// bReplicaTarget is how many replicas tenant B's 95th-percentile bucket
// needs after rescaling, so that B's daily peaks exceed its guarantee of
// one replica and pre-warming can matter.
const bReplicaTarget = 2

// traceEffect drives tenant B with the trace's request rate, rescaled so
// its 95th-percentile bucket needs bReplicaTarget replicas, one trace
// bucket per control cycle, over the last 2 + HeldOutSeasons seasons (two
// for the forecaster to learn, the rest scored); A and C run steady.
func traceEffect(p TracePlan, rep *TraceReport) ([]TraceEffect, error) {
	cfg := p.Config
	ctl := cfg.Controller
	perReq := meanWeightedRequest(workload.RealScenario(), p.BaseSeed)
	seasons := 2 + p.HeldOutSeasons
	whole := len(p.Series.Requests) / p.Season
	rates := slices.Clone(p.Series.Requests[(whole-seasons)*p.Season : whole*p.Season])
	p95 := eval.Percentile(rates, 95)
	if !(p95 > 0) || !(perReq > 0) {
		return nil, errors.New("trace: cannot rescale a series without traffic")
	}
	need := float64(bReplicaTarget) * ctl.ReplicaCapacityTokensPerSec / (1 + ctl.Headroom) / perReq // req/s
	rep.Scale = need / p95
	for i := range rates {
		rates[i] *= rep.Scale
	}
	rep.SimulatedSeasons = seasons
	cfg.Controller.Forecast.SeasonBuckets = p.Season
	cycle := ctl.CycleSeconds
	base := workload.RealScenario()
	sc := workload.Scenario{Name: "azure-trace-B", Duration: time.Duration(float64(len(rates)) * cycle * float64(time.Second))}
	for _, t := range base.Tenants {
		switch t.Tenant {
		case "B":
			t.Kind, t.Series, t.SeriesBucketSeconds = workload.KindSeries, rates, cycle
		default:
			t.Kind, t.PeakRPS = workload.KindSteady, t.BaseRPS
		}
		sc.Tenants = append(sc.Tenants, t)
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	scored := time.Duration(float64(2*p.Season) * cycle * float64(time.Second))
	steps := needSteps(rates, perReq, ctl)
	rep.Steps = len(steps)
	var effects []TraceEffect
	for _, setup := range []string{sim.SetupFull, sim.SetupFullForecast} {
		var viol, p99, minutes, react, unreacted []float64
		for r := range p.Runs {
			res, err := sim.Simulate(sim.Options{Setup: setup, Config: cfg, Scenario: sc, Seed: p.BaseSeed + int64(r), ColdStart: p.ColdStart})
			if err != nil {
				return nil, fmt.Errorf("simulate %s: %w", setup, err)
			}
			v, q := tenantStatsAfter(res.Data, "B", scored)
			viol, p99 = append(viol, v), append(p99, q)
			minutes = append(minutes, eval.Compute(res.Data).ReplicaMinutes)
			mean, missed := reactions(res.Data.Timeline, steps, cycle, scored)
			react, unreacted = append(react, mean), append(unreacted, float64(missed))
		}
		effects = append(effects, TraceEffect{Setup: setup, BViolationPct: eval.Summarize(viol), BP99TTFT: eval.Summarize(p99),
			ReplicaMinutes: eval.Summarize(minutes), ReactionSeconds: eval.Summarize(react), Unreacted: eval.Summarize(unreacted)})
	}
	return effects, nil
}

// needSteps returns the start times (s) of the buckets where B's replica
// need (estimate.Replicas of its weighted rate) rises.
func needSteps(rates []float64, perReq float64, ctl config.ControllerSpec) []float64 {
	var steps []float64
	prev := -1
	for i, r := range rates {
		n := estimate.Replicas(r*perReq, ctl.Headroom, ctl.ReplicaCapacityTokensPerSec)
		if prev >= 0 && n > prev {
			steps = append(steps, float64(i)*ctl.CycleSeconds)
		}
		prev = n
	}
	return steps
}

// reactions measures, for the steps at or after from, the time from the
// step until Desired first exceeds its value 30 s before the step, searching
// [step-30 s, step+60 s]; it returns the mean over steps that reacted and
// how many did not.
func reactions(tl []eval.Sample, steps []float64, cycle float64, from time.Duration) (float64, int) {
	desiredAt := func(t float64) (int, bool) {
		i, ok := slices.BinarySearchFunc(tl, time.Duration(t*float64(time.Second)), func(s eval.Sample, d time.Duration) int {
			return cmp.Compare(s.T, d)
		})
		if !ok && i >= len(tl) {
			return 0, false
		}
		return tl[min(i, len(tl)-1)].Desired, true
	}
	var sum float64
	n, missed := 0, 0
	for _, st := range steps {
		if st < from.Seconds() {
			continue
		}
		base, ok := desiredAt(st - 30)
		if !ok {
			continue
		}
		reacted := false
		for t := st - 30; t <= st+60; t += 1 {
			if d, ok := desiredAt(t); ok && d > base {
				sum += t - st
				n++
				reacted = true
				break
			}
		}
		if !reacted {
			missed++
		}
	}
	if n == 0 {
		return math.NaN(), missed
	}
	return sum / float64(n), missed
}

// tenantStatsAfter is a tenant's SLO violation % (non-shed, failed count
// as misses) and p99 TTFT (completed) over requests arriving at or after
// from.
func tenantStatsAfter(d eval.RunData, tenant string, from time.Duration) (float64, float64) {
	slo := d.SLOs[tenant]
	served, miss := 0, 0
	var ttfts []float64
	for _, r := range d.Records {
		if r.Tenant != tenant || r.Shed || r.Arrival < from {
			continue
		}
		served++
		if !r.MetSLO(slo) {
			miss++
		}
		if !r.Failed {
			ttfts = append(ttfts, r.TTFT().Seconds())
		}
	}
	if served == 0 {
		return math.NaN(), math.NaN()
	}
	return 100 * float64(miss) / float64(served), eval.Percentile(ttfts, 99)
}

// meanWeightedRequest is the mean weighted tokens per request (prompt
// words + 2 x output tokens) of the scenario's trace for seed.
func meanWeightedRequest(sc workload.Scenario, seed int64) float64 {
	specs := workload.Generate(sc, seed)
	if len(specs) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, sp := range specs {
		sum += model.WeightedTokens(float64(sp.InputTokens), float64(sp.OutputTokens))
	}
	return sum / float64(len(specs))
}
