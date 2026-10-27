package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/model"
)

// promServer emulates GET /api/v1/query. results maps a substring of the
// query to its per-tenant sample values (strings, as Prometheus sends them);
// a query matching nothing gets an empty vector. Queries are recorded.
type promServer struct {
	mu      sync.Mutex
	results map[string]map[string]string
	queries []string
	// answer, when set, replaces the whole response body.
	answer string
}

func (p *promServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/query" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query().Get("query")
	p.mu.Lock()
	p.queries = append(p.queries, q)
	answer := p.answer
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if answer != "" {
		_, _ = w.Write([]byte(answer))
		return
	}
	type sample struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	result := []sample{}
	for key, byTenant := range p.results {
		if !strings.Contains(q, key) {
			continue
		}
		for tenant, v := range byTenant {
			metric := map[string]string{}
			if tenant != "" {
				metric["tenant"] = tenant
			}
			result = append(result, sample{Metric: metric, Value: []any{1761480000.123, v}})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
}

func TestPrometheusSourceObserve(t *testing.T) {
	ps := &promServer{results: map[string]map[string]string{
		"arrival_weighted_tokens_total": {"A": "1200.5", "B": "40"},
		"arrivals_total":                {"A": "6", "B": "0.5"},
		"queue_length":                  {"A": "3", "B": "0"},
		"inflight":                      {"A": "4", "B": "1"},
		`slo="met"`:                     {"A": "18.2"},
		"completed_total[":              {"A": "19.6", "B": "0", "": "99"},
		"histogram_quantile":            {"A": "1.75", "B": "NaN"},
		"shed_total":                    {"A": "2.4", "C": "+Inf"},
	}}
	srv := httptest.NewServer(ps)
	defer srv.Close()

	obs, err := PrometheusSource{BaseURL: srv.URL + "/", Window: 30 * time.Second}.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.OK || len(obs.Tenants) != 2 {
		t.Fatalf("Observe = %+v, want OK with tenants A and B (C only has an Inf sample)", obs)
	}
	a, b := obs.Tenants[0], obs.Tenants[1]
	wantA := model.TenantObservation{Tenant: "A", ArrivalWeightedTokensPerSec: 1200.5, ArrivalRequestsPerSec: 6,
		QueueLength: 3, InFlight: 4, Completed: 20, Shed: 2, HasSLOData: true, P99TTFTSeconds: 1.75}
	if math.Abs(a.SLOAttainment-18.2/19.6) > 1e-12 {
		t.Errorf("A attainment %v, want met/completed = %v", a.SLOAttainment, 18.2/19.6)
	}
	a.SLOAttainment = 0
	if a != wantA {
		t.Errorf("A = %+v, want %+v", a, wantA)
	}
	// B completed nothing and its p99 is NaN: no SLO data.
	wantB := model.TenantObservation{Tenant: "B", ArrivalWeightedTokensPerSec: 40, ArrivalRequestsPerSec: 0.5, InFlight: 1}
	if b != wantB {
		t.Errorf("B = %+v, want %+v", b, wantB)
	}

	if len(ps.queries) != 8 {
		t.Fatalf("ran %d queries, want 8: %q", len(ps.queries), ps.queries)
	}
	for _, want := range []string{
		"sum by (tenant)(rate(tenantscaler_gateway_arrival_weighted_tokens_total[30s]))",
		"sum by (tenant)(rate(tenantscaler_gateway_arrivals_total[30s]))",
		"sum by (tenant)(tenantscaler_gateway_queue_length)",
		"sum by (tenant)(tenantscaler_gateway_inflight)",
		`sum by (tenant)(increase(tenantscaler_gateway_completed_total{slo="met"}[30s]))`,
		"sum by (tenant)(increase(tenantscaler_gateway_completed_total[30s]))",
		"histogram_quantile(0.99, sum by (tenant, le)(rate(tenantscaler_gateway_ttft_seconds_bucket[30s])))",
		"sum by (tenant)(increase(tenantscaler_gateway_shed_total[30s]))",
	} {
		found := false
		for _, q := range ps.queries {
			found = found || q == want
		}
		if !found {
			t.Errorf("query %q was not run", want)
		}
	}
}

// The gateway exports its queue gauge at 0 for every tenant, so a
// Prometheus without it is not scraping the gateway: that must fail the
// observation (fail-static, R4), not read as zero demand.
func TestPrometheusSourceDefaultWindowAndEmpty(t *testing.T) {
	ps := &promServer{}
	srv := httptest.NewServer(ps)
	defer srv.Close()
	obs, err := PrometheusSource{BaseURL: srv.URL}.Observe(context.Background())
	if err == nil || obs.OK || !strings.Contains(err.Error(), "is Prometheus scraping the gateway?") {
		t.Errorf("Observe on an empty Prometheus = %+v, %v; want an error about the missing gateway series", obs, err)
	}
	if !strings.Contains(ps.queries[0], "[60s]") {
		t.Errorf("query %q does not use the default 60s window", ps.queries[0])
	}
}

// Right after the gateway starts only its gauges have samples (rate() and
// increase() need two scrapes): that is a valid, idle observation.
func TestPrometheusSourceGaugesOnly(t *testing.T) {
	ps := &promServer{results: map[string]map[string]string{
		"queue_length": {"A": "0", "B": "2"},
		"inflight":     {"A": "0", "B": "1"},
	}}
	srv := httptest.NewServer(ps)
	defer srv.Close()
	obs, err := PrometheusSource{BaseURL: srv.URL}.Observe(context.Background())
	want := []model.TenantObservation{{Tenant: "A"}, {Tenant: "B", QueueLength: 2, InFlight: 1}}
	if err != nil || !obs.OK || len(obs.Tenants) != 2 || obs.Tenants[0] != want[0] || obs.Tenants[1] != want[1] {
		t.Errorf("Observe = %+v, %v; want OK with %+v", obs, err, want)
	}
}

func TestPrometheusSourceErrors(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		code   int
		want   string
	}{
		{"api error", `{"status":"error","errorType":"bad_data","error":"parse error"}`, http.StatusOK, "bad_data: parse error"},
		{"http error", `{"status":"error","errorType":"bad_data","error":"invalid parameter"}`, http.StatusBadRequest, "400"},
		{"matrix result", `{"status":"success","data":{"resultType":"matrix","result":[]}}`, http.StatusOK, `"matrix"`},
		{"non-numeric value", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"tenant":"A"},"value":[1,"abc"]}]}}`, http.StatusOK, "abc"},
		{"value not a string", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"tenant":"A"},"value":[1,2]}]}}`, http.StatusOK, "not a string"},
		{"not json", `<html>`, http.StatusOK, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				fmt.Fprint(w, tt.answer)
			}))
			defer srv.Close()
			_, err := PrometheusSource{BaseURL: srv.URL}.Observe(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Observe error = %v, want one containing %q", err, tt.want)
			}
		})
	}
	if _, err := (PrometheusSource{}).Observe(context.Background()); err == nil {
		t.Error("Observe without a base URL succeeded")
	}
}

func TestPromRange(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{15 * time.Second, "15s"},
		{90 * time.Second, "90s"},
		{1500 * time.Millisecond, "1500ms"},
		{time.Microsecond, "1ms"},
	}
	for _, tt := range tests {
		if got := promRange(tt.in); got != tt.want {
			t.Errorf("promRange(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
