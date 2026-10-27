// Package scalerun runs a baseline autoscaler live: a KEDA queue-length
// scaler or a CPU-style HPA (package autoscale) that periodically reads a
// replica pool's status and resizes it, the way the KEDA and HPA
// controllers resize a Deployment in Kubernetes. It is the control plane of
// the baseline stacks TenantScaler is compared against (setups S1 and S2),
// on the simulated pool or on real llama.cpp replicas.
//
// KEDA's metric is the total queue length at sync time: the requests
// waiting inside the replicas (pool status Waiting;
// llamacpp:requests_deferred on a real replica) plus, when a gateway is
// configured, the gateway's queued count.
//
// The HPA's metric is slot occupancy, the stand-in for CPU utilisation,
// averaged over the cycle like a CPU rate: Run samples the pool every
// SampleEvery (1 s, as the simulator does), and each sample is the busy
// slots (Running) of ready replicas over MaxNumSeqs times the replicas the
// pool should have, so replicas still starting count as idle, as the
// Kubernetes replica calculator counts pods that are not ready on a
// scale-up.
package scalerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/pool"
)

// Autoscaler algorithms.
const (
	// AlgoKEDA scales on queue length (KEDA ScaledObject, AverageValue).
	AlgoKEDA = "keda"
	// AlgoHPA scales on slot occupancy (CPU-style HPA, Utilization).
	AlgoHPA = "hpa"
)

// Pool is the replica pool the runner resizes. *pool.Client implements it;
// wrap an in-process *pool.Pool with InProcess.
type Pool interface {
	Scale(ctx context.Context, n int) error
	Status(ctx context.Context) (pool.Status, error)
}

// defaultHTTP fetches gateway stats when Runner.HTTP is nil.
var defaultHTTP = &http.Client{Timeout: 5 * time.Second}

// Decision is the outcome of one autoscaler sync.
type Decision struct {
	Time time.Time `json:"time"`
	Algo string    `json:"algo"`
	// Metric is the autoscaler input (KEDA: total queued requests; HPA:
	// mean slot occupancy in [0, 1]); MetricOK is false when it could not
	// be measured, in which case nothing changed.
	Metric   float64 `json:"metric"`
	MetricOK bool    `json:"metricOk"`
	// Samples is the number of occupancy samples averaged into an HPA
	// Metric (the samples since the previous sync, this sync's included).
	Samples int `json:"samples,omitempty"`
	// PoolOK is false when the pool status could not be read; Current,
	// Desired, Ready and Starting are zero then.
	PoolOK bool `json:"poolOk"`
	// Current is the pool's desired count before the sync, Desired the
	// autoscaler's answer; Scaled reports that the pool was resized.
	Current int  `json:"current"`
	Desired int  `json:"desired"`
	Scaled  bool `json:"scaled"`
	// Ready and Starting describe the pool when the sync read it.
	Ready    int    `json:"ready"`
	Starting int    `json:"starting"`
	Reason   string `json:"reason"`
	Error    string `json:"error,omitempty"`
}

// Runner is the live baseline autoscaler loop. Configure the exported
// fields, then call Run (or RunOnce); do not change them afterwards. It is
// safe for concurrent use.
type Runner struct {
	// Algo is AlgoKEDA or AlgoHPA; the matching scaler must be set.
	Algo string
	KEDA *autoscale.KEDA
	HPA  *autoscale.HPA
	// Pool is the pool to read and resize.
	Pool Pool
	// GatewayStatsURL, when set, is the gateway's stats URL (e.g.
	// "http://127.0.0.1:8080/stats"); its JSON field "queued" is added to
	// the KEDA queue length.
	GatewayStatsURL string
	// HTTP fetches the gateway stats; nil means a client with a 5 s timeout.
	HTTP *http.Client
	// MaxNumSeqs is the slots per replica (engine maxNumSeqs, or llama.cpp
	// parallel slots); required for the HPA.
	MaxNumSeqs int
	// Cycle is the sync period of Run (15 s in Kubernetes).
	Cycle time.Duration
	// SampleEvery is how often Run samples slot occupancy between HPA
	// syncs; 0 means DefaultSampleEvery. KEDA does not sample.
	SampleEvery time.Duration
	// Clock returns the current time; nil means time.Now.
	Clock func() time.Time
	// Logger receives scaling events and errors; nil means slog.Default().
	Logger *slog.Logger
	// OnDecision, when set, is called after every sync (from the goroutine
	// running RunOnce), e.g. to collect action times for evaluation.
	OnDecision func(Decision)

	once    sync.Once
	reg     *prometheus.Registry
	metrics runnerMetrics

	stepMu sync.Mutex // serialises syncs; the scalers are not goroutine-safe

	winMu sync.Mutex
	win   occupancyWindow // HPA samples since the last sync

	mu     sync.Mutex
	last   Decision
	has    bool
	cycles int64
	errs   int64
}

