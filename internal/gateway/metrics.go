package gateway

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
)

// Failure reasons in tenantscaler_gateway_failed_total: requests that were
// admitted but did not complete.
const (
	// FailUpstream: the replica could not be reached, answered with an
	// error status, or broke the stream off.
	FailUpstream = "upstream"
	// FailDispatchTimeout: no replica slot within Config.DispatchTimeout.
	FailDispatchTimeout = "dispatch-timeout"
	// FailCanceled: the client went away before the response was complete.
	FailCanceled = "canceled"
	// FailShutdown: the gateway was closed while the request was active.
	FailShutdown = "shutdown"
	// FailTenantRemoved: in partition mode, the tenant was removed (see
	// Gateway.SetTenants) while its request waited; it owns no endpoints
	// any more.
	FailTenantRemoved = "tenant-removed"
)

// SLO outcome labels of tenantscaler_gateway_completed_total.
const (
	sloMet    = "met"
	sloMissed = "missed"
)

// ttftBuckets resolve the 0.5-5 s range of typical TTFT SLOs finely, so
// histogram_quantile(0.99, ...) is close to the exact p99 near the SLO.
var ttftBuckets = []float64{0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10, 15, 30, 60, 120}

// tpotBuckets cover GPU-class (tens of ms) to CPU-class (hundreds of ms)
// decode speeds.
var tpotBuckets = []float64{0.005, 0.01, 0.02, 0.03, 0.05, 0.075, 0.1, 0.125, 0.15, 0.2, 0.25, 0.3, 0.5, 1}

// policyBuckets cover in-process (sub-millisecond) to cross-cluster pushes.
var policyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// gatewayMetrics holds the gateway's Prometheus collectors. The names are
// part of the contract with the controller's PrometheusSource (DESIGN.md
// section 12).
type gatewayMetrics struct {
	arrivals      *prometheus.CounterVec
	arrivalTokens *prometheus.CounterVec
	shed          *prometheus.CounterVec
	completed     *prometheus.CounterVec
	failed        *prometheus.CounterVec
	outputTokens  *prometheus.CounterVec
	ttft          *prometheus.HistogramVec
	tpot          *prometheus.HistogramVec
	queue         *prometheus.GaugeVec
	inflight      *prometheus.GaugeVec
	policyVersion prometheus.Gauge
	policyLatency prometheus.Histogram
	endpoints     prometheus.Gauge
}

func newGatewayMetrics(reg prometheus.Registerer) *gatewayMetrics {
	const ns, sub = "tenantscaler", "gateway"
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Subsystem: sub, Name: name, Help: help}, labels)
	}
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Subsystem: sub, Name: name, Help: help}, labels)
	}
	histogram := func(name, help string, buckets []float64) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help, Buckets: buckets,
		}, []string{"tenant"})
	}
	m := &gatewayMetrics{
		arrivals: counter("arrivals_total",
			"Requests that arrived, admitted or shed, by tenant and priority class.", "tenant", "priority"),
		arrivalTokens: counter("arrival_weighted_tokens_total",
			"Weighted tokens (input + 2 x max_tokens) of arriving requests, admitted or shed.", "tenant"),
		shed: counter("shed_total",
			"Requests rejected with 429, by tenant and reason.", "tenant", "reason"),
		completed: counter("completed_total",
			"Requests completed, by tenant and whether they met the tenant's TTFT and TPOT SLO. "+
				"Admitted requests that failed (dispatch timeout or upstream failure) count as missed.", "tenant", "slo"),
		failed: counter("failed_total",
			"Admitted requests that did not complete, by tenant and reason.", "tenant", "reason"),
		outputTokens: counter("output_tokens_total",
			"Output tokens of completed requests, as reported by the replica (usage) or counted from the stream.", "tenant"),
		ttft: histogram("ttft_seconds",
			"Time to first token measured at the gateway, from arrival (including gateway queueing). "+
				"A failed request that relayed no token contributes the time until it failed, a lower bound.", ttftBuckets),
		tpot: histogram("tpot_seconds",
			"Mean time per output token after the first, measured at the gateway (streamed requests only).", tpotBuckets),
		queue: gauge("queue_length",
			"Requests waiting in the gateway queue for a replica slot.", "tenant"),
		inflight: gauge("inflight",
			"Requests dispatched to a replica and not yet finished.", "tenant"),
		policyVersion: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Subsystem: sub, Name: "policy_version",
			Help: "Version of the tenant policy in force (0 = the equal-weight fail-safe policy)."}),
		policyLatency: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Subsystem: sub, Name: "policy_propagation_seconds",
			Help: "Policy propagation latency: receive time minus the policy's issuedAt.", Buckets: policyBuckets}),
		endpoints: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Subsystem: sub, Name: "endpoints",
			Help: "Ready replica endpoints the gateway currently dispatches to."}),
	}
	reg.MustRegister(m.arrivals, m.arrivalTokens, m.shed, m.completed, m.failed, m.outputTokens,
		m.ttft, m.tpot, m.queue, m.inflight, m.policyVersion, m.policyLatency, m.endpoints)
	return m
}

// initTenant creates every series of a tenant at zero, so rate() and
// increase() work from the first scrape instead of missing the first event.
func (m *gatewayMetrics) initTenant(tenant string) {
	for _, p := range []model.Priority{model.PriorityCritical, model.PriorityStandard, model.PrioritySheddable} {
		m.arrivals.WithLabelValues(tenant, p.Class())
	}
	m.arrivalTokens.WithLabelValues(tenant)
	for _, r := range []string{fairq.ReasonOverShare, fairq.ReasonDeadlineUnreachable, fairq.ReasonSaturated, fairq.ReasonQueueFull} {
		m.shed.WithLabelValues(tenant, r)
	}
	m.completed.WithLabelValues(tenant, sloMet)
	m.completed.WithLabelValues(tenant, sloMissed)
	for _, r := range []string{FailUpstream, FailDispatchTimeout, FailCanceled, FailShutdown, FailTenantRemoved} {
		m.failed.WithLabelValues(tenant, r)
	}
	m.outputTokens.WithLabelValues(tenant)
	m.ttft.WithLabelValues(tenant)
	m.tpot.WithLabelValues(tenant)
	m.queue.WithLabelValues(tenant)
	m.inflight.WithLabelValues(tenant)
}

// arrival counts one arriving request.
func (m *gatewayMetrics) arrival(tenant string, p model.Priority, weighted float64) {
	m.arrivals.WithLabelValues(tenant, p.Class()).Inc()
	m.arrivalTokens.WithLabelValues(tenant).Add(weighted)
}

// complete records one completed request. TPOT is observed only for
// streamed requests, where it is measurable.
func (m *gatewayMetrics) complete(tenant string, ttft, tpot time.Duration, tokens int, met, streamed bool) {
	slo := sloMissed
	if met {
		slo = sloMet
	}
	m.completed.WithLabelValues(tenant, slo).Inc()
	m.outputTokens.WithLabelValues(tenant).Add(float64(tokens))
	m.ttft.WithLabelValues(tenant).Observe(ttft.Seconds())
	if streamed && tokens > 1 {
		m.tpot.WithLabelValues(tenant).Observe(tpot.Seconds())
	}
}

// missed records an admitted request that failed (see Gateway.recordFailure)
// as a completed request that missed its SLO, with its TTFT or a lower bound.
func (m *gatewayMetrics) missed(tenant string, ttft time.Duration) {
	m.completed.WithLabelValues(tenant, sloMissed).Inc()
	m.ttft.WithLabelValues(tenant).Observe(ttft.Seconds())
}
