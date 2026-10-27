package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/model"
)

// DefaultPrometheusWindow is the rate window PrometheusSource uses when its
// Window is zero. rate() needs several scrapes inside the window, so it is
// longer than a typical control cycle.
const DefaultPrometheusWindow = time.Minute

// PrometheusSource reads per-tenant observations from the gateways' metrics
// through the Prometheus HTTP API (instant queries). A failed query fails the
// whole observation, and so does a missing tenantscaler_gateway_queue_length
// series: the gateway creates it at 0 for every tenant, so its absence means
// Prometheus is not scraping the gateway, not that demand is zero. A tenant
// missing from one of the other queries, or a NaN/Inf sample, reads as 0.
type PrometheusSource struct {
	// BaseURL is the Prometheus server, e.g. "http://127.0.0.1:9090".
	BaseURL string
	// Window is the range of rate()/increase(); zero means
	// DefaultPrometheusWindow.
	Window time.Duration
	// HTTP is the client to use; nil means a client with a 5 s timeout.
	HTTP *http.Client
}

// promTenant accumulates one tenant's query results.
type promTenant struct {
	weighted, arrivals, queue, inflight float64
	met, completed, shed                float64
	p99                                 float64
	hasP99                              bool
}

// promQuery is one instant query and where its value goes.
type promQuery struct {
	expr string
	set  func(t *promTenant, v float64)
	// required means an answer without any tenant sample is an error.
	required bool
}

// promQueries returns the queries of DESIGN.md section 12 for window w.
func promQueries(w time.Duration) []promQuery {
	r := promRange(w)
	return []promQuery{
		{expr: "sum by (tenant)(rate(tenantscaler_gateway_arrival_weighted_tokens_total[" + r + "]))",
			set: func(t *promTenant, v float64) { t.weighted = v }},
		{expr: "sum by (tenant)(rate(tenantscaler_gateway_arrivals_total[" + r + "]))",
			set: func(t *promTenant, v float64) { t.arrivals = v }},
		{expr: "sum by (tenant)(tenantscaler_gateway_queue_length)",
			set: func(t *promTenant, v float64) { t.queue = v }, required: true},
		{expr: "sum by (tenant)(tenantscaler_gateway_inflight)",
			set: func(t *promTenant, v float64) { t.inflight = v }},
		{expr: `sum by (tenant)(increase(tenantscaler_gateway_completed_total{slo="met"}[` + r + "]))",
			set: func(t *promTenant, v float64) { t.met = v }},
		{expr: "sum by (tenant)(increase(tenantscaler_gateway_completed_total[" + r + "]))",
			set: func(t *promTenant, v float64) { t.completed = v }},
		{expr: "histogram_quantile(0.99, sum by (tenant, le)(rate(tenantscaler_gateway_ttft_seconds_bucket[" + r + "])))",
			set: func(t *promTenant, v float64) { t.p99, t.hasP99 = v, true }},
		{expr: "sum by (tenant)(increase(tenantscaler_gateway_shed_total[" + r + "]))",
			set: func(t *promTenant, v float64) { t.shed = v }},
	}
}

// promRange formats a duration as a PromQL range ("15s", "1500ms"); PromQL
// does not accept Go's fractional forms such as "1.5s".
func promRange(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(max(1, d.Milliseconds()), 10) + "ms"
}

// Observe runs every query and assembles one observation per tenant seen in
// any of them, sorted by tenant name. It fails when the gateway's queue
// series are missing (see PrometheusSource).
func (s PrometheusSource) Observe(ctx context.Context) (model.Observation, error) {
	if s.BaseURL == "" {
		return model.Observation{}, fmt.Errorf("prometheus source: no base URL")
	}
	w := s.Window
	if w <= 0 {
		w = DefaultPrometheusWindow
	}
	tenants := map[string]*promTenant{}
	for _, q := range promQueries(w) {
		samples, err := s.query(ctx, q.expr)
		if err != nil {
			return model.Observation{}, err
		}
		if q.required && len(samples) == 0 {
			return model.Observation{}, fmt.Errorf("prometheus source: query %q returned no tenant series: is Prometheus scraping the gateway?", q.expr)
		}
		for name, v := range samples {
			t := tenants[name]
			if t == nil {
				t = &promTenant{}
				tenants[name] = t
			}
			q.set(t, v)
		}
	}
	obs := model.Observation{OK: true, Tenants: make([]model.TenantObservation, 0, len(tenants))}
	for name, t := range tenants {
		obs.Tenants = append(obs.Tenants, t.observation(name))
	}
	sort.Slice(obs.Tenants, func(i, j int) bool { return obs.Tenants[i].Tenant < obs.Tenants[j].Tenant })
	return obs, nil
}

// observation converts the accumulated samples. Counts are rounded because
// increase() extrapolates. SLO data needs at least one completed request
// and a p99, since the controller's slack is computed from the p99.
func (t *promTenant) observation(name string) model.TenantObservation {
	o := model.TenantObservation{
		Tenant:                      name,
		ArrivalWeightedTokensPerSec: t.weighted,
		ArrivalRequestsPerSec:       t.arrivals,
		QueueLength:                 roundCount(t.queue),
		InFlight:                    roundCount(t.inflight),
		Completed:                   roundCount(t.completed),
		Shed:                        roundCount(t.shed),
	}
	if o.Completed > 0 && t.hasP99 {
		o.HasSLOData = true
		o.SLOAttainment = math.Min(1, math.Max(0, t.met/t.completed))
		o.P99TTFTSeconds = t.p99
	}
	return o
}

func roundCount(v float64) int { return int(math.Round(math.Max(0, v))) }

// promResponse is the Prometheus HTTP API envelope of an instant query.
type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"` // [unix time, "value"]
		} `json:"result"`
	} `json:"data"`
}

// query runs one instant query and returns the finite value per tenant
// label. Samples without a tenant label, NaN and Inf values are dropped.
func (s PrometheusSource) query(ctx context.Context, expr string) (map[string]float64, error) {
	target := strings.TrimRight(s.BaseURL, "/") + "/api/v1/query?query=" + url.QueryEscape(expr)
	var resp promResponse
	if err := doJSON(ctx, s.HTTP, http.MethodGet, target, nil, &resp); err != nil {
		return nil, fmt.Errorf("prometheus source: query %q: %w", expr, err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus source: query %q: status %q: %s: %s", expr, resp.Status, resp.ErrorType, resp.Error)
	}
	if resp.Data.ResultType != "vector" {
		return nil, fmt.Errorf("prometheus source: query %q: result type %q, want vector", expr, resp.Data.ResultType)
	}
	out := make(map[string]float64, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		name := r.Metric["tenant"]
		if name == "" || len(r.Value) != 2 {
			continue
		}
		str, ok := r.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("prometheus source: query %q: tenant %q: sample value %v is not a string", expr, name, r.Value[1])
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return nil, fmt.Errorf("prometheus source: query %q: tenant %q: %w", expr, name, err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[name] = v
	}
	return out, nil
}