// Validate checks the configuration (Cycle is checked by Run).
func (r *Runner) Validate() error {
	switch r.Algo {
	case AlgoKEDA:
		if r.KEDA == nil {
			return errors.New("scalerun: algo keda needs a KEDA scaler")
		}
	case AlgoHPA:
		if r.HPA == nil {
			return errors.New("scalerun: algo hpa needs an HPA")
		}
		if r.MaxNumSeqs < 1 {
			return fmt.Errorf("scalerun: algo hpa needs MaxNumSeqs >= 1, got %d", r.MaxNumSeqs)
		}
	default:
		return fmt.Errorf("scalerun: algo must be %q or %q, got %q", AlgoKEDA, AlgoHPA, r.Algo)
	}
	if r.Pool == nil {
		return errors.New("scalerun: Pool is required")
	}
	return nil
}

// Run syncs once immediately and then every Cycle until ctx is done; it
// returns nil then. Between HPA syncs it samples slot occupancy every
// SampleEvery. Sync errors are logged and kept in Last, not returned; an
// invalid configuration (including Cycle <= 0) is returned at once.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Cycle <= 0 {
		return fmt.Errorf("scalerun: cycle must be > 0, got %v", r.Cycle)
	}
	every, err := r.sampleEvery()
	if err != nil {
		return err
	}
	tick := time.NewTicker(r.Cycle)
	defer tick.Stop()
	var sample <-chan time.Time
	if r.Algo == AlgoHPA && every < r.Cycle {
		st := time.NewTicker(every)
		defer st.Stop()
		sample = st.C
	}
	for ctx.Err() == nil { // checked first: select picks at random when both are ready
		_, _ = r.RunOnce(ctx) // errors are recorded in the decision and logged
		r.sampleUntilSync(ctx, tick.C, sample)
	}
	return nil
}

// sampleUntilSync takes occupancy samples on sample until the next sync
// tick or until ctx is done.
func (r *Runner) sampleUntilSync(ctx context.Context, sync, sample <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sync:
			return
		case <-sample:
			if err := r.Sample(ctx); err != nil && ctx.Err() == nil {
				r.logger().Debug("scalerun: occupancy sample failed", "algo", r.Algo, "err", err)
			}
		}
	}
}

// RunOnce performs one sync: read the pool status (and the gateway queue
// for KEDA), compute the metric, step the autoscaler from the pool's
// desired count and scale the pool when the answer differs. The decision is
// recorded (Last, /status) and returned; err is non-nil when the status,
// the metric or the scale call failed.
func (r *Runner) RunOnce(ctx context.Context) (Decision, error) {
	r.once.Do(r.setup)
	if err := r.Validate(); err != nil {
		return Decision{}, err
	}
	r.stepMu.Lock()
	defer r.stepMu.Unlock()

	d := Decision{Time: r.now(), Algo: r.Algo}
	err := r.sync(ctx, &d)
	if err != nil {
		d.Error = err.Error()
		r.logger().Warn("scalerun: sync failed", "algo", r.Algo, "err", err)
	} else if d.Scaled {
		r.logger().Info("scalerun: scaled", "algo", r.Algo, "from", d.Current, "to", d.Desired, "reason", d.Reason)
	} else {
		r.logger().Debug("scalerun: hold", "algo", r.Algo, "replicas", d.Current, "reason", d.Reason)
	}
	r.record(d, err)
	if r.OnDecision != nil {
		r.OnDecision(d)
	}
	return d, err
}

