package experiment

import (
	"fmt"
	"io"
	"math"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// ProfileOptions tunes Profile; zero fields take the defaults.
type ProfileOptions struct {
	// Duration of each probe run (default 300 s).
	Duration time.Duration
	// Seeds per probe (default 3); a rate meets the SLO only if every
	// seed does.
	Seeds    int
	BaseSeed int64
	// Precision is the relative width at which the search stops
	// (default 0.01); MaxProbes bounds the number of probes (default 40).
	Precision float64
	MaxProbes int
}

// ProfilePoint is one probed load.
type ProfilePoint struct {
	WeightedTokensPerSec, RequestsPerSec float64
	// P99TTFTSeconds and P99TPOTSeconds are the worst p99 over the seeds.
	P99TTFTSeconds, P99TPOTSeconds float64
	Meets                          bool
}

// ProfileResult is the SIMULATED per-replica capacity.
type ProfileResult struct {
	// CapacityTokensPerSec is C: the largest weighted token rate one
	// replica serves with p99 TTFT and p99 TPOT within the SLO.
	CapacityTokensPerSec, RequestsPerSec float64
	// MeanWeightedTokens is the mean input + 2 x output tokens per request
	// of the default length distributions.
	MeanWeightedTokens float64
	// SLO is the strictest tenant SLO of the config.
	SLO             model.SLO
	SlotsPerReplica int
	Engine          config.EngineSpec
	Configured      float64 // the config's current replicaCapacityTokensPerSec
	Points          []ProfilePoint
}

// profileTenant names the single tenant of a profile run.
const profileTenant = "profile"

// Profile measures C on the simulated engine: one tenant, one replica,
// the fair gateway with its dispatch window, steady Poisson load with the
// default scenario's length distributions (standard priority, so nothing
// is shed), binary search on the weighted token rate until p99 TTFT and
// p99 TPOT stop meeting the strictest tenant SLO.
func Profile(cfg config.Config, opt ProfileOptions) (ProfileResult, error) {
	if err := cfg.Validate(); err != nil {
		return ProfileResult{}, err
	}
	opt = opt.withDefaults()
	res := ProfileResult{SLO: strictestSLO(cfg.Tenants), Engine: cfg.Engine, SlotsPerReplica: cfg.SlotsPerReplica(),
		Configured: cfg.Controller.ReplicaCapacityTokensPerSec}
	pcfg := cfg
	pcfg.Pool.CapacityReplicas, pcfg.Pool.MinReplicas = 1, 1
	pcfg.Tenants = []model.TenantSpec{{Name: profileTenant, GuaranteedReplicas: 1, MaxReplicas: 1, SLO: res.SLO}}
	pattern := workload.DefaultScenario().Tenants[0]
	pattern.Tenant, pattern.Kind, pattern.CV = profileTenant, workload.KindSteady, 1
	pattern.Mix = workload.PriorityMix{Standard: 1}
	res.MeanWeightedTokens = meanWeightedTokens(pattern, opt.BaseSeed)

	probe := func(rate float64) (ProfilePoint, error) {
		pt := ProfilePoint{WeightedTokensPerSec: rate, RequestsPerSec: rate / res.MeanWeightedTokens, Meets: true}
		p := pattern
		p.BaseRPS, p.PeakRPS = pt.RequestsPerSec, pt.RequestsPerSec
		sc := workload.Scenario{Name: "profile", Duration: opt.Duration, Tenants: []workload.TenantPattern{p}}
		for k := range opt.Seeds {
			d, err := sim.Run(sim.Options{Setup: sim.SetupFull, Config: pcfg, Scenario: sc, Seed: opt.BaseSeed + int64(k), InitialReplicas: 1})
			if err != nil {
				return pt, err
			}
			m := eval.Compute(d)
			ttft, tpot := m.P99TTFT[profileTenant], m.P99TPOT[profileTenant]
			pt.P99TTFTSeconds = math.Max(pt.P99TTFTSeconds, ttft)
			pt.P99TPOTSeconds = math.Max(pt.P99TPOTSeconds, tpot)
			if m.Failed > 0 || m.Shed > 0 || !(ttft <= res.SLO.TTFTSeconds) || !(tpot <= res.SLO.TPOT().Seconds()) {
				pt.Meets = false
			}
		}
		res.Points = append(res.Points, pt)
		return pt, nil
	}

	lo, hi := 0.0, res.MeanWeightedTokens // start at 1 request/s
	for {
		pt, err := probe(hi)
		if err != nil {
			return res, err
		}
		if !pt.Meets {
			break
		}
		if len(res.Points) >= opt.MaxProbes {
			return res, fmt.Errorf("profile: still within the SLO at %.0f weighted tokens/s after %d probes", hi, len(res.Points))
		}
		lo, hi = hi, 2*hi
	}
	for hi-lo > opt.Precision*hi && len(res.Points) < opt.MaxProbes {
		mid := (lo + hi) / 2
		pt, err := probe(mid)
		if err != nil {
			return res, err
		}
		if pt.Meets {
			lo = mid
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return res, fmt.Errorf("profile: no probed load met the SLO (lowest probe %.1f weighted tokens/s)", hi)
	}
	res.CapacityTokensPerSec, res.RequestsPerSec = lo, lo/res.MeanWeightedTokens
	return res, nil
}

func (o ProfileOptions) withDefaults() ProfileOptions {
	if o.Duration <= 0 {
		o.Duration = 300 * time.Second
	}
	if o.Seeds <= 0 {
		o.Seeds = 3
	}
	if !(o.Precision > 0) {
		o.Precision = 0.01
	}
	if o.MaxProbes <= 0 {
		o.MaxProbes = 40
	}
	return o
}

// strictestSLO combines the tightest TTFT and TPOT bounds of the tenants.
func strictestSLO(tenants []model.TenantSpec) model.SLO {
	s := tenants[0].SLO
	for _, t := range tenants[1:] {
		s.TTFTSeconds = math.Min(s.TTFTSeconds, t.SLO.TTFTSeconds)
		s.TPOTMillis = math.Min(s.TPOTMillis, t.SLO.TPOTMillis)
		s.TargetFraction = math.Max(s.TargetFraction, t.SLO.TargetFraction)
	}
	return s
}

// meanWeightedTokens estimates the mean weighted tokens per request of a
// pattern's length distributions from a large generated sample.
func meanWeightedTokens(p workload.TenantPattern, seed int64) float64 {
	p.BaseRPS, p.PeakRPS = 20, 20
	specs := workload.Generate(workload.Scenario{Name: "lengths", Duration: 1000 * time.Second, Tenants: []workload.TenantPattern{p}}, seed)
	var sum float64
	for _, s := range specs {
		sum += model.WeightedTokens(float64(s.InputTokens), float64(s.OutputTokens))
	}
	return sum / float64(max(len(specs), 1))
}

// WriteText prints the result for the profile command.
func (r ProfileResult) WriteText(w io.Writer) error {
	t := eval.Table{Header: []string{"weighted tok/s", "req/s", "p99 TTFT s", "p99 TPOT ms", "meets SLO"}}
	for _, p := range r.Points {
		t.Rows = append(t.Rows, []string{eval.FormatValue(p.WeightedTokensPerSec), eval.FormatValue(p.RequestsPerSec),
			eval.FormatValue(p.P99TTFTSeconds), eval.FormatValue(1000 * p.P99TPOTSeconds), fmt.Sprint(p.Meets)})
	}
	if _, err := fmt.Fprintf(w, "SIMULATED engine profile (one replica, fair gateway, dispatch window %d, engine %s + %s/token prefill, %s/token decode, %d seqs)\n"+
		"SLO: p99 TTFT <= %gs and p99 TPOT <= %gms; mean request = %.0f weighted tokens\n\n",
		r.SlotsPerReplica, r.Engine.PrefillOverhead, r.Engine.PrefillPerToken, r.Engine.InterTokenLatency, r.Engine.MaxNumSeqs,
		r.SLO.TTFTSeconds, r.SLO.TPOTMillis, r.MeanWeightedTokens); err != nil {
		return err
	}
	if err := t.WriteMarkdown(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nC (simulated) = %.0f weighted tokens/s per replica (%.2f req/s). The config uses replicaCapacityTokensPerSec: %g.\n"+
		"These numbers describe the simulated engine only; measure real replicas separately.\n",
		r.CapacityTokensPerSec, r.RequestsPerSec, r.Configured)
	return err
}
