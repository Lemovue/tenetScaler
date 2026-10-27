package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"tenantscaler/internal/control"
	"tenantscaler/internal/metrics"
)

// DefaultDecisionsLimit is how many records GET /decisions returns when the
// request has no limit parameter.
const DefaultDecisionsLimit = 100

// runnerMetrics are the controller's Prometheus series, on a private
// registry served by the Runner's /metrics.
type runnerMetrics struct {
	reg        *prometheus.Registry
	cycles     prometheus.Counter
	failStatic prometheus.Counter
	errors     *prometheus.CounterVec
	desired    prometheus.Gauge
	pool       prometheus.Gauge
	grant      *prometheus.GaugeVec
	tenants    map[string]bool // label values currently exported by grant
}

func newRunnerMetrics() *runnerMetrics {
	m := &runnerMetrics{
		reg: metrics.NewRegistry(),
		cycles: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tenantscaler_controller_cycles_total", Help: "Control cycles run.",
		}),
		failStatic: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tenantscaler_controller_failstatic_total",
			Help: "Control cycles that failed static (metrics or pool status unavailable).",
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tenantscaler_controller_errors_total", Help: "Cycle stage failures by stage.",
		}, []string{"stage"}),
		desired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tenantscaler_controller_desired_replicas",
			Help: "Sum of grants clamped to [minReplicas, capacity], before stabilization, of the last cycle that did not fail static.",
		}),
		pool: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tenantscaler_controller_pool_replicas",
			Help: "Stabilized pool size the controller last applied (fail-static cycles hold it).",
		}),
		grant: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tenantscaler_controller_tenant_grant", Help: "Replicas granted to each tenant by the last cycle that did not fail static.",
		}, []string{"tenant"}),
		tenants: map[string]bool{},
	}
	m.reg.MustRegister(m.cycles, m.failStatic, m.errors, m.desired, m.pool, m.grant)
	for _, s := range []string{StageTenants, StageMetrics, StagePoolStatus, StageActuation, StagePublish, StageDecisionLog} {
		m.errors.WithLabelValues(s) // export zeros so rate() works from the start
	}
	return m
}

// observe updates the series after one cycle. A fail-static cycle changes
// nothing, so it leaves the replica and grant gauges at the last applied
// decision. Grants of tenants that left the decision are removed.
func (m *runnerMetrics) observe(d control.Decision, errs []stageError) {
	m.cycles.Inc()
	for _, e := range errs {
		m.errors.WithLabelValues(e.stage).Inc()
	}
	if d.FailStatic {
		m.failStatic.Inc()
		return
	}
	m.desired.Set(float64(d.DesiredReplicas))
	m.pool.Set(float64(d.PoolReplicas))
	seen := make(map[string]bool, len(d.Records))
	for _, rec := range d.Records {
		if rec.Tenant == control.PoolRecordTenant {
			continue
		}
		seen[rec.Tenant] = true
		m.grant.WithLabelValues(rec.Tenant).Set(float64(rec.Final.Grant))
	}
	for t := range m.tenants {
		if !seen[t] {
			m.grant.DeleteLabelValues(t)
		}
	}
	m.tenants = seen
}

// Handler serves the controller API:
//
//	GET /decisions?limit=&tenant=  decision records, newest first (JSON array;
//	                               limit defaults to DefaultDecisionsLimit,
//	                               0 means all)
//	GET /status                    Status of the last cycle (JSON)
//	GET /healthz                   {"status":"ok"}
//	GET /metrics                   Prometheus metrics
func (r *Runner) Handler() http.Handler {
	r.once.Do(r.init)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /decisions", r.handleDecisions)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.Status())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", metrics.Handler(r.metrics.reg))
	return mux
}

func (r *Runner) handleDecisions(w http.ResponseWriter, req *http.Request) {
	limit := DefaultDecisionsLimit
	if s := req.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("limit must be a non-negative integer, got %q", s),
			})
			return
		}
		limit = n
	}
	if r.Log == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, r.Log.List(limit, req.URL.Query().Get("tenant")))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		code = http.StatusInternalServerError
		data, _ = json.Marshal(map[string]string{"error": "encode response: " + err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}