// sync fills in d; see RunOnce.
func (r *Runner) sync(ctx context.Context, d *Decision) error {
	st, err := r.Pool.Status(ctx)
	if err != nil {
		d.Reason = "pool status unavailable: holding"
		return fmt.Errorf("pool status: %w", err)
	}
	d.PoolOK = true
	d.Current, d.Desired, d.Ready, d.Starting = st.Desired, st.Desired, st.Ready, st.Starting

	var metric float64
	var detail string
	if r.Algo == AlgoHPA {
		metric, d.Samples, detail = r.hpaMetric(st)
	} else if metric, detail, err = r.kedaMetric(ctx, st); err != nil {
		d.Reason = "metric unavailable: holding"
		return err
	}
	if math.IsNaN(metric) {
		d.Reason = detail + ": holding"
		return nil
	}
	d.Metric, d.MetricOK = metric, true
	if r.Algo == AlgoKEDA {
		d.Desired = r.KEDA.Step(d.Time, st.Desired, metric)
	} else {
		d.Desired = r.HPA.Step(d.Time, st.Desired, metric)
	}
	switch {
	case d.Desired > d.Current:
		d.Reason = fmt.Sprintf("%s: scale up %d -> %d", detail, d.Current, d.Desired)
	case d.Desired < d.Current:
		d.Reason = fmt.Sprintf("%s: scale down %d -> %d", detail, d.Current, d.Desired)
	default:
		d.Reason = fmt.Sprintf("%s: hold at %d", detail, d.Current)
		return nil
	}
	if err := r.Pool.Scale(ctx, d.Desired); err != nil {
		return fmt.Errorf("scale pool to %d: %w", d.Desired, err)
	}
	d.Scaled = true
	return nil
}

// kedaMetric returns KEDA's input, the total queue length, and a
// human-readable description.
func (r *Runner) kedaMetric(ctx context.Context, st pool.Status) (float64, string, error) {
	waiting := 0
	for _, rs := range st.Replicas {
		waiting += rs.Waiting
	}
	if r.GatewayStatsURL == "" {
		return float64(waiting), fmt.Sprintf("queue %d (waiting in replicas)", waiting), nil
	}
	queued, err := r.gatewayQueued(ctx)
	if err != nil {
		return 0, "", err
	}
	total := waiting + queued
	return float64(total), fmt.Sprintf("queue %d (%d waiting in replicas + %d in the gateway)", total, waiting, queued), nil
}

// gatewayQueued fetches the "queued" field of the gateway stats.
func (r *Runner) gatewayQueued(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.GatewayStatsURL, nil)
	if err != nil {
		return 0, fmt.Errorf("gateway stats: %w", err)
	}
	hc := r.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gateway stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("gateway stats: GET %s: %s: %s", r.GatewayStatsURL, resp.Status, msg)
	}
	var stats struct {
		Queued *int `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, fmt.Errorf("gateway stats: decode %s: %w", r.GatewayStatsURL, err)
	}
	if stats.Queued == nil {
		return 0, fmt.Errorf("gateway stats: %s has no \"queued\" field", r.GatewayStatsURL)
	}
	return *stats.Queued, nil
}

// Last returns the most recent decision; ok is false before the first sync.
func (r *Runner) Last() (d Decision, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.has
}

// record stores d as the last decision and updates the counters.
func (r *Runner) record(d Decision, err error) {
	r.mu.Lock()
	r.last, r.has = d, true
	r.cycles++
	if err != nil {
		r.errs++
	}
	r.mu.Unlock()
	r.metrics.observe(d, err)
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// inProcess adapts an in-process pool to the Pool interface.
type inProcess struct{ p *pool.Pool }

// InProcess wraps an in-process pool (local runs, tests) as a Pool.
func InProcess(p *pool.Pool) Pool { return inProcess{p} }

// Scale resizes the pool.
func (a inProcess) Scale(_ context.Context, n int) error { return a.p.Scale(n) }

// Status returns the pool status.
func (a inProcess) Status(context.Context) (pool.Status, error) { return a.p.Status(), nil }
