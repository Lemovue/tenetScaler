package controller

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/model"
)

// statsServer serves body at GET /stats and records the window parameters.
func statsServer(t *testing.T, body string, windows *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		if windows != nil {
			*windows = append(*windows, r.URL.Query().Get("window"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// gatewayStatsJSON is a full gateway.Stats answer, including fields the
// controller ignores.
const gatewayStatsJSON = `{
 "time": "2026-01-15T12:00:00Z", "mode": "fair", "policyVersion": 7, "policyLatencyMs": 1.5,
 "endpoints": 4, "slots": 16, "inFlight": 5, "queued": 3,
 "tenants": [
  {"tenant": "B", "arrivalWeightedTokensPerSec": 100, "arrivalRequestsPerSec": 1, "queueLength": 0,
   "inFlight": 1, "sloAttainment": 0, "hasSloData": false, "p99TtftSeconds": 0, "completed": 0, "shed": 0},
  {"tenant": "A", "arrivalWeightedTokensPerSec": 900.5, "arrivalRequestsPerSec": 4.5, "queueLength": 3,
   "inFlight": 4, "sloAttainment": 0.9, "hasSloData": true, "p99TtftSeconds": 2.5, "completed": 30, "shed": 2}
 ],
 "scheduler": [{"tenant": "A", "virtualTokens": 12.5}]
}`

func TestGatewaySourceSingle(t *testing.T) {
	var windows []string
	srv := statsServer(t, gatewayStatsJSON, &windows)
	src := GatewaySource{URLs: []string{srv.URL + "/"}, Window: 15 * time.Second}
	obs, err := src.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	want := []model.TenantObservation{
		{Tenant: "A", ArrivalWeightedTokensPerSec: 900.5, ArrivalRequestsPerSec: 4.5, QueueLength: 3, InFlight: 4,
			SLOAttainment: 0.9, HasSLOData: true, P99TTFTSeconds: 2.5, Completed: 30, Shed: 2},
		{Tenant: "B", ArrivalWeightedTokensPerSec: 100, ArrivalRequestsPerSec: 1, InFlight: 1},
	}
	if !obs.OK || !reflect.DeepEqual(obs.Tenants, want) {
		t.Errorf("Observe = %+v, want OK and %+v", obs, want)
	}
	if !reflect.DeepEqual(windows, []string{"15s"}) {
		t.Errorf("window parameters %v, want [15s]", windows)
	}
}

func TestGatewaySourceMergesGateways(t *testing.T) {
	second := `{"tenants": [
	  {"tenant": "A", "arrivalWeightedTokensPerSec": 99.5, "arrivalRequestsPerSec": 0.5, "queueLength": 1,
	   "inFlight": 2, "sloAttainment": 0.5, "hasSloData": true, "p99TtftSeconds": 4, "completed": 10, "shed": 1},
	  {"tenant": "C", "arrivalWeightedTokensPerSec": 10, "queueLength": 2}
	]}`
	var w1, w2 []string
	g1 := statsServer(t, gatewayStatsJSON, &w1)
	g2 := statsServer(t, second, &w2)
	obs, err := GatewaySource{URLs: []string{g1.URL, g2.URL}}.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(w1) != 1 || w1[0] != "" || len(w2) != 1 {
		t.Errorf("window parameters %v %v, want one request each and no window when Window is 0", w1, w2)
	}
	byName := map[string]model.TenantObservation{}
	for _, o := range obs.Tenants {
		byName[o.Tenant] = o
	}
	a := byName["A"]
	// Attainment weighted by completed: (0.9*30 + 0.5*10) / 40 = 0.8.
	if a.ArrivalWeightedTokensPerSec != 1000 || a.ArrivalRequestsPerSec != 5 || a.QueueLength != 4 ||
		a.InFlight != 6 || a.Completed != 40 || a.Shed != 3 || a.P99TTFTSeconds != 4 ||
		!a.HasSLOData || math.Abs(a.SLOAttainment-0.8) > 1e-12 {
		t.Errorf("merged A = %+v", a)
	}
	if c := byName["C"]; c.QueueLength != 2 || c.HasSLOData {
		t.Errorf("merged C = %+v, want C from the second gateway only", c)
	}
	if names := []string{obs.Tenants[0].Tenant, obs.Tenants[1].Tenant, obs.Tenants[2].Tenant}; !reflect.DeepEqual(names, []string{"A", "B", "C"}) {
		t.Errorf("tenant order %v, want sorted", names)
	}
}

func TestGatewaySourceErrors(t *testing.T) {
	ok := statsServer(t, gatewayStatsJSON, nil)
	broken := statsServer(t, `{"tenants": [`, nil)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	tests := []struct {
		name string
		urls []string
		want string
	}{
		{"no urls", nil, "no gateway URLs"},
		{"one gateway down fails the whole observation", []string{ok.URL, failing.URL}, "503"},
		{"bad json", []string{broken.URL}, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GatewaySource{URLs: tt.urls}.Observe(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Observe error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestMergeTenants(t *testing.T) {
	tests := []struct {
		name   string
		groups [][]model.TenantObservation
		want   model.TenantObservation
	}{
		{
			name: "no SLO data anywhere",
			groups: [][]model.TenantObservation{
				{{Tenant: "A", QueueLength: 2}}, {{Tenant: "A", QueueLength: 3}},
			},
			want: model.TenantObservation{Tenant: "A", QueueLength: 5},
		},
		{
			name: "a gateway without SLO data does not dilute attainment",
			groups: [][]model.TenantObservation{
				{{Tenant: "A", HasSLOData: true, SLOAttainment: 0.6, P99TTFTSeconds: 1, Completed: 5}},
				{{Tenant: "A", ArrivalRequestsPerSec: 2}},
			},
			want: model.TenantObservation{Tenant: "A", ArrivalRequestsPerSec: 2, HasSLOData: true, SLOAttainment: 0.6, P99TTFTSeconds: 1, Completed: 5},
		},
		{
			name: "zero completed falls back to the plain mean",
			groups: [][]model.TenantObservation{
				{{Tenant: "A", HasSLOData: true, SLOAttainment: 1}},
				{{Tenant: "A", HasSLOData: true, SLOAttainment: 0.5, P99TTFTSeconds: 3}},
			},
			want: model.TenantObservation{Tenant: "A", HasSLOData: true, SLOAttainment: 0.75, P99TTFTSeconds: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeTenants(tt.groups...)
			if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
				t.Errorf("MergeTenants = %+v, want [%+v]", got, tt.want)
			}
		})
	}
	if got := MergeTenants(); got == nil || len(got) != 0 {
		t.Errorf("MergeTenants() = %#v, want an empty non-nil slice", got)
	}
}

// The stats JSON must round-trip through the shared model type: a gateway
// built on model.TenantObservation produces exactly what the source reads.
func TestGatewayStatsDecodesModelJSON(t *testing.T) {
	in := model.TenantObservation{Tenant: "x", ArrivalWeightedTokensPerSec: 1, ArrivalRequestsPerSec: 2, QueueLength: 3,
		InFlight: 4, SLOAttainment: 0.5, HasSLOData: true, P99TTFTSeconds: 6, Completed: 7, Shed: 8}
	data, err := json.Marshal(map[string]any{"tenants": []model.TenantObservation{in}})
	if err != nil {
		t.Fatal(err)
	}
	srv := statsServer(t, string(data), nil)
	obs, err := GatewaySource{URLs: []string{srv.URL}}.Observe(context.Background())
	if err != nil || len(obs.Tenants) != 1 || obs.Tenants[0] != in {
		t.Errorf("Observe = %+v, %v; want [%+v]", obs.Tenants, err, in)
	}
}
