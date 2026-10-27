package realrun

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

// ScalingLevel is one point of the replica scaling curve: k replicas of the
// budget loaded at once, each closed-loop at the same concurrency, measured
// in one common window.
type ScalingLevel struct {
	Replicas int
	// Pooled is measured over every replica's requests: its throughput is
	// the sum over replicas and its percentiles are pooled.
	Pooled Level
	// PerReplica holds each replica's own measurement, in endpoint order.
	PerReplica []Level
	// CPUPerformancePct is the CPU's effective frequency as a percentage of
	// nominal, sampled at the end of the window (NaN if not readable).
	CPUPerformancePct float64
}

// ScalingCurve launches the configured backend's replicas, each on its own
// core, and for k = 1..n scales the pool to k and drives every ready
// replica closed-loop at concurrency c (default: slots per replica, the
// gateway's dispatch window) in one common window. It shows how the
// aggregate capacity grows with the replica budget on this machine,
// interference between replicas (memory bandwidth, caches, power limits)
// included.
func ScalingCurve(ctx context.Context, plan ProfilePlan, n, c int) ([]ScalingLevel, error) {
	plan = plan.withDefaults()
	cfg := plan.Config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("scaling: config: %w", err)
	}
	if err := cpuOnly(cfg); err != nil {
		return nil, fmt.Errorf("scaling: %w", err)
	}
	if n < 1 || n > cfg.Pool.CapacityReplicas {
		return nil, fmt.Errorf("scaling: replicas must be in [1, capacity %d], got %d", cfg.Pool.CapacityReplicas, n)
	}
	if c <= 0 {
		c = stack.BackendSlots(cfg)
	}
	if _, err := pinServices(cfg, plan.Logger); err != nil {
		return nil, fmt.Errorf("scaling: %w", err)
	}
	backend, err := stack.NewBackend(cfg, plan.LogDir, plan.Seed, plan.Logger)
	if err != nil {
		return nil, fmt.Errorf("scaling: %w", err)
	}
	timeout, err := stack.ReadyTimeout(cfg)
	if err != nil {
		return nil, err
	}
	p := pool.New(pool.Config{Name: "scaling", MaxReplicas: n, Seed: plan.Seed, Backend: backend, Logger: plan.Logger})
	defer p.Close()
	lengths := workload.Generate(profileScenario(plan.In, plan.Out), plan.Seed)
	slo := strictestSLO(cfg.Tenants)
	warn := slog.New(warnOnly{plan.Logger.Handler()})
	var levels []ScalingLevel
	for k := 1; k <= n; k++ {
		if err := p.Scale(k); err != nil {
			return levels, fmt.Errorf("scaling: scale to %d: %w", k, err)
		}
		if err := stack.WaitReady(ctx, p, k, timeout); err != nil {
			return levels, fmt.Errorf("scaling: %w", err)
		}
		eps := p.Endpoints()
		if len(eps) != k {
			return levels, fmt.Errorf("scaling: %d endpoints ready, want %d", len(eps), k)
		}
		lv := scalingLevel(ctx, eps, k, c, plan, lengths, slo, warn)
		levels = append(levels, lv)
		plan.Logger.Info("scaling level done", "replicas", k, "weightedTokPerSec", lv.Pooled.WeightedTokensPerSec,
			"p99TTFT", lv.Pooled.TTFTP99, "p99TPOT", lv.Pooled.TPOTP99, "cpuPerformancePct", lv.CPUPerformancePct)
		if err := ctx.Err(); err != nil {
			return levels, err
		}
		waitIdle(ctx, p)
	}
	return levels, nil
}

