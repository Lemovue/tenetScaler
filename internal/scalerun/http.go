package scalerun

import (
	"encoding/json"
	"math"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"tenantscaler/internal/metrics"
)

// Status is the body of GET /status.
type Status struct {
	Algo         string  `json:"algo"`
	CycleSeconds float64 `json:"cycleSeconds"`
	Cycles       int64   `json:"cycles"`
	Errors       int64   `json:"errors"`
	// Last is the most recent decision; nil before the first sync.
	Last *Decision `json:"last"`
}

// Handler serves GET /status (Status), GET /healthz and GET /metrics
// (Prometheus: tenantscaler_baseline_*).
func (r *Runner) Handler() http.Handler {
	r.once.Do(r.setup)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		st := Status{Algo: r.Algo, CycleSeconds: r.Cycle.Seconds(), Cycles: r.cycles, Errors: r.errs}
		if r.has {
			last := r.last
			st.Last = &last
		}
		r.mu.Unlock()
		writeJSON(w, st)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", metrics.Handler(r.reg))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) // the client may be gone; nothing to do then
}

// runnerMetrics are the runner's Prometheus series.
type runnerMetrics struct {
	cycles, errors prometheus.Counter
	scaleEvents    *prometheus.CounterVec
	metric         prometheus.Gauge
	desired        prometheus.Gauge
}

// setup creates the registry and series (once, lazily, because a Runner is
// configured as a struct literal).
func (r *Runner) setup() {
	r.reg = metrics.NewRegistry()
	labels := prometheus.Labels{"algo": r.Algo}
	m := runnerMetrics{
		cycles: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tenantscaler_baseline_cycles_total", Help: "Autoscaler syncs run.", ConstLabels: labels,
		}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tenantscaler_baseline_errors_total", Help: "Autoscaler syncs that failed.", ConstLabels: labels,
		}),
		scaleEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tenantscaler_baseline_scale_events_total", Help: "Pool resizes by direction.", ConstLabels: labels,
		}, []string{"direction"}),
		metric: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "tenantscaler_baseline_metric",
			Help:        "Last autoscaler input: queue length (keda) or slot occupancy (hpa); NaN when unavailable.",
			ConstLabels: labels,
		}),
		desired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tenantscaler_baseline_desired_replicas", Help: "Last desired replica count.", ConstLabels: labels,
		}),
	}
	r.reg.MustRegister(m.cycles, m.errors, m.scaleEvents, m.metric, m.desired)
	r.metrics = m
}

// observe updates the series after a sync.
func (m runnerMetrics) observe(d Decision, err error) {
	m.cycles.Inc()
	if err != nil {
		m.errors.Inc()
	}
	if d.MetricOK {
		m.metric.Set(d.Metric)
	} else {
		m.metric.Set(math.NaN())
	}
	if d.PoolOK {
		m.desired.Set(float64(d.Desired))
	}
	if d.Scaled {
		dir := "up"
		if d.Desired < d.Current {
			dir = "down"
		}
		m.scaleEvents.WithLabelValues(dir).Inc()
	}
}