// scalingLevel drives the k endpoints at once and measures the common
// window.
func scalingLevel(ctx context.Context, eps []string, k, c int, plan ProfilePlan, lengths []workload.Spec, slo model.SLO, warn *slog.Logger) ScalingLevel {
	start := time.Now()
	winStart, winEnd := start.Add(plan.Warmup), start.Add(plan.Warmup+plan.LevelDuration)
	runs := make([][]sentRequest, len(eps))
	var wg sync.WaitGroup
	for i, url := range eps {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DisableKeepAlives = true // see measure: llama-server closes idle connections
		d := &driver{url: url, hc: &http.Client{Transport: tr}, seed: plan.Seed*7919 + int64(1000*k+i), warn: warn}
		// Each replica cycles through the lengths from its own offset.
		own := append(append([]workload.Spec{}, lengths[i*97%len(lengths):]...), lengths[:i*97%len(lengths)]...)
		wg.Go(func() { runs[i] = d.closedLoop(ctx, c, winEnd, own) })
	}
	wg.Wait()
	lv := ScalingLevel{Replicas: k, CPUPerformancePct: CPUPerformancePct()}
	var all []sentRequest
	for _, r := range runs {
		lv.PerReplica = append(lv.PerReplica, measureLevel(c, r, winStart, winEnd, slo))
		all = append(all, r...)
	}
	lv.Pooled = measureLevel(c*k, all, winStart, winEnd, slo)
	return lv
}

// ScalingTable is the scaling curve as a table. Efficiency compares the
// mean per-replica throughput with one replica alone.
func ScalingTable(levels []ScalingLevel) eval.Table {
	t := eval.Table{Header: []string{"replicas", "requests", "weighted tok/s (total)", "per replica (mean)",
		"per replica (min)", "efficiency vs 1 replica", "p50 TTFT s", "p99 TTFT s", "p50 TPOT ms", "p99 TPOT ms",
		"failed", "CPU performance % of nominal"}}
	one := math.NaN()
	for _, lv := range levels {
		mean := lv.Pooled.WeightedTokensPerSec / float64(lv.Replicas)
		lo := math.Inf(1)
		for _, r := range lv.PerReplica {
			lo = math.Min(lo, r.WeightedTokensPerSec)
		}
		if lv.Replicas == 1 {
			one = mean
		}
		t.Rows = append(t.Rows, []string{
			strconv.Itoa(lv.Replicas), strconv.Itoa(lv.Pooled.Requests),
			eval.FormatValue(lv.Pooled.WeightedTokensPerSec), eval.FormatValue(mean), eval.FormatValue(lo),
			eval.FormatValue(mean / one),
			eval.FormatValue(lv.Pooled.TTFTP50), eval.FormatValue(lv.Pooled.TTFTP99),
			eval.FormatValue(1000 * lv.Pooled.TPOTP50), eval.FormatValue(1000 * lv.Pooled.TPOTP99),
			failedCell(lv.Pooled), eval.FormatValue(lv.CPUPerformancePct),
		})
	}
	return t
}

// WriteScalingCSV writes one row per replica per level, plus the pooled
// row (replica "all").
func WriteScalingCSV(path string, levels []ScalingLevel) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"replicas", "replica", "requests", "completed_in_window", "failed", "weighted_tok_per_s",
		"output_tok_per_s", "p50_ttft_s", "p99_ttft_s", "p50_tpot_s", "p99_tpot_s", "cpu_performance_pct"})
	row := func(k int, name string, lv Level, perf float64) {
		_ = w.Write([]string{strconv.Itoa(k), name, strconv.Itoa(lv.Requests), strconv.Itoa(lv.Completed),
			strconv.Itoa(lv.Failed), ftoa(lv.WeightedTokensPerSec), ftoa(lv.OutputTokensPerSec),
			ftoa(lv.TTFTP50), ftoa(lv.TTFTP99), ftoa(lv.TPOTP50), ftoa(lv.TPOTP99), ftoa(perf)})
	}
	for _, lv := range levels {
		for i, r := range lv.PerReplica {
			row(lv.Replicas, strconv.Itoa(i), r, lv.CPUPerformancePct)
		}
		row(lv.Replicas, "all", lv.Pooled, lv.CPUPerformancePct)
	}
	w.Flush()
	return errors.Join(w.Error(), f.Close())
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
